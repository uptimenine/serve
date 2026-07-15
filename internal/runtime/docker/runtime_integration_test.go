//go:build integration

package docker_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/uptimenine/serve/internal/runtime"
	dockerruntime "github.com/uptimenine/serve/internal/runtime/docker"
)

const testImage = "busybox:1.36"

func TestDockerRuntimePullRunAndInspectContainer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	rt := newDockerRuntime(t)
	name := testContainerName(t, "inspect")

	if err := rt.PullImage(ctx, testImage); err != nil {
		t.Fatalf("pull image: %v", err)
	}
	id, err := rt.CreateContainer(ctx, runtime.ContainerSpec{
		Name:    name,
		Image:   testImage,
		Command: []string{"sh", "-c", "sleep 60"},
		Labels: map[string]string{
			"serve.integration_test": t.Name(),
			"serve.managed":          "true",
		},
	})
	if err != nil {
		t.Fatalf("create container: %v", err)
	}
	defer removeContainer(t, rt, id)

	if err := rt.StartContainer(ctx, id); err != nil {
		t.Fatalf("start container: %v", err)
	}

	state, err := rt.InspectContainer(ctx, id)
	if err != nil {
		t.Fatalf("inspect container: %v", err)
	}
	if !state.Running {
		t.Fatalf("expected container to be running, got %#v", state)
	}
	if state.Name != name || state.Image == "" {
		t.Fatalf("unexpected inspected state: %#v", state)
	}
	if state.Labels["serve.integration_test"] != t.Name() {
		t.Fatalf("expected labels round-tripped, got %#v", state.Labels)
	}
}

func TestDockerRuntimeCreatesNetworkBeforeContainerUsesIt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	rt := newDockerRuntime(t)
	networkName := testContainerName(t, "network")
	containerName := testContainerName(t, "networked")

	if err := rt.PullImage(ctx, testImage); err != nil {
		t.Fatalf("pull image: %v", err)
	}
	if err := rt.CreateNetwork(ctx, runtime.NetworkSpec{Name: networkName}); err != nil {
		t.Fatalf("create network: %v", err)
	}
	defer removeNetwork(t, rt, networkName)

	id, err := rt.CreateContainer(ctx, runtime.ContainerSpec{
		Name:    containerName,
		Image:   testImage,
		Command: []string{"sh", "-c", "sleep 60"},
		Network: networkName,
		Aliases: []string{"web"},
		Labels:  map[string]string{"serve.integration_test": t.Name()},
	})
	if err != nil {
		t.Fatalf("create networked container: %v", err)
	}
	defer removeContainer(t, rt, id)

	if err := rt.StartContainer(ctx, id); err != nil {
		t.Fatalf("start networked container: %v", err)
	}
	state, err := rt.InspectContainer(ctx, id)
	if err != nil {
		t.Fatalf("inspect networked container: %v", err)
	}
	if !state.Running {
		t.Fatalf("expected networked container to be running, got %#v", state)
	}
}

func TestDockerRuntimeListsContainersByLabel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	rt := newDockerRuntime(t)

	if err := rt.PullImage(ctx, testImage); err != nil {
		t.Fatalf("pull image: %v", err)
	}
	webID, err := rt.CreateContainer(ctx, runtime.ContainerSpec{
		Name:    testContainerName(t, "web"),
		Image:   testImage,
		Command: []string{"sh", "-c", "sleep 60"},
		Labels: map[string]string{
			"serve.integration_test": t.Name(),
			"serve.role":             "web",
		},
	})
	if err != nil {
		t.Fatalf("create web container: %v", err)
	}
	defer removeContainer(t, rt, webID)
	workerID, err := rt.CreateContainer(ctx, runtime.ContainerSpec{
		Name:    testContainerName(t, "worker"),
		Image:   testImage,
		Command: []string{"sh", "-c", "sleep 60"},
		Labels: map[string]string{
			"serve.integration_test": t.Name(),
			"serve.role":             "worker",
		},
	})
	if err != nil {
		t.Fatalf("create worker container: %v", err)
	}
	defer removeContainer(t, rt, workerID)
	if err := rt.StartContainer(ctx, webID); err != nil {
		t.Fatalf("start web container: %v", err)
	}
	if err := rt.StartContainer(ctx, workerID); err != nil {
		t.Fatalf("start worker container: %v", err)
	}

	containers, err := rt.ListContainers(ctx, runtime.ContainerFilters{Labels: map[string]string{
		"serve.integration_test": t.Name(),
		"serve.role":             "web",
	}})
	if err != nil {
		t.Fatalf("list containers: %v", err)
	}
	if len(containers) != 1 {
		t.Fatalf("expected one matching container, got %#v", containers)
	}
	if containers[0].ID != webID || !containers[0].Running {
		t.Fatalf("expected running web container %s, got %#v", webID, containers[0])
	}
}

