package events

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/uptimenine/serve/internal/agent/healing"
)

// JSONSink writes one JSON line per lifecycle event, matching the runtime
// event log format shared by the agent event stream and persisted logs.
type JSONSink struct {
	mu     sync.Mutex
	writer io.Writer
	now    func() time.Time
}

func NewJSONSink(writer io.Writer) *JSONSink {
	return &JSONSink{writer: writer, now: time.Now}
}

type eventLine struct {
	Time        string `json:"time"`
	Level       string `json:"level"`
	Component   string `json:"component"`
	Event       string `json:"event"`
	Service     string `json:"service,omitempty"`
	Destination string `json:"destination,omitempty"`
	Role        string `json:"role,omitempty"`
	Container   string `json:"container,omitempty"`
	Version     string `json:"version,omitempty"`
	ExitCode    int    `json:"exit_code"`
	OOMKilled   bool   `json:"oom_killed"`
	Attempt     int    `json:"attempt,omitempty"`
	Actor       string `json:"actor,omitempty"`
}

func (s *JSONSink) Emit(ctx context.Context, event healing.LifecycleEvent) error {
	_ = ctx

	line := eventLine{
		Time:        s.now().UTC().Format(time.RFC3339),
		Level:       level(event.Name),
		Component:   "agent",
		Event:       event.Name,
		Service:     event.Service,
		Destination: event.Destination,
		Role:        event.Role,
		Container:   event.Container,
		Version:     event.Version,
		ExitCode:    event.ExitCode,
		OOMKilled:   event.OOMKilled,
		Attempt:     event.Attempt,
		Actor:       event.Actor,
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	return json.NewEncoder(s.writer).Encode(line)
}

func level(event string) string {
	switch event {
	case "restart_loop_detected":
		return "error"
	case "container_exited", "container_restarted", "container_recreated", "container_unhealthy", "container_oom_killed":
		return "warn"
	default:
		return "info"
	}
}
