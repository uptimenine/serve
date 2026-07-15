package kamalproxy_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/uptimenine/serve/internal/agent/proxy"
	"github.com/uptimenine/serve/internal/agent/proxy/kamalproxy"
	"github.com/uptimenine/serve/internal/runtime"
	fakeruntime "github.com/uptimenine/serve/internal/runtime/fake"
)

func webTarget() proxy.Target {
	return proxy.Target{
		Service:       "my-app",
		Role:          "web",
		ContainerName: "my-app-web-production-abc123-r1",
		Port:          3000,
		HealthPath:    "/up",
	}
}

func TestAddTargetBootsProxyContainerWhenMissing(t *testing.T) {
	rt := fakeruntime.NewRuntime()
	manager := kamalproxy.New(rt, kamalproxy.Options{Network: "serve"})

	if err := manager.AddTarget(context.Background(), webTarget()); err != nil {
		t.Fatalf("AddTarget: %v", err)
	}

	spec, ok := rt.CreatedSpec("serve-proxy")
	if !ok {
		t.Fatalf("expected proxy container %q to be created, operations: %v", "serve-proxy", rt.Operations())
	}
	if spec.Labels["serve.managed"] != "true" || spec.Labels["serve.container_type"] != "proxy" {
		t.Fatalf("proxy container missing serve labels, got %v", spec.Labels)
	}
	if spec.Network != "serve" {
		t.Fatalf("proxy container network = %q, want %q", spec.Network, "serve")
	}
	assertPublishesPort(t, spec, 80)
	assertPublishesPort(t, spec, 443)

	states, err := rt.ListContainers(context.Background(), runtime.ContainerFilters{Labels: map[string]string{"serve.container_type": "proxy"}})
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}
	if len(states) != 1 || !states[0].Running {
		t.Fatalf("expected one running proxy container, got %+v", states)
	}
}

func TestAddTargetAdoptsExistingRunningProxy(t *testing.T) {
	rt := fakeruntime.NewRuntime()
	startProxyContainer(t, rt)
	rt.ClearOperations()

	manager := kamalproxy.New(rt, kamalproxy.Options{Network: "serve"})
	if err := manager.AddTarget(context.Background(), webTarget()); err != nil {
		t.Fatalf("AddTarget: %v", err)
	}

	for _, op := range rt.Operations() {
		if strings.HasPrefix(op, "create_container:") {
			t.Fatalf("expected existing proxy to be adopted, but a container was created: %v", rt.Operations())
		}
	}
}

func TestAddTargetIssuesKamalProxyDeploy(t *testing.T) {
	rt := fakeruntime.NewRuntime()
	manager := kamalproxy.New(rt, kamalproxy.Options{Network: "serve"})

	if err := manager.AddTarget(context.Background(), webTarget()); err != nil {
		t.Fatalf("AddTarget: %v", err)
	}

	exec := findExec(t, rt, "kamal-proxy deploy")
	want := "exec_container:serve-proxy:kamal-proxy deploy my-app-web --target=my-app-web-production-abc123-r1:3000 --health-check-path=/up --deploy-timeout=30s --drain-timeout=30s"
	if exec != want {
		t.Fatalf("deploy exec\n got: %s\nwant: %s", exec, want)
	}
}

func TestRemoveTargetIssuesKamalProxyRemove(t *testing.T) {
	rt := fakeruntime.NewRuntime()
	manager := kamalproxy.New(rt, kamalproxy.Options{Network: "serve"})

	target := webTarget()
	if err := manager.AddTarget(context.Background(), target); err != nil {
		t.Fatalf("AddTarget: %v", err)
	}
	if err := manager.RemoveTarget(context.Background(), target); err != nil {
		t.Fatalf("RemoveTarget: %v", err)
	}

	exec := findExec(t, rt, "kamal-proxy remove")
	want := "exec_container:serve-proxy:kamal-proxy remove my-app-web"
	if exec != want {
		t.Fatalf("remove exec\n got: %s\nwant: %s", exec, want)
	}
}

func TestRemoveTargetForUnknownServiceIsANoop(t *testing.T) {
	rt := fakeruntime.NewRuntime()
	manager := kamalproxy.New(rt, kamalproxy.Options{Network: "serve"})

	if err := manager.RemoveTarget(context.Background(), webTarget()); err != nil {
		t.Fatalf("RemoveTarget unknown service: %v", err)
	}
	if operations := rt.Operations(); len(operations) != 0 {
		t.Fatalf("unknown removal touched proxy runtime: %v", operations)
	}
}