func TestDockerRuntimeStreamsPlainLogs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	rt := newDockerRuntime(t)
	name := testContainerName(t, "logs")

	if err := rt.PullImage(ctx, testImage); err != nil {
		t.Fatalf("pull image: %v", err)
	}
	events, err := rt.Events(ctx)
	if err != nil {
		t.Fatalf("subscribe to events: %v", err)
	}
	id, err := rt.CreateContainer(ctx, runtime.ContainerSpec{
		Name:    name,
		Image:   testImage,
		Command: []string{"sh", "-c", "printf 'serve-log-line\\n'"},
		Labels:  map[string]string{"serve.integration_test": t.Name()},
	})
	if err != nil {
		t.Fatalf("create logging container: %v", err)
	}
	defer removeContainer(t, rt, id)
	if err := rt.StartContainer(ctx, id); err != nil {
		t.Fatalf("start logging container: %v", err)
	}
	waitForDie(t, ctx, events, id)

	logs, err := rt.Logs(ctx, id, runtime.LogOptions{})
	if err != nil {
		t.Fatalf("open logs: %v", err)
	}
	defer logs.Close()
	contents, err := io.ReadAll(logs)
	if err != nil {
		t.Fatalf("read logs: %v", err)
	}
	if string(contents) != "serve-log-line\n" {
		t.Fatalf("expected plain log output, got %q", string(contents))
	}
}

func TestDockerRuntimePublishesHostPorts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	rt := newDockerRuntime(t)
	name := testContainerName(t, "port")
	hostPort := freeTCPPort(t)

	if err := rt.PullImage(ctx, testImage); err != nil {
		t.Fatalf("pull image: %v", err)
	}
	id, err := rt.CreateContainer(ctx, runtime.ContainerSpec{
		Name:    name,
		Image:   testImage,
		Command: []string{"sh", "-c", "mkdir -p /www && printf 'ok\\n' >/www/index.html && httpd -f -p 8080 -h /www"},
		Ports:   []runtime.Port{{Name: "http", ContainerPort: 8080, HostPort: hostPort}},
		Labels:  map[string]string{"serve.integration_test": t.Name()},
	})
	if err != nil {
		t.Fatalf("create port publishing container: %v", err)
	}
	defer removeContainer(t, rt, id)
	if err := rt.StartContainer(ctx, id); err != nil {
		t.Fatalf("start port publishing container: %v", err)
	}

	body := waitForHTTP(t, ctx, fmt.Sprintf("http://127.0.0.1:%d/", hostPort))
	if body != "ok\n" {
		t.Fatalf("expected response body %q, got %q", "ok\n", body)
	}
}

func TestDockerRuntimeAppliesEnvFiles(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	rt := newDockerRuntime(t)
	name := testContainerName(t, "env-file")
	envFile := filepath.Join(t.TempDir(), "app.env")
	if err := os.WriteFile(envFile, []byte("SERVE_SECRET=available-from-env-file\n"), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}

	if err := rt.PullImage(ctx, testImage); err != nil {
		t.Fatalf("pull image: %v", err)
	}
	events, err := rt.Events(ctx)
	if err != nil {
		t.Fatalf("subscribe to events: %v", err)
	}
	id, err := rt.CreateContainer(ctx, runtime.ContainerSpec{
		Name:     name,
		Image:    testImage,
		Command:  []string{"sh", "-c", "test \"$SERVE_SECRET\" = available-from-env-file"},
		EnvFiles: []string{envFile},
		Labels:   map[string]string{"serve.integration_test": t.Name()},
	})
	if err != nil {
		t.Fatalf("create container: %v", err)
	}
	defer removeContainer(t, rt, id)

	if err := rt.StartContainer(ctx, id); err != nil {
		t.Fatalf("start container: %v", err)
	}

	event := waitForDie(t, ctx, events, id)
	if event.ExitCode != 0 {
		t.Fatalf("expected env-file backed command to exit 0, got %#v", event)
	}
}

