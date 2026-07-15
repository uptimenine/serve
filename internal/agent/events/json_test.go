package events_test

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/uptimenine/serve/internal/agent/events"
	"github.com/uptimenine/serve/internal/agent/healing"
)

func TestJSONSinkWritesOneStructuredLinePerEvent(t *testing.T) {
	var buffer bytes.Buffer
	sink := events.NewJSONSink(&buffer)

	err := sink.Emit(context.Background(), healing.LifecycleEvent{
		Name:        "container_restarted",
		Service:     "my-app",
		Destination: "production",
		Role:        "web",
		Version:     "abc123",
		Container:   "my-app-web-production-abc123-r1",
		ExitCode:    137,
		OOMKilled:   true,
		Attempt:     2,
		Actor:       "serve-agent",
	})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(buffer.Bytes(), &decoded); err != nil {
		t.Fatalf("event line is not valid JSON: %v\n%s", err, buffer.String())
	}

	expectations := map[string]any{
		"event":       "container_restarted",
		"level":       "warn",
		"component":   "agent",
		"service":     "my-app",
		"destination": "production",
		"role":        "web",
		"version":     "abc123",
		"container":   "my-app-web-production-abc123-r1",
		"exit_code":   float64(137),
		"oom_killed":  true,
		"attempt":     float64(2),
		"actor":       "serve-agent",
	}
	for key, want := range expectations {
		if decoded[key] != want {
			t.Fatalf("field %q = %v, want %v (line: %s)", key, decoded[key], want, buffer.String())
		}
	}

	timestamp, ok := decoded["time"].(string)
	if !ok || timestamp == "" {
		t.Fatalf("event line missing time: %s", buffer.String())
	}
	if _, err := time.Parse(time.RFC3339, timestamp); err != nil {
		t.Fatalf("time %q is not RFC3339: %v", timestamp, err)
	}
}

func TestJSONSinkLevels(t *testing.T) {
	cases := map[string]string{
		"container_exited":      "warn",
		"container_restarted":   "warn",
		"restart_loop_detected": "error",
		"container_started":     "info",
	}
	for event, wantLevel := range cases {
		var buffer bytes.Buffer
		sink := events.NewJSONSink(&buffer)
		if err := sink.Emit(context.Background(), healing.LifecycleEvent{Name: event}); err != nil {
			t.Fatalf("Emit %s: %v", event, err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(buffer.Bytes(), &decoded); err != nil {
			t.Fatalf("decode %s: %v", event, err)
		}
		if decoded["level"] != wantLevel {
			t.Fatalf("event %s level = %v, want %s", event, decoded["level"], wantLevel)
		}
	}
}