func TestUnchangedTargetSetSkipsProxyCalls(t *testing.T) {
	rt := fakeruntime.NewRuntime()
	manager := kamalproxy.New(rt, kamalproxy.Options{Network: "serve"})

	target := webTarget()
	if err := manager.AddTarget(context.Background(), target); err != nil {
		t.Fatalf("AddTarget: %v", err)
	}
	if err := manager.AddTarget(context.Background(), target); err != nil {
		t.Fatalf("AddTarget (repeat): %v", err)
	}
	if got := countExecs(rt, "kamal-proxy deploy"); got != 1 {
		t.Fatalf("expected 1 deploy exec for unchanged target, got %d: %v", got, rt.Operations())
	}

	if err := manager.RemoveTarget(context.Background(), target); err != nil {
		t.Fatalf("RemoveTarget: %v", err)
	}
	if err := manager.RemoveTarget(context.Background(), target); err != nil {
		t.Fatalf("RemoveTarget (repeat): %v", err)
	}
	if got := countExecs(rt, "kamal-proxy remove"); got != 1 {
		t.Fatalf("expected 1 remove exec for unchanged target, got %d: %v", got, rt.Operations())
	}
}

func TestChangedTargetIssuesNewDeploy(t *testing.T) {
	rt := fakeruntime.NewRuntime()
	manager := kamalproxy.New(rt, kamalproxy.Options{Network: "serve"})

	target := webTarget()
	if err := manager.AddTarget(context.Background(), target); err != nil {
		t.Fatalf("AddTarget: %v", err)
	}
	target.ContainerName = "my-app-web-production-def456-r1"
	if err := manager.AddTarget(context.Background(), target); err != nil {
		t.Fatalf("AddTarget (new version): %v", err)
	}

	if got := countExecs(rt, "kamal-proxy deploy"); got != 2 {
		t.Fatalf("expected 2 deploy execs after target change, got %d: %v", got, rt.Operations())
	}
}

func TestAddSecondReplicaDeploysFullTargetSet(t *testing.T) {
	rt := fakeruntime.NewRuntime()
	manager := kamalproxy.New(rt, kamalproxy.Options{Network: "serve"})

	replica1 := webTarget()
	replica2 := webTarget()
	replica2.ContainerName = "my-app-web-production-abc123-r2"

	if err := manager.AddTarget(context.Background(), replica1); err != nil {
		t.Fatalf("AddTarget r1: %v", err)
	}
	if err := manager.AddTarget(context.Background(), replica2); err != nil {
		t.Fatalf("AddTarget r2: %v", err)
	}

	last := lastExec(t, rt, "kamal-proxy deploy")
	for _, address := range []string{"my-app-web-production-abc123-r1:3000", "my-app-web-production-abc123-r2:3000"} {
		if !strings.Contains(last, "--target="+address) {
			t.Fatalf("expected last deploy to include %s, got: %s", address, last)
		}
	}
}

func TestRemoveOneReplicaRedeploysRemaining(t *testing.T) {
	rt := fakeruntime.NewRuntime()
	manager := kamalproxy.New(rt, kamalproxy.Options{Network: "serve"})

	replica1 := webTarget()
	replica2 := webTarget()
	replica2.ContainerName = "my-app-web-production-abc123-r2"

	if err := manager.AddTarget(context.Background(), replica1); err != nil {
		t.Fatalf("AddTarget r1: %v", err)
	}
	if err := manager.AddTarget(context.Background(), replica2); err != nil {
		t.Fatalf("AddTarget r2: %v", err)
	}
	if err := manager.RemoveTarget(context.Background(), replica1); err != nil {
		t.Fatalf("RemoveTarget r1: %v", err)
	}

	if got := countExecs(rt, "kamal-proxy remove"); got != 0 {
		t.Fatalf("removing one of two replicas must not remove the service, operations: %v", rt.Operations())
	}
	last := lastExec(t, rt, "kamal-proxy deploy")
	if strings.Contains(last, "my-app-web-production-abc123-r1:3000") {
		t.Fatalf("removed replica still routed: %s", last)
	}
	if !strings.Contains(last, "--target=my-app-web-production-abc123-r2:3000") {
		t.Fatalf("remaining replica not routed: %s", last)
	}
}

