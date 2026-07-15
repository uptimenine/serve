package state_test

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	agentstate "github.com/uptimenine/serve/internal/agent/state"
	"github.com/uptimenine/serve/internal/planner"
)

func TestStoreRejectsUnsafeStateIdentity(t *testing.T) {
	store := agentstate.NewStore(t.TempDir())
	desired := desiredState("abc123")
	desired.Service = "../escape"

	if err := store.SaveDesired(desired); err == nil {
		t.Fatal("expected unsafe service to be rejected")
	}
	if _, err := store.LoadDesired("my-app", "../escape"); err == nil {
		t.Fatal("expected unsafe destination to be rejected")
	}
}

func TestStoreAtomicallySavesAndLoadsDesiredState(t *testing.T) {
	store := agentstate.NewStore(t.TempDir())
	desired := desiredState("abc123")

	if err := store.SaveDesired(desired); err != nil {
		t.Fatalf("save desired state: %v", err)
	}

	loaded, err := store.LoadDesired("my-app", "production")
	if err != nil {
		t.Fatalf("load desired state: %v", err)
	}
	if !reflect.DeepEqual(loaded, desired) {
		t.Fatalf("expected loaded desired state %#v, got %#v", desired, loaded)
	}

	entries, err := os.ReadDir(filepath.Join(store.Dir()))
	if err != nil {
		t.Fatalf("read state dir: %v", err)
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".tmp" {
			t.Fatalf("atomic save left temporary file behind: %s", entry.Name())
		}
	}
}

func TestStorePreservesLastGoodWhenDesiredStateChanges(t *testing.T) {
	store := agentstate.NewStore(t.TempDir())
	lastGood := desiredState("abc123")
	candidate := desiredState("def456")

	if err := store.SaveLastGood(lastGood); err != nil {
		t.Fatalf("save last-good state: %v", err)
	}
	if err := store.SaveDesired(candidate); err != nil {
		t.Fatalf("save candidate desired state: %v", err)
	}

	loaded, err := store.LoadLastGood("my-app", "production")
	if err != nil {
		t.Fatalf("load last-good state: %v", err)
	}
	if !reflect.DeepEqual(loaded, lastGood) {
		t.Fatalf("expected last-good state to remain %#v, got %#v", lastGood, loaded)
	}
}

func TestStoreReturnsEmptyActualStateOnFirstBoot(t *testing.T) {
	store := agentstate.NewStore(t.TempDir())

	actual, err := store.LoadActual("my-app", "production")

	if err != nil {
		t.Fatalf("load actual state: %v", err)
	}
	if actual.Service != "my-app" || actual.Destination != "production" {
		t.Fatalf("expected actual state identity, got %#v", actual)
	}
	if len(actual.Containers) != 0 {
		t.Fatalf("expected no actual containers on first boot, got %#v", actual.Containers)
	}
}

func TestListDesiredReturnsAllSavedStates(t *testing.T) {
	store := agentstate.NewStore(t.TempDir())

	first := desiredState("abc123")
	second := desiredState("def456")
	second.Service = "other-app"
	second.Containers = nil
	if err := store.SaveDesired(first); err != nil {
		t.Fatalf("save first desired state: %v", err)
	}
	if err := store.SaveDesired(second); err != nil {
		t.Fatalf("save second desired state: %v", err)
	}
	// last-good and actual files must not be listed as desired states.
	if err := store.SaveLastGood(first); err != nil {
		t.Fatalf("save last-good state: %v", err)
	}

	listed, err := store.ListDesired()
	if err != nil {
		t.Fatalf("list desired states: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("expected 2 desired states, got %#v", listed)
	}
	services := map[string]bool{}
	for _, desired := range listed {
		services[desired.Service] = true
	}
	if !services["my-app"] || !services["other-app"] {
		t.Fatalf("expected my-app and other-app, got %#v", services)
	}
}

func TestListDesiredOnMissingDirReturnsEmpty(t *testing.T) {
	store := agentstate.NewStore(filepath.Join(t.TempDir(), "missing"))

	listed, err := store.ListDesired()
	if err != nil {
		t.Fatalf("list desired states: %v", err)
	}
	if len(listed) != 0 {
		t.Fatalf("expected no desired states, got %#v", listed)
	}
}

func desiredState(version string) planner.DesiredState {
	return planner.DesiredState{
		Service:          "my-app",
		Destination:      "production",
		Host:             "app1.example.com",
		Version:          version,
		Network:          "serve",
		RetainContainers: 5,
		Containers: []planner.Container{
			{
				Name:          "my-app-web-production-" + version + "-r1",
				Role:          "web",
				ContainerType: "app",
				Image:         "ghcr.io/acme/my-app:" + version,
				Replica:       1,
				Labels: map[string]string{
					"serve.managed":        "true",
					"serve.service":        "my-app",
					"serve.destination":    "production",
					"serve.role":           "web",
					"serve.version":        version,
					"serve.replica":        "1",
					"serve.container_type": "app",
				},
			},
		},
	}
}