func TestDockerRuntimeReceivesDieEvent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	rt := newDockerRuntime(t)
	name := testContainerName(t, "die")

	if err := rt.PullImage(ctx, testImage); err != nil {
		t.Fatalf("pull image: %v", err)
	}
	events, err := rt.Events(ctx)
	if err != nil {
		t.Fatalf("subscribe to events: %v", err)
	}
	id, err := rt.CreateContainer(ctx, runtime.ContainerSpec{
		Name:    name,
		Image:   testImage,
		Command: []string{"sh", "-c", "exit 42"},
		Labels: map[string]string{
			"serve.integration_test": t.Name(),
			"serve.managed":          "true",
		},
	})
	if err != nil {
		t.Fatalf("create container: %v", err)
	}
	defer removeContainer(t, rt, id)

	if err := rt.StartContainer(ctx, id); err != nil {
		t.Fatalf("start container: %v", err)
	}

	event := waitForDie(t, ctx, events, id)
	if event.ExitCode != 42 {
		t.Fatalf("expected exit code 42, got %#v", event)
	}
	if event.Labels["serve.integration_test"] != t.Name() {
		t.Fatalf("expected event labels, got %#v", event.Labels)
	}
}

func newDockerRuntime(t *testing.T) *dockerruntime.Runtime {
	t.Helper()
	rt, err := dockerruntime.NewFromEnv()
	if err != nil {
		t.Fatalf("create Docker runtime: %v", err)
	}
	return rt
}

func testContainerName(t *testing.T, suffix string) string {
	t.Helper()
	return fmt.Sprintf("serve-integration-%s-%d", suffix, time.Now().UnixNano())
}

func waitForDie(t *testing.T, ctx context.Context, events <-chan runtime.RuntimeEvent, id runtime.ContainerID) runtime.RuntimeEvent {
	t.Helper()
	for {
		select {
		case event := <-events:
			if event.ContainerID == id && event.Type == runtime.EventDie {
				return event
			}
		case <-ctx.Done():
			t.Fatalf("timed out waiting for die event: %v", ctx.Err())
		}
	}
}

func waitForHTTP(t *testing.T, ctx context.Context, url string) string {
	t.Helper()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		select {
		case <-ticker.C:
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				t.Fatalf("create request: %v", err)
			}
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				lastErr = err
				continue
			}
			body, readErr := io.ReadAll(response.Body)
			closeErr := response.Body.Close()
			if readErr != nil {
				t.Fatalf("read response body: %v", readErr)
			}
			if closeErr != nil {
				t.Fatalf("close response body: %v", closeErr)
			}
			if response.StatusCode == http.StatusOK {
				return string(body)
			}
			lastErr = fmt.Errorf("unexpected status %d with body %q", response.StatusCode, string(body))
		case <-ctx.Done():
			t.Fatalf("timed out waiting for HTTP response from %s: %v", url, lastErr)
		}
	}
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve TCP port: %v", err)
	}
	defer listener.Close()
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("expected TCP addr, got %T", listener.Addr())
	}
	return addr.Port
}

func removeContainer(t *testing.T, rt *dockerruntime.Runtime, id runtime.ContainerID) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = rt.StopContainer(ctx, id, time.Second)
	if err := rt.RemoveContainer(ctx, id); err != nil {
		t.Logf("remove container %s: %v", id, err)
	}
}

func removeNetwork(t *testing.T, rt *dockerruntime.Runtime, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := rt.RemoveNetwork(ctx, name); err != nil && !strings.Contains(err.Error(), "not found") {
		t.Logf("remove network %s: %v", name, err)
	}
}
