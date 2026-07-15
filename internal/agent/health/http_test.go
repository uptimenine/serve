package health_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/uptimenine/serve/internal/agent/health"
)

func TestHTTPCheckerReportsHealthyForSuccessfulResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/up" {
			t.Fatalf("expected /up path, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	status, err := health.NewHTTPChecker(server.Client()).Check(context.Background(), health.Target{Address: server.URL, Path: "/up"})

	if err != nil {
		t.Fatalf("check health: %v", err)
	}
	if status != health.Healthy {
		t.Fatalf("expected healthy, got %s", status)
	}
}

func TestHTTPCheckerReportsUnhealthyForFailedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	status, err := health.NewHTTPChecker(server.Client()).Check(context.Background(), health.Target{Address: server.URL, Path: "/up"})

	if err != nil {
		t.Fatalf("check health: %v", err)
	}
	if status != health.Unhealthy {
		t.Fatalf("expected unhealthy, got %s", status)
	}
}
