package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

const stateSchemaVersion = 1

type serviceState struct {
	Healthy     bool      `json:"healthy"`
	Detail      string    `json:"detail"`
	Since       time.Time `json:"since"`
	LastChecked time.Time `json:"last_checked"`
}

type monitorState struct {
	SchemaVersion int                     `json:"schema_version"`
	Services      map[string]serviceState `json:"services"`
}

func newMonitorState() *monitorState {
	return &monitorState{SchemaVersion: stateSchemaVersion, Services: map[string]serviceState{}}
}

// loadState is deliberately tolerant: a missing, unreadable, corrupt, or
// unknown-schema file yields an empty state (first-run semantics) so a broken
// state file can never stop monitoring.
func loadState(path string) *monitorState {
	data, err := os.ReadFile(filepath.Clean(path)) // #nosec G304 G703 -- operator-supplied state path
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("state file unreadable; starting fresh", "path", path, "error", err) // #nosec G706 -- path is operator config; error is OS-generated
		}
		return newMonitorState()
	}
	var st monitorState
	if err := json.Unmarshal(data, &st); err != nil {
		slog.Warn("state file corrupt; starting fresh", "path", path, "error", err) // #nosec G706 -- path is operator config; error is a local JSON decode message
		return newMonitorState()
	}
	if st.SchemaVersion != stateSchemaVersion {
		slog.Warn("state file has unknown schema; starting fresh", // #nosec G706 -- path is operator config; schema is an integer
			"path", path, "schema", st.SchemaVersion)
		return newMonitorState()
	}
	if st.Services == nil {
		st.Services = map[string]serviceState{}
	}
	return &st
}

// saveState writes atomically: temp file in the same directory (os.CreateTemp
// creates it 0600), then rename, so a crash mid-write can never corrupt the
// previous state.
func saveState(path string, st *monitorState) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("state: encoding: %w", err)
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".osm-monitor-state-*.tmp")
	if err != nil {
		return fmt.Errorf("state: creating temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op after successful rename
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("state: writing: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("state: closing temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil { // #nosec G703 -- operator-supplied state path
		return fmt.Errorf("state: replacing %s: %w", path, err)
	}
	return nil
}

type transitionKind int

const (
	transitionNone transitionKind = iota
	transitionDown
	transitionRecovered
)

type transition struct {
	kind      transitionKind
	downSince time.Time // for transitionRecovered: when the outage began
	next      serviceState
}

// computeTransition derives the alert decision and the next stored state for
// one service. prev == nil means the service has no recorded state (first
// run): coming up UP is silent, coming up DOWN alerts. A detail change while
// the status is unchanged updates the record but never re-alerts.
func computeTransition(prev *serviceState, cur checkResult, now time.Time) transition {
	next := serviceState{
		Healthy:     cur.healthy,
		Detail:      cur.detail,
		Since:       now,
		LastChecked: now,
	}
	if prev != nil && prev.Healthy == cur.healthy {
		next.Since = prev.Since // status unchanged — keep when it began
		return transition{kind: transitionNone, next: next}
	}
	if cur.healthy {
		if prev == nil {
			return transition{kind: transitionNone, next: next}
		}
		return transition{kind: transitionRecovered, downSince: prev.Since, next: next}
	}
	return transition{kind: transitionDown, next: next}
}
