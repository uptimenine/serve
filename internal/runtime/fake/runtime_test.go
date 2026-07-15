package fake_test

import (
	"context"
	"testing"
	"time"

	"github.com/uptimenine/serve/internal/runtime"
	"github.com/uptimenine/serve/internal/runtime/fake"
)

func TestFakeRuntimeRecordsStartedContainersAsRunning(t *testing.T) {
	rt := fake.NewRuntime()
	id, err := rt.CreateContainer(context.Background(), runtime.ContainerSpec{
		Name:   "my-app-web-production-abc123-r1",
		Image:  "ghcr.io/acme/my-app:abc123",
		Labels: map[string]string{"serve.managed": "true", "serve.role": "web"},
	})
	if err != nil {
		t.Fatalf("create container: %v", err)
	}

	if err := rt.StartContainer(context.Background(), id); err != nil {
		t.Fatalf("start container: %v", err)
	}

	state, err := rt.InspectContainer(context.Background(), id)
	if err != nil {
		t.Fatalf("inspect container: %v", err)
	}
	if !state.Running {
		t.Fatalf("expected container to be running, got %#v", state)
	}
	if state.Name != "my-app-web-production-abc123-r1" || state.Image != "ghcr.io/acme/my-app:abc123" {
		t.Fatalf("unexpected inspected state: %#v", state)
	}
}

func TestFakeRuntimeListsContainersByLabels(t *testing.T) {
	rt := fake.NewRuntime()
	webID, err := rt.CreateContainer(context.Background(), runtime.ContainerSpec{
		Name:   "web",
		Image:  "app:web",
		Labels: map[string]string{"serve.managed": "true", "serve.role": "web"},
	})
	if err != nil {
		t.Fatalf("create web container: %v", err)
	}
	workerID, err := rt.CreateContainer(context.Background(), runtime.ContainerSpec{
		Name:   "worker",
		Image:  "app:worker",
		Labels: map[string]string{"serve.managed": "true", "serve.role": "worker"},
	})
	if err != nil {
		t.Fatalf("create worker container: %v", err)
	}
	if err := rt.StartContainer(context.Background(), webID); err != nil {
		t.Fatalf("start web container: %v", err)
	}
	if err := rt.StartContainer(context.Background(), workerID); err != nil {
		t.Fatalf("start worker container: %v", err)
	}

	containers, err := rt.ListContainers(context.Background(), runtime.ContainerFilters{
		Labels: map[string]string{"serve.managed": "true", "serve.role": "web"},
	})
	if err != nil {
		t.Fatalf("list containers: %v", err)
	}
	if len(containers) != 1 {
		t.Fatalf("expected one matching container, got %#v", containers)
	}
	if containers[0].ID != webID {
		t.Fatalf("expected web container %q, got %#v", webID, containers[0])
	}
}

func TestFakeRuntimeEmitsDieEvents(t *testing.T) {
	rt := fake.NewRuntime()
	events, err := rt.Events(context.Background())
	if err != nil {
		t.Fatalf("subscribe to events: %v", err)
	}
	id, err := rt.CreateContainer(context.Background(), runtime.ContainerSpec{
		Name:   "web",
		Image:  "app:web",
		Labels: map[string]string{"serve.managed": "true", "serve.role": "web"},
	})
	if err != nil {
		t.Fatalf("create container: %v", err)
	}
	if err := rt.StartContainer(context.Background(), id); err != nil {
		t.Fatalf("start container: %v", err)
	}

	rt.Die(id, 137, true)

	select {
	case event := <-events:
		if event.Type != runtime.EventDie {
			t.Fatalf("expected die event, got %#v", event)
		}
		if event.ContainerID != id || event.ExitCode != 137 || !event.OOMKilled {
			t.Fatalf("unexpected die event details: %#v", event)
		}
		if event.Labels["serve.role"] != "web" {
			t.Fatalf("expected event labels, got %#v", event.Labels)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for die event")
	}

	state, err := rt.InspectContainer(context.Background(), id)
	if err != nil {
		t.Fatalf("inspect container after die: %v", err)
	}
	if state.Running {
		t.Fatalf("expected dead container to be stopped, got %#v", state)
	}
}