func TestSetTargetsReplacesTargetSetAtomically(t *testing.T) {
	rt := fakeruntime.NewRuntime()
	manager := kamalproxy.New(rt, kamalproxy.Options{Network: "serve"})

	old := webTarget()
	if err := manager.AddTarget(context.Background(), old); err != nil {
		t.Fatalf("AddTarget old: %v", err)
	}

	new1 := webTarget()
	new1.ContainerName = "my-app-web-production-def456-r1"
	new2 := webTarget()
	new2.ContainerName = "my-app-web-production-def456-r2"

	before := countExecs(rt, "kamal-proxy deploy")
	if err := manager.SetTargets(context.Background(), "my-app", "web", []proxy.Target{new1, new2}, proxy.RouteOptions{}); err != nil {
		t.Fatalf("SetTargets: %v", err)
	}
	if got := countExecs(rt, "kamal-proxy deploy"); got != before+1 {
		t.Fatalf("expected exactly one deploy exec for the cutover, got %d new", got-before)
	}

	last := lastExec(t, rt, "kamal-proxy deploy")
	if strings.Contains(last, "abc123") {
		t.Fatalf("old target still routed after cutover: %s", last)
	}
	for _, address := range []string{"my-app-web-production-def456-r1:3000", "my-app-web-production-def456-r2:3000"} {
		if !strings.Contains(last, "--target="+address) {
			t.Fatalf("cutover missing target %s: %s", address, last)
		}
	}

	// Unchanged set is a no-op.
	if err := manager.SetTargets(context.Background(), "my-app", "web", []proxy.Target{new2, new1}, proxy.RouteOptions{}); err != nil {
		t.Fatalf("SetTargets (repeat): %v", err)
	}
	if got := countExecs(rt, "kamal-proxy deploy"); got != before+1 {
		t.Fatalf("unchanged target set must not redeploy, operations: %v", rt.Operations())
	}
}

func TestSetTargetsPassesHostnamesTLSAndTimeouts(t *testing.T) {
	rt := fakeruntime.NewRuntime()
	manager := kamalproxy.New(rt, kamalproxy.Options{Network: "serve"})

	opts := proxy.RouteOptions{
		Hosts:         []string{"app.example.com", "www.example.com"},
		TLS:           true,
		DeployTimeout: 45 * time.Second,
		DrainTimeout:  60 * time.Second,
	}
	if err := manager.SetTargets(context.Background(), "my-app", "web", []proxy.Target{webTarget()}, opts); err != nil {
		t.Fatalf("SetTargets: %v", err)
	}

	last := lastExec(t, rt, "kamal-proxy deploy")
	for _, fragment := range []string{
		"--host=app.example.com",
		"--host=www.example.com",
		"--tls",
		"--deploy-timeout=45s",
		"--drain-timeout=60s",
	} {
		if !strings.Contains(last, fragment) {
			t.Fatalf("deploy exec missing %q: %s", fragment, last)
		}
	}
	if strings.Contains(last, "--deploy-timeout=30s") {
		t.Fatalf("route timeout must override the default: %s", last)
	}
}

func TestSetTargetsRedeploysWhenOnlyRouteOptionsChange(t *testing.T) {
	rt := fakeruntime.NewRuntime()
	manager := kamalproxy.New(rt, kamalproxy.Options{Network: "serve"})
	targets := []proxy.Target{webTarget()}

	if err := manager.SetTargets(context.Background(), "my-app", "web", targets, proxy.RouteOptions{}); err != nil {
		t.Fatalf("SetTargets initial: %v", err)
	}
	if err := manager.SetTargets(context.Background(), "my-app", "web", targets, proxy.RouteOptions{
		Hosts: []string{"app.example.com"}, TLS: true,
	}); err != nil {
		t.Fatalf("SetTargets changed route: %v", err)
	}

	if got := countExecs(rt, "kamal-proxy deploy"); got != 2 {
		t.Fatalf("route option change must redeploy, got %d execs: %v", got, rt.Operations())
	}
	last := lastExec(t, rt, "kamal-proxy deploy")
	if !strings.Contains(last, "--host=app.example.com") || !strings.Contains(last, "--tls") {
		t.Fatalf("changed route options were not applied: %s", last)
	}
}

