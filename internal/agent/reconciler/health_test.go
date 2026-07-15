package reconciler_test

import (
	"context"
	"testing"

	"github.com/uptimenine/serve/internal/agent/health"
	healthfake "github.com/uptimenine/serve/internal/agent/health/fake"
	"github.com/uptimenine/serve/internal/agent/proxy"
	proxyfake "github.com/uptimenine/serve/internal/agent/proxy/fake"
	"github.com/uptimenine/serve/internal/agent/reconciler"
	"github.com/uptimenine/serve/internal/planner"
	"github.com/uptimenine/serve/internal/runtime/fake"
)

func TestReconcileAddsHealthyWebContainerToProxy(t *testing.T) {
	rt := fake.NewRuntime()
	checker := healthfake.NewChecker()
	proxyManager := proxyfake.NewManager()
	desired := desiredState("abc123")
	desired.Containers[0].Proxy = true
	desired.Containers[0].Healthcheck = healthcheck()
	checker.SetStatus("my-app-web-production-abc123-r1", health.Healthy)

	_, err := reconciler.NewWithHealth(rt, checker, proxyManager).Reconcile(context.Background(), desired)

	if err != nil {
		t.Fatalf("reconcile desired state: %v", err)
	}
	targets := proxyManager.Targets()
	if len(targets) != 1 {
		t.Fatalf("expected one proxy target, got %#v", targets)
	}
	expected := proxy.Target{
		Service:       "my-app",
		Role:          "web",
		ContainerName: "my-app-web-production-abc123-r1",
		Port:          3000,
		HealthPath:    "/up",
	}
	if targets[0] != expected {
		t.Fatalf("expected target %#v, got %#v", expected, targets[0])
	}
}

func TestReconcileDoesNotAddUnhealthyWebContainerToProxy(t *testing.T) {
	rt := fake.NewRuntime()
	checker := healthfake.NewChecker()
	proxyManager := proxyfake.NewManager()
	desired := desiredState("abc123")
	desired.Containers[0].Proxy = true
	desired.Containers[0].Healthcheck = healthcheck()
	checker.SetStatus("my-app-web-production-abc123-r1", health.Unhealthy)

	_, err := reconciler.NewWithHealth(rt, checker, proxyManager).Reconcile(context.Background(), desired)

	if err != nil {
		t.Fatalf("reconcile desired state: %v", err)
	}
	if targets := proxyManager.Targets(); len(targets) != 0 {
		t.Fatalf("expected no proxy targets for unhealthy container, got %#v", targets)
	}
}

func TestReconcileRemovesPreviouslyHealthyProxyTargetAfterHealthFailure(t *testing.T) {
	rt := fake.NewRuntime()
	checker := healthfake.NewChecker()
	proxyManager := proxyfake.NewManager()
	desired := desiredState("abc123")
	desired.Containers[0].Proxy = true
	desired.Containers[0].Healthcheck = healthcheck()
	checker.SetStatus("my-app-web-production-abc123-r1", health.Healthy)
	reconcile := reconciler.NewWithHealth(rt, checker, proxyManager)
	if _, err := reconcile.Reconcile(context.Background(), desired); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}
	checker.SetStatus("my-app-web-production-abc123-r1", health.Unhealthy)

	_, err := reconcile.Reconcile(context.Background(), desired)

	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if targets := proxyManager.Targets(); len(targets) != 0 {
		t.Fatalf("expected unhealthy target to be removed, got %#v", targets)
	}
	if proxyManager.RemoveCount() != 1 {
		t.Fatalf("expected one proxy target removal, got %d", proxyManager.RemoveCount())
	}
}

func TestReconcileDoesNotRegisterNonProxyWorkerWithProxy(t *testing.T) {
	rt := fake.NewRuntime()
	checker := healthfake.NewChecker()
	proxyManager := proxyfake.NewManager()
	desired := desiredState("abc123")
	desired.Containers[0].Role = "worker"
	desired.Containers[0].Proxy = false
	checker.SetStatus("my-app-web-production-abc123-r1", health.Healthy)

	_, err := reconciler.NewWithHealth(rt, checker, proxyManager).Reconcile(context.Background(), desired)

	if err != nil {
		t.Fatalf("reconcile desired state: %v", err)
	}
	if targets := proxyManager.Targets(); len(targets) != 0 {
		t.Fatalf("expected worker to skip proxy registration, got %#v", targets)
	}
	if checker.CheckCount() != 0 {
		t.Fatalf("expected worker health not to be checked for proxy registration, got %d checks", checker.CheckCount())
	}
}

func healthcheck() *planner.Healthcheck {
	return &planner.Healthcheck{Type: "http", Path: "/up", Port: 3000}
}
