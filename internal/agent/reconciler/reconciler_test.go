package reconciler_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/uptimenine/serve/internal/agent/reconciler"
	"github.com/uptimenine/serve/internal/planner"
	"github.com/uptimenine/serve/internal/runtime"
	"github.com/uptimenine/serve/internal/runtime/fake"
)

func TestReconcileCreatesNetworkBeforeStartingMissingContainers(t *testing.T) {
	rt := fake.NewRuntime()
	reconcile := reconciler.New(rt)
	desired := desiredState("abc123")

	result, err := reconcile.Reconcile(context.Background(), desired)

	if err != nil {
		t.Fatalf("reconcile desired state: %v", err)
	}
	if result.NetworksCreated != 1 || result.ContainersCreated != 1 || result.ContainersStarted != 1 {
		t.Fatalf("unexpected reconcile result: %#v", result)
	}

	containers, err := rt.ListContainers(context.Background(), runtime.ContainerFilters{Labels: map[string]string{"serve.managed": "true"}})
	if err != nil {
		t.Fatalf("list containers: %v", err)
	}
	if len(containers) != 1 {
		t.Fatalf("expected one container, got %#v", containers)
	}
	if !containers[0].Running || containers[0].Name != "my-app-web-production-abc123-r1" {
		t.Fatalf("expected desired container to be running, got %#v", containers[0])
	}

	operations := rt.Operations()
	if len(operations) < 4 {
		t.Fatalf("expected network/pull/create/start operations, got %#v", operations)
	}
	if operations[0] != "create_network:serve" {
		t.Fatalf("expected network to be created first, got operations %#v", operations)
	}
	if operations[1] != "pull_image:ghcr.io/acme/my-app:abc123" {
		t.Fatalf("expected image pull before container create, got operations %#v", operations)
	}
	if operations[2] != "create_container:my-app-web-production-abc123-r1" {
		t.Fatalf("expected container create after pull, got operations %#v", operations)
	}
}

func TestReconcileCreatesContainerWhenPullFailsButImageIsLocal(t *testing.T) {
	rt := fake.NewRuntime()
	rt.SetPullError(fmt.Errorf("registry unreachable"))
	desired := desiredState("abc123")

	result, err := reconciler.New(rt).Reconcile(context.Background(), desired)

	if err != nil {
		t.Fatalf("reconcile must tolerate pull failure when create succeeds: %v", err)
	}
	if result.ContainersCreated != 1 || result.ContainersStarted != 1 {
		t.Fatalf("unexpected reconcile result: %#v", result)
	}
}

func TestReconcileDoesNotRecreateMatchingRunningContainer(t *testing.T) {
	rt := fake.NewRuntime()
	desired := desiredState("abc123")
	id, err := rt.CreateContainer(context.Background(), runtime.ContainerSpec{
		Name:   "my-app-web-production-abc123-r1",
		Image:  "ghcr.io/acme/my-app:abc123",
		Labels: desired.Containers[0].Labels,
	})
	if err != nil {
		t.Fatalf("seed existing container: %v", err)
	}
	if err := rt.StartContainer(context.Background(), id); err != nil {
		t.Fatalf("start existing container: %v", err)
	}
	rt.ClearOperations()

	result, err := reconciler.New(rt).Reconcile(context.Background(), desired)

	if err != nil {
		t.Fatalf("reconcile desired state: %v", err)
	}
	if result.ContainersCreated != 0 || result.ContainersStarted != 0 {
		t.Fatalf("expected no container changes, got %#v", result)
	}
	containers, err := rt.ListContainers(context.Background(), runtime.ContainerFilters{Labels: map[string]string{"serve.managed": "true"}})
	if err != nil {
		t.Fatalf("list containers: %v", err)
	}
	if len(containers) != 1 || containers[0].ID != id {
		t.Fatalf("expected existing container to remain, got %#v", containers)
	}
}

func TestReconcileRemovesStaleContainerWhenRetentionAllows(t *testing.T) {
	rt := fake.NewRuntime()
	staleLabels := map[string]string{
		"serve.managed":        "true",
		"serve.service":        "my-app",
		"serve.destination":    "production",
		"serve.role":           "web",
		"serve.version":        "old123",
		"serve.replica":        "1",
		"serve.container_type": "app",
	}
	staleID, err := rt.CreateContainer(context.Background(), runtime.ContainerSpec{
		Name:   "my-app-web-production-old123-r1",
		Image:  "ghcr.io/acme/my-app:old123",
		Labels: staleLabels,
	})
	if err != nil {
		t.Fatalf("seed stale container: %v", err)
	}
	if err := rt.StartContainer(context.Background(), staleID); err != nil {
		t.Fatalf("start stale container: %v", err)
	}
	desired := desiredState("abc123")
	desired.RetainContainers = 0
	rt.ClearOperations()

	result, err := reconciler.New(rt).Reconcile(context.Background(), desired)

	if err != nil {
		t.Fatalf("reconcile desired state: %v", err)
	}
	if result.ContainersRemoved != 1 {
		t.Fatalf("expected one stale container removed, got %#v", result)
	}
	containers, err := rt.ListContainers(context.Background(), runtime.ContainerFilters{Labels: map[string]string{"serve.version": "old123"}})
	if err != nil {
		t.Fatalf("list old containers: %v", err)
	}
	if len(containers) != 0 {
		t.Fatalf("expected stale container to be removed, got %#v", containers)
	}
	operations := rt.Operations()
	if len(operations) < 2 || operations[len(operations)-2] != "stop_container:"+string(staleID) || operations[len(operations)-1] != "remove_container:my-app-web-production-old123-r1" {
		t.Fatalf("stale running container was not stopped before removal: %v", operations)
	}
}

func TestEnsureContainerStopsRunningMismatchBeforeReplacement(t *testing.T) {
	rt := fake.NewRuntime()
	desired := desiredState("abc123")
	id, err := rt.CreateContainer(context.Background(), runtime.ContainerSpec{
		Name: desired.Containers[0].Name, Image: "wrong:image", Labels: desired.Containers[0].Labels,
	})
	if err != nil {
		t.Fatalf("seed mismatched container: %v", err)
	}
	if err := rt.StartContainer(context.Background(), id); err != nil {
		t.Fatalf("start mismatched container: %v", err)
	}
	rt.ClearOperations()

	if _, err := reconciler.New(rt).EnsureContainer(context.Background(), desired, desired.Containers[0]); err != nil {
		t.Fatalf("ensure container: %v", err)
	}

	operations := rt.Operations()
	if len(operations) < 2 || operations[0] != "stop_container:"+string(id) || operations[1] != "remove_container:"+desired.Containers[0].Name {
		t.Fatalf("mismatched running container was not stopped before removal: %v", operations)
	}
}

func desiredState(version string) planner.DesiredState {
	name := "my-app-web-production-" + version + "-r1"
	return planner.DesiredState{
		Service:          "my-app",
		Destination:      "production",
		Host:             "app1.example.com",
		Version:          version,
		Network:          "serve",
		RetainContainers: 5,
		Containers: []planner.Container{
			{
				Name:          name,
				Role:          "web",
				ContainerType: "app",
				Image:         "ghcr.io/acme/my-app:" + version,
				Command:       []string{"./server"},
				Ports:         []planner.Port{{Name: "http", ContainerPort: 3000}},
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