func TestSetTargetsRedeploysWhenOnlyHealthPathChanges(t *testing.T) {
	rt := fakeruntime.NewRuntime()
	manager := kamalproxy.New(rt, kamalproxy.Options{Network: "serve"})
	target := webTarget()

	if err := manager.SetTargets(context.Background(), "my-app", "web", []proxy.Target{target}, proxy.RouteOptions{}); err != nil {
		t.Fatalf("SetTargets initial: %v", err)
	}
	target.HealthPath = "/ready"
	if err := manager.SetTargets(context.Background(), "my-app", "web", []proxy.Target{target}, proxy.RouteOptions{}); err != nil {
		t.Fatalf("SetTargets changed health path: %v", err)
	}

	if got := countExecs(rt, "kamal-proxy deploy"); got != 2 {
		t.Fatalf("health path change must redeploy, got %d execs: %v", got, rt.Operations())
	}
	if last := lastExec(t, rt, "kamal-proxy deploy"); !strings.Contains(last, "--health-check-path=/ready") {
		t.Fatalf("changed health path was not applied: %s", last)
	}
}

func TestHealingRedeployKeepsRouteOptions(t *testing.T) {
	rt := fakeruntime.NewRuntime()
	manager := kamalproxy.New(rt, kamalproxy.Options{Network: "serve"})

	replica1 := webTarget()
	replica2 := webTarget()
	replica2.ContainerName = "my-app-web-production-abc123-r2"
	opts := proxy.RouteOptions{Hosts: []string{"app.example.com"}, TLS: true}
	if err := manager.SetTargets(context.Background(), "my-app", "web", []proxy.Target{replica1, replica2}, opts); err != nil {
		t.Fatalf("SetTargets: %v", err)
	}

	// The healing path removes a single dead replica without knowing the
	// route options; the redeploy must keep them.
	if err := manager.RemoveTarget(context.Background(), replica1); err != nil {
		t.Fatalf("RemoveTarget: %v", err)
	}

	last := lastExec(t, rt, "kamal-proxy deploy")
	if !strings.Contains(last, "--host=app.example.com") || !strings.Contains(last, "--tls") {
		t.Fatalf("healing redeploy dropped route options: %s", last)
	}
	if strings.Contains(last, "my-app-web-production-abc123-r1:3000") {
		t.Fatalf("removed replica still routed: %s", last)
	}
}

func TestSetTargetsEmptyRemovesService(t *testing.T) {
	rt := fakeruntime.NewRuntime()
	manager := kamalproxy.New(rt, kamalproxy.Options{Network: "serve"})

	if err := manager.AddTarget(context.Background(), webTarget()); err != nil {
		t.Fatalf("AddTarget: %v", err)
	}
	if err := manager.SetTargets(context.Background(), "my-app", "web", nil, proxy.RouteOptions{}); err != nil {
		t.Fatalf("SetTargets empty: %v", err)
	}
	if got := countExecs(rt, "kamal-proxy remove"); got != 1 {
		t.Fatalf("expected empty target set to remove the service, operations: %v", rt.Operations())
	}
}

func startProxyContainer(t *testing.T, rt *fakeruntime.Runtime) {
	t.Helper()
	id, err := rt.CreateContainer(context.Background(), runtime.ContainerSpec{
		Name:  "serve-proxy",
		Image: kamalproxy.DefaultImage,
		Labels: map[string]string{
			"serve.managed":        "true",
			"serve.container_type": "proxy",
		},
	})
	if err != nil {
		t.Fatalf("create proxy container: %v", err)
	}
	if err := rt.StartContainer(context.Background(), id); err != nil {
		t.Fatalf("start proxy container: %v", err)
	}
}

func assertPublishesPort(t *testing.T, spec runtime.ContainerSpec, port int) {
	t.Helper()
	for _, p := range spec.Ports {
		if p.ContainerPort == port && p.HostPort == port {
			return
		}
	}
	t.Fatalf("proxy container does not publish port %d, ports: %+v", port, spec.Ports)
}

func findExec(t *testing.T, rt *fakeruntime.Runtime, prefix string) string {
	t.Helper()
	for _, op := range rt.Operations() {
		if strings.HasPrefix(op, "exec_container:serve-proxy:"+prefix) {
			return op
		}
	}
	t.Fatalf("no exec matching %q, operations: %v", prefix, rt.Operations())
	return ""
}

func lastExec(t *testing.T, rt *fakeruntime.Runtime, prefix string) string {
	t.Helper()
	last := ""
	for _, op := range rt.Operations() {
		if strings.HasPrefix(op, "exec_container:serve-proxy:"+prefix) {
			last = op
		}
	}
	if last == "" {
		t.Fatalf("no exec matching %q, operations: %v", prefix, rt.Operations())
	}
	return last
}

func countExecs(rt *fakeruntime.Runtime, prefix string) int {
	count := 0
	for _, op := range rt.Operations() {
		if strings.HasPrefix(op, "exec_container:serve-proxy:"+prefix) {
			count++
		}
	}
	return count
}
