package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestStateSaveLoadRoundTrip(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.json")
	want := newMonitorState()
	want.Services["osm_api"] = serviceState{
		Healthy:     true,
		Detail:      "api=online database=online gpx=online",
		Since:       time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC),
		LastChecked: time.Date(2026, 7, 21, 12, 5, 0, 0, time.UTC),
	}
	want.Services["nominatim"] = serviceState{
		Healthy:     false,
		Detail:      "HTTP 500: status=700 message=Database connection failed",
		Since:       time.Date(2026, 7, 21, 11, 45, 0, 0, time.UTC),
		LastChecked: time.Date(2026, 7, 21, 12, 5, 0, 0, time.UTC),
	}

	if err := saveState(path, want); err != nil {
		t.Fatalf("saveState: %v", err)
	}
	got := loadState(path)
	if got.SchemaVersion != stateSchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", got.SchemaVersion, stateSchemaVersion)
	}
	if len(got.Services) != 2 {
		t.Fatalf("got %d services, want 2", len(got.Services))
	}
	for key, wantSvc := range want.Services {
		gotSvc := got.Services[key]
		if gotSvc.Healthy != wantSvc.Healthy || gotSvc.Detail != wantSvc.Detail ||
			!gotSvc.Since.Equal(wantSvc.Since) || !gotSvc.LastChecked.Equal(wantSvc.LastChecked) {
			t.Errorf("%s = %+v, want %+v", key, gotSvc, wantSvc)
		}
	}
}

func TestSaveStateAtomicAndPrivate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := saveState(path, newMonitorState()); err != nil {
		t.Fatalf("saveState: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "state.json" {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("directory contains %v, want only state.json (no leftover temp files)", names)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("state file permissions = %o, want 600", perm)
		}
	}
}

func TestLoadStateMissingFile(t *testing.T) {
	t.Parallel()
	st := loadState(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if st == nil || st.Services == nil || len(st.Services) != 0 {
		t.Errorf("loadState on missing file = %+v, want empty state", st)
	}
}

func TestLoadStateCorruptFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0o600); err != nil {
		t.Fatal(err)
	}
	st := loadState(path)
	if st == nil || len(st.Services) != 0 {
		t.Errorf("loadState on corrupt file = %+v, want empty state (never fatal)", st)
	}
}

func TestLoadStateUnknownSchema(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.json")
	content := `{"schema_version": 99, "services": {"osm_api": {"healthy": true}}}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	st := loadState(path)
	if len(st.Services) != 0 {
		t.Errorf("loadState with schema 99 kept %d services, want empty state", len(st.Services))
	}
}

func TestLoadStateNormalizesNilServices(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"schema_version": 1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st := loadState(path)
	if st.Services == nil {
		t.Error("loadState left Services nil, want initialized map")
	}
}

func TestComputeTransition(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 21, 12, 5, 0, 0, time.UTC)
	earlier := time.Date(2026, 7, 21, 11, 45, 0, 0, time.UTC)
	upPrev := &serviceState{Healthy: true, Detail: "old detail", Since: earlier}
	downPrev := &serviceState{Healthy: false, Detail: "old failure", Since: earlier}

	cases := []struct {
		name          string
		prev          *serviceState
		cur           checkResult
		wantKind      transitionKind
		wantSince     time.Time
		wantDownSince time.Time
	}{
		{"first run healthy is silent", nil,
			checkResult{healthy: true, detail: "OK"}, transitionNone, now, time.Time{}},
		{"first run unhealthy alerts", nil,
			checkResult{healthy: false, detail: "HTTP 503"}, transitionDown, now, time.Time{}},
		{"up to down alerts", upPrev,
			checkResult{healthy: false, detail: "HTTP 503"}, transitionDown, now, time.Time{}},
		{"down to up recovers with outage start", downPrev,
			checkResult{healthy: true, detail: "OK"}, transitionRecovered, now, earlier},
		{"steady up keeps since", upPrev,
			checkResult{healthy: true, detail: "new detail"}, transitionNone, earlier, time.Time{}},
		{"steady down never re-alerts on detail drift", downPrev,
			checkResult{healthy: false, detail: "different failure"}, transitionNone, earlier, time.Time{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := computeTransition(tc.prev, tc.cur, now)
			if tr.kind != tc.wantKind {
				t.Errorf("kind = %d, want %d", tr.kind, tc.wantKind)
			}
			if !tr.next.Since.Equal(tc.wantSince) {
				t.Errorf("next.Since = %s, want %s", tr.next.Since, tc.wantSince)
			}
			if tc.wantKind == transitionRecovered && !tr.downSince.Equal(tc.wantDownSince) {
				t.Errorf("downSince = %s, want %s", tr.downSince, tc.wantDownSince)
			}
			if tr.next.Healthy != tc.cur.healthy || tr.next.Detail != tc.cur.detail {
				t.Errorf("next = %+v, want current health %v and detail %q",
					tr.next, tc.cur.healthy, tc.cur.detail)
			}
			if !tr.next.LastChecked.Equal(now) {
				t.Errorf("next.LastChecked = %s, want %s", tr.next.LastChecked, now)
			}
		})
	}
}
