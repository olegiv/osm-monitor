package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const alertTimeFormat = "2006-01-02 15:04:05 MST"

// sendWebex posts a markdown message to a Webex incoming webhook. The webhook
// URL is a credential — errors must never contain it, so transport failures
// surface only the underlying cause and HTTP failures only the status code.
func sendWebex(ctx context.Context, client *http.Client, webhookURL, markdown string) error {
	payload, err := json.Marshal(map[string]string{"markdown": markdown})
	if err != nil {
		return fmt.Errorf("webex: encoding payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(payload)) // #nosec G704 -- webhook URL is operator config validated to http(s) at startup
	if err != nil {
		return errors.New("webex: building request failed")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req) // #nosec G704 -- webhook URL is operator config validated to http(s) at startup
	if err != nil {
		if ue, ok := errors.AsType[*url.Error](err); ok {
			return fmt.Errorf("webex: request failed: %w", ue.Err)
		}
		return errors.New("webex: request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("webex: webhook returned HTTP %d", resp.StatusCode)
	}
	return nil
}

func buildDownMessage(svc serviceCheck, result checkResult, now time.Time, host string) string {
	return fmt.Sprintf(
		"🔴 **DOWN — %s**\n- **Reason:** %s\n- **Checked:** %s (%d attempt(s))\n- **Monitor:** %s",
		svc.name, result.detail, now.UTC().Format(alertTimeFormat), result.attempts, host)
}

func buildRecoveredMessage(svc serviceCheck, result checkResult, downSince, now time.Time, host string) string {
	outage := now.Sub(downSince).Round(time.Second)
	return fmt.Sprintf(
		"🟢 **RECOVERED — %s**\n- **Status:** %s\n- **Down since:** %s — **outage duration: %s**\n- **Recovered:** %s\n- **Monitor:** %s",
		svc.name, result.detail, downSince.UTC().Format(alertTimeFormat), outage,
		now.UTC().Format(alertTimeFormat), host)
}

// serviceStatus is the per-service input to the heartbeat summary.
type serviceStatus struct {
	name    string
	healthy bool
}

// buildHeartbeatMessage summarizes one full cycle. It is sent unconditionally
// on --heartbeat runs, proving the cron pipeline, the checks, and the webhook
// are all alive even when no transition happened. Down services show only ❌
// here — the details live in the DOWN alert.
func buildHeartbeatMessage(statuses []serviceStatus, now time.Time, host string) string {
	parts := make([]string, len(statuses))
	for i, st := range statuses {
		mark := "✅"
		if !st.healthy {
			mark = "❌"
		}
		parts[i] = st.name + " " + mark
	}
	return fmt.Sprintf(
		"💓 **HEARTBEAT — osm-monitor**\n- **Services:** %s\n- **Checked:** %s\n- **Monitor:** %s (osm-monitor %s)",
		strings.Join(parts, " · "), now.UTC().Format(alertTimeFormat), host, version)
}

func monitorHost() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		return "unknown"
	}
	return host
}
