package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSendWebexPostsMarkdownPayload(t *testing.T) {
	t.Parallel()
	var gotMethod, gotContentType, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
	}))
	defer server.Close()

	err := sendWebex(context.Background(), server.Client(), server.URL, "**test** message")
	if err != nil {
		t.Fatalf("sendWebex: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(gotBody), &payload); err != nil {
		t.Fatalf("body %q is not JSON: %v", gotBody, err)
	}
	if len(payload) != 1 || payload["markdown"] != "**test** message" {
		t.Errorf(`payload = %v, want exactly {"markdown": "**test** message"}`, payload)
	}
}

func TestSendWebexNon2xxIsErrorWithoutURL(t *testing.T) {
	t.Parallel()
	const secretPath = "/v1/webhooks/incoming/secret-token-abc"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "invalid webhook", http.StatusBadRequest)
	}))
	defer server.Close()

	err := sendWebex(context.Background(), server.Client(), server.URL+secretPath, "msg")
	if err == nil {
		t.Fatal("sendWebex succeeded, want error on HTTP 400")
	}
	if !strings.Contains(err.Error(), "HTTP 400") {
		t.Errorf("error = %q, want the HTTP status", err)
	}
	if strings.Contains(err.Error(), "secret-token-abc") || strings.Contains(err.Error(), server.URL) {
		t.Errorf("error = %q leaks the webhook URL", err)
	}
}

func TestSendWebexTransportErrorWithoutURL(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := server.URL + "/v1/webhooks/incoming/secret-token-xyz"
	server.Close() // force connection refused

	err := sendWebex(context.Background(), &http.Client{Timeout: time.Second}, url, "msg")
	if err == nil {
		t.Fatal("sendWebex succeeded, want transport error")
	}
	if !strings.Contains(err.Error(), "request failed") {
		t.Errorf("error = %q, want transport failure", err)
	}
	if strings.Contains(err.Error(), "secret-token-xyz") {
		t.Errorf("error = %q leaks the webhook path", err)
	}
}

func TestBuildDownMessage(t *testing.T) {
	t.Parallel()
	svc := serviceCheck{name: "OSM API", key: "osm_api"}
	result := checkResult{healthy: false, detail: "api=readonly database=online gpx=online", attempts: 3}
	now := time.Date(2026, 7, 21, 12, 5, 0, 0, time.UTC)

	msg := buildDownMessage(svc, result, now, "monitor-host01")
	for _, want := range []string{
		"🔴", "**DOWN — OSM API**",
		"api=readonly database=online gpx=online",
		"2026-07-21 12:05:00 UTC", "3 attempt(s)", "monitor-host01",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("down message %q missing %q", msg, want)
		}
	}
}

func TestBuildRecoveredMessage(t *testing.T) {
	t.Parallel()
	svc := serviceCheck{name: "Nominatim", key: "nominatim"}
	result := checkResult{healthy: true, detail: "OK (data updated 2026-07-21T11:58:02+00:00)", attempts: 1}
	downSince := time.Date(2026, 7, 21, 11, 45, 0, 0, time.UTC)
	now := time.Date(2026, 7, 21, 12, 5, 0, 0, time.UTC)

	msg := buildRecoveredMessage(svc, result, downSince, now, "monitor-host01")
	for _, want := range []string{
		"🟢", "**RECOVERED — Nominatim**",
		"OK (data updated 2026-07-21T11:58:02+00:00)",
		"2026-07-21 11:45:00 UTC", "outage duration: 20m0s",
		"2026-07-21 12:05:00 UTC", "monitor-host01",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("recovered message %q missing %q", msg, want)
		}
	}
}

func TestAlertMessagesNeutralizeDetailMarkdown(t *testing.T) {
	t.Parallel()
	svc := serviceCheck{name: "OSM API", key: "osm_api"}
	result := checkResult{
		detail:   "see [status page](https://evil.example/phish) `now`",
		attempts: 1,
	}
	now := time.Date(2026, 7, 22, 9, 2, 0, 0, time.UTC)

	down := buildDownMessage(svc, result, now, "host")
	recovered := buildRecoveredMessage(svc, result, now.Add(-time.Hour), now, "host")
	// The remote-derived detail must be wrapped in a code span, with input
	// backticks replaced, so markdown (e.g. the link) renders as literal text.
	const wantSpan = "`see [status page](https://evil.example/phish) 'now'`"
	for _, msg := range []string{down, recovered} {
		if !strings.Contains(msg, wantSpan) {
			t.Errorf("message %q missing code-span-wrapped detail %q", msg, wantSpan)
		}
	}
}

func TestBuildHeartbeatMessage(t *testing.T) {
	t.Parallel()
	statuses := []serviceStatus{
		{name: "OSM API", healthy: true},
		{name: "Nominatim", healthy: true},
		{name: "OpenRouteService", healthy: false},
	}
	now := time.Date(2026, 7, 22, 9, 2, 0, 0, time.UTC)

	msg := buildHeartbeatMessage(statuses, now, "monitor-host01")
	for _, want := range []string{
		"💓", "**HEARTBEAT — osm-monitor**",
		"OSM API ✅", "Nominatim ✅", "OpenRouteService ❌",
		"2026-07-22 09:02:00 UTC", "monitor-host01", version,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("heartbeat message %q missing %q", msg, want)
		}
	}
}

func TestMonitorHost(t *testing.T) {
	t.Parallel()
	if monitorHost() == "" {
		t.Error("monitorHost() returned empty string, want hostname or fallback")
	}
}
