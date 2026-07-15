package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentstate "github.com/uptimenine/serve/internal/agent/state"
	"github.com/uptimenine/serve/internal/cli"
	"github.com/uptimenine/serve/internal/config"
	"github.com/uptimenine/serve/internal/planner"
	"github.com/uptimenine/serve/internal/runtime"
	"github.com/uptimenine/serve/internal/runtime/fake"
)

func TestVersionPrintsBuildVersion(t *testing.T) {
	cmd := cli.New("v1.2.3-test")

	var stdout bytes.Buffer
	exitCode := cmd.Run(context.Background(), []string{"version"}, &stdout, io.Discard)

	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", exitCode)
	}

	got := strings.TrimSpace(stdout.String())
	if got != "v1.2.3-test" {
		t.Fatalf("expected version output %q, got %q", "v1.2.3-test", got)
	}
}

func TestHelpCommandsPrintUsageWithImplementedAndPlannedCommands(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"--help"}, {"-h"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			cmd := cli.New("v1.2.3-test")

			var stdout bytes.Buffer
			exitCode := cmd.Run(context.Background(), args, &stdout, io.Discard)

			if exitCode != 0 {
				t.Fatalf("expected exit code 0, got %d", exitCode)
			}
			output := stdout.String()
			for _, expected := range []string{"Usage:", "serve <command>", "init", "status", "agent apply", "deploy", "logs", "events", "doctor", "remove", "secrets edit", "version", "help"} {
				if !strings.Contains(output, expected) {
					t.Fatalf("expected help output to contain %q, got:\n%s", expected, output)
				}
			}
		})
	}
}

func TestPlannedCommandReturnsNotImplementedInsteadOfUnknown(t *testing.T) {
	cmd := cli.New("v1.2.3-test")

	var stderr bytes.Buffer
	exitCode := cmd.Run(context.Background(), []string{"setup"}, io.Discard, &stderr)

	if exitCode != 1 {
		t.Fatalf("expected exit code 1, got %d", exitCode)
	}
	if !strings.Contains(stderr.String(), "serve setup is not implemented yet") {
		t.Fatalf("expected not implemented message, got %q", stderr.String())
	}
}

func TestInitCreatesValidStarterConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "serve.yml")
	cmd := cli.New("v1.2.3-test")

	var stdout bytes.Buffer
	exitCode := cmd.Run(context.Background(), []string{"init", "--path", path}, &stdout, io.Discard)

	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", exitCode)
	}
	if !strings.Contains(stdout.String(), "Created "+path) {
		t.Fatalf("expected created message, got %q", stdout.String())
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("expected generated config to validate: %v", err)
	}
	if cfg.Service != "my-app" || cfg.Image != "ghcr.io/acme/my-app" {
		t.Fatalf("unexpected generated config: %#v", cfg)
	}
}

func TestInitRefusesToOverwriteUnlessForced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "serve.yml")
	if err := os.WriteFile(path, []byte("service: existing\nimage: example/app\n"), 0o644); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	cmd := cli.New("v1.2.3-test")

	var stderr bytes.Buffer
	exitCode := cmd.Run(context.Background(), []string{"init", "--path", path}, io.Discard, &stderr)

	if exitCode != 1 {
		t.Fatalf("expected exit code 1, got %d", exitCode)
	}
	if !strings.Contains(stderr.String(), "already exists") {
		t.Fatalf("expected already exists error, got %q", stderr.String())
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(contents), "existing") {
		t.Fatalf("expected existing config to remain, got %q", string(contents))
	}

	var stdout bytes.Buffer
	exitCode = cmd.Run(context.Background(), []string{"init", "--path", path, "--force"}, &stdout, io.Discard)

	if exitCode != 0 {
		t.Fatalf("expected forced init exit code 0, got %d", exitCode)
	}
	contents, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("read forced config: %v", err)
	}
	if strings.Contains(string(contents), "existing") {
		t.Fatalf("expected forced init to overwrite existing config, got %q", string(contents))
	}
}

func TestStatusReportsNoManagedContainers(t *testing.T) {
	rt := fake.NewRuntime()
	cmd := cli.New("v1.2.3-test", cli.WithRuntime(rt))

	var stdout bytes.Buffer
	exitCode := cmd.Run(context.Background(), []string{"status"}, &stdout, io.Discard)

	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", exitCode)
	}
	if !strings.Contains(stdout.String(), "No Serve-managed containers found.") {
		t.Fatalf("unexpected status output: %q", stdout.String())
	}
}

func TestStatusListsManagedContainers(t *testing.T) {
	rt := fake.NewRuntime()
	id := createManagedContainer(t, rt, "my-app-web-production-abc123-r1", "web")
	if err := rt.StartContainer(context.Background(), id); err != nil {
		t.Fatalf("start container: %v", err)
	}
	cmd := cli.New("v1.2.3-test", cli.WithRuntime(rt))

	var stdout bytes.Buffer
	exitCode := cmd.Run(context.Background(), []string{"status"}, &stdout, io.Discard)

	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", exitCode)
	}
	output := stdout.String()
	for _, expected := range []string{"SERVICE", "DESTINATION", "ROLE", "VERSION", "CONTAINER", "STATUS", "my-app", "production", "web", "abc123", "my-app-web-production-abc123-r1", "running"} {
		if !strings.Contains(output, expected) {
			t.Fatalf("expected status output to contain %q, got:\n%s", expected, output)
		}
	}
}

func TestLogsStreamsSelectedContainerLogs(t *testing.T) {
	rt := fake.NewRuntime()
	id := createManagedContainer(t, rt, "my-app-web-production-abc123-r1", "web")
	rt.SetLogs(id, "hello from container\n")
	cmd := cli.New("v1.2.3-test", cli.WithRuntime(rt))

	var stdout bytes.Buffer
	exitCode := cmd.Run(context.Background(), []string{"logs", "--container", "my-app-web-production-abc123-r1"}, &stdout, io.Discard)

	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", exitCode)
	}
	if stdout.String() != "hello from container\n" {
		t.Fatalf("expected container logs, got %q", stdout.String())
	}
}

func TestEventsPrintsOneRuntimeEvent(t *testing.T) {
	rt := fake.NewRuntime()
	id := createManagedContainer(t, rt, "my-app-web-production-abc123-r1", "web")
	cmd := cli.New("v1.2.3-test", cli.WithRuntime(rt))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	done := make(chan int, 1)

	go func() {
		done <- cmd.Run(ctx, []string{"events", "--once"}, &stdout, &stderr)
	}()
	rt.WaitForSubscribers(t, 1)
	rt.Die(id, 137, true)

	select {
	case exitCode := <-done:
		if exitCode != 0 {
			t.Fatalf("expected exit code 0, got %d, stderr %q", exitCode, stderr.String())
		}
	case <-ctx.Done():
		t.Fatalf("timed out waiting for events command: %v", ctx.Err())
	}
	output := stdout.String()
	for _, expected := range []string{"die", "my-app-web-production-abc123-r1", "137", "oom=true"} {
		if !strings.Contains(output, expected) {
			t.Fatalf("expected event output to contain %q, got %q", expected, output)
		}
	}
}

func TestRemoveDeletesMatchingManagedContainers(t *testing.T) {
	rt := fake.NewRuntime()
	createManagedContainer(t, rt, "my-app-web-production-abc123-r1", "web")
	createManagedContainer(t, rt, "other-worker-production-abc123-r1", "worker")
	cmd := cli.New("v1.2.3-test", cli.WithRuntime(rt))

	var stdout bytes.Buffer
	exitCode := cmd.Run(context.Background(), []string{"remove", "--service", "my-app", "--destination", "production", "--force"}, &stdout, io.Discard)

	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", exitCode)
	}
	if !strings.Contains(stdout.String(), "Removed 1 container") {
		t.Fatalf("expected removal summary, got %q", stdout.String())
	}
	remaining, err := rt.ListContainers(context.Background(), runtime.ContainerFilters{Labels: map[string]string{"serve.managed": "true"}})
	if err != nil {
		t.Fatalf("list containers: %v", err)
	}
	if len(remaining) != 1 || remaining[0].Labels["serve.service"] != "other" {
		t.Fatalf("expected only other service to remain, got %#v", remaining)
	}
}

func TestDoctorReportsDockerAndNetworkChecks(t *testing.T) {
	rt := fake.NewRuntime()
	cmd := cli.New("v1.2.3-test", cli.WithRuntime(rt))

	var stdout bytes.Buffer
	exitCode := cmd.Run(context.Background(), []string{"doctor"}, &stdout, io.Discard)

	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", exitCode)
	}
	output := stdout.String()
	for _, expected := range []string{"Docker reachable: ok", "serve network: ok"} {
		if !strings.Contains(output, expected) {
			t.Fatalf("expected doctor output to contain %q, got %q", expected, output)
		}
	}
}

func TestSecretsEditRunsSOPSForSecretsFile(t *testing.T) {
	runner := &recordingRunner{}
	cmd := cli.New("v1.2.3-test", cli.WithRunner(runner))

	var stdout bytes.Buffer
	exitCode := cmd.Run(context.Background(), []string{"secrets", "edit", "--file", "prod.secrets.yml"}, &stdout, io.Discard)

	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", exitCode)
	}
	if runner.name != "sops" || len(runner.args) != 1 || runner.args[0] != "prod.secrets.yml" {
		t.Fatalf("expected sops prod.secrets.yml, got name=%q args=%#v", runner.name, runner.args)
	}
	if !strings.Contains(stdout.String(), "Edited prod.secrets.yml") {
		t.Fatalf("expected edit message, got %q", stdout.String())
	}
}

func TestAgentApplyLoadsDesiredStateSavesItAndReconciles(t *testing.T) {
	rt := fake.NewRuntime()
	stateDir := t.TempDir()
	desiredPath := writeDesiredState(t, stateDir, desiredState("abc123"))
	cmd := cli.New("v1.2.3-test", cli.WithRuntime(rt))

	var stdout bytes.Buffer
	exitCode := cmd.Run(context.Background(), []string{"agent", "apply", desiredPath, "--state-dir", stateDir}, &stdout, io.Discard)

	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", exitCode)
	}
	if !strings.Contains(stdout.String(), "Applied desired state") {
		t.Fatalf("expected apply result, got %q", stdout.String())
	}
	stored, err := agentstate.NewStore(stateDir).LoadDesired("my-app", "production")
	if err != nil {
		t.Fatalf("load stored desired state: %v", err)
	}
	if stored.Version != "abc123" {
		t.Fatalf("expected stored desired version abc123, got %#v", stored)
	}
	containers := listManagedContainers(t, rt)
	if len(containers) != 1 || !containers[0].Running {
		t.Fatalf("expected one running managed container, got %#v", containers)
	}
}

func TestAgentApplyRejectsUnsafeIdentityBeforeRuntimeChanges(t *testing.T) {
	rt := fake.NewRuntime()
	stateDir := t.TempDir()
	desired := desiredState("abc123")
	desired.Service = "../escape"
	desiredPath := writeDesiredState(t, stateDir, desired)
	cmd := cli.New("v1.2.3-test", cli.WithRuntime(rt))

	exitCode := cmd.Run(context.Background(), []string{"agent", "apply", desiredPath, "--state-dir", stateDir}, io.Discard, io.Discard)

	if exitCode != 1 {
		t.Fatalf("expected exit code 1, got %d", exitCode)
	}
	if operations := rt.Operations(); len(operations) != 0 {
		t.Fatalf("unsafe desired state changed runtime: %v", operations)
	}
}

func TestAgentRunStartsDaemonAndStopsOnCancel(t *testing.T) {
	rt := fake.NewRuntime()
	dir, err := os.MkdirTemp("", "serveagent")
	if err != nil {
		t.Fatalf("make temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "agent.sock")

	cmd := cli.New("v1.2.3-test", cli.WithRuntime(rt))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- cmd.Run(ctx, []string{"agent", "run", "--state-dir", filepath.Join(dir, "state"), "--socket", socket, "--reconcile-interval", "50ms"}, io.Discard, io.Discard)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("agent socket never appeared at %s", socket)
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case exitCode := <-done:
		if exitCode != 0 {
			t.Fatalf("expected exit code 0 after cancel, got %d", exitCode)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("agent run did not stop after cancel")
	}
}

func TestLocalDeployPlansAndAppliesDesiredState(t *testing.T) {
	rt := fake.NewRuntime()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "serve.yml")
	if err := os.WriteFile(configPath, []byte(`service: my-app
image: ghcr.io/acme/my-app
destination: production
servers:
  web:
    hosts:
      - localhost
    command: ./server
    app_port: 3000
    replicas: 1
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cmd := cli.New("v1.2.3-test", cli.WithRuntime(rt))

	var stdout bytes.Buffer
	exitCode := cmd.Run(context.Background(), []string{"deploy", "--local", "--config", configPath, "--host", "localhost", "--version", "abc123", "--state-dir", dir}, &stdout, io.Discard)

	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", exitCode)
	}
	if !strings.Contains(stdout.String(), "Deployed my-app production abc123 locally") {
		t.Fatalf("unexpected deploy output: %q", stdout.String())
	}
	containers := listManagedContainers(t, rt)
	byName := map[string]runtime.ContainerState{}
	for _, container := range containers {
		byName[container.Name] = container
	}
	app, ok := byName["my-app-web-production-abc123-r1"]
	if !ok || !app.Running {
		t.Fatalf("expected running app container, got %#v", containers)
	}
	proxy, ok := byName["serve-proxy"]
	if !ok || !proxy.Running {
		t.Fatalf("expected deploy to boot the serve-proxy container, got %#v", containers)
	}
	deployed := false
	for _, op := range rt.Operations() {
		if strings.HasPrefix(op, "exec_container:serve-proxy:kamal-proxy deploy my-app-web") {
			deployed = true
		}
	}
	if !deployed {
		t.Fatalf("expected traffic to be routed via kamal-proxy, operations: %v", rt.Operations())
	}
	lastGood, err := agentstate.NewStore(dir).LoadLastGood("my-app", "production")
	if err != nil {
		t.Fatalf("expected deploy to record last-good state: %v", err)
	}
	if lastGood.Version != "abc123" {
		t.Fatalf("last-good version = %q, want abc123", lastGood.Version)
	}
}

func TestRemoteDeployAppliesStateTransactionallyOnEachHost(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "serve.yml")
	if err := os.WriteFile(configPath, []byte(`service: my-app
image: ghcr.io/acme/my-app
destination: production
servers:
  web:
    hosts:
      - app1.example.com
      - app2.example.com
    command: ./server
    app_port: 3000
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	ssh := &recordingSSHRunner{}
	cmd := cli.New("v1.2.3-test", cli.WithSSHRunner(ssh))

	var stdout bytes.Buffer
	exitCode := cmd.Run(context.Background(), []string{"deploy", "--config", configPath, "--version", "abc123"}, &stdout, io.Discard)

	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", exitCode)
	}

	for _, host := range []string{"app1.example.com", "app2.example.com"} {
		apply := ssh.callFor(t, host, "agent apply")
		if apply.command != "sudo serve agent apply /dev/stdin --state-dir /var/lib/serve/state" {
			t.Fatalf("apply command for %s = %q", host, apply.command)
		}
		var desired planner.DesiredState
		if err := json.Unmarshal([]byte(apply.stdin), &desired); err != nil {
			t.Fatalf("applied state for %s is not JSON: %v", host, err)
		}
		if desired.Host != host || desired.Version != "abc123" {
			t.Fatalf("uploaded state for %s has host=%q version=%q", host, desired.Host, desired.Version)
		}
		if len(desired.Containers) != 1 || desired.Containers[0].Name != "my-app-web-production-abc123-r1" {
			t.Fatalf("unexpected containers for %s: %#v", host, desired.Containers)
		}
	}
	if !strings.Contains(stdout.String(), "app1.example.com") || !strings.Contains(stdout.String(), "app2.example.com") {
		t.Fatalf("deploy output should report each host, got %q", stdout.String())
	}
}

func TestRemoteDeployEmbedsEncryptedSecretsFile(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "serve.yml")
	if err := os.WriteFile(configPath, []byte(`service: my-app
image: ghcr.io/acme/my-app
destination: production
servers:
  web:
    hosts:
      - app1.example.com
    command: ./server
    app_port: 3000
env:
  secret:
    - DATABASE_URL
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	encryptedFile := "DATABASE_URL: ENC[AES256_GCM,data:database-ciphertext]\nsops:\n  kms: []\n"
	if err := os.WriteFile(filepath.Join(dir, "serve.secrets.yml"), []byte(encryptedFile), 0o644); err != nil {
		t.Fatalf("write secrets file: %v", err)
	}

	ssh := &recordingSSHRunner{}
	cmd := cli.New("v1.2.3-test", cli.WithSSHRunner(ssh))

	exitCode := cmd.Run(context.Background(), []string{"deploy", "--config", configPath, "--version", "abc123"}, io.Discard, io.Discard)

	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", exitCode)
	}
	upload := ssh.callFor(t, "app1.example.com", "agent apply")
	var desired planner.DesiredState
	if err := json.Unmarshal([]byte(upload.stdin), &desired); err != nil {
		t.Fatalf("uploaded state is not JSON: %v", err)
	}
	if desired.SecretsFile != encryptedFile {
		t.Fatalf("uploaded state must embed the encrypted secrets file, got %q", desired.SecretsFile)
	}
	if len(desired.Containers) != 1 || len(desired.Containers[0].SecretNames) != 1 {
		t.Fatalf("expected secret names on container, got %#v", desired.Containers)
	}
}

func TestRemoteDeployWithSecretsFailsClearlyWhenSecretsFileMissing(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "serve.yml")
	if err := os.WriteFile(configPath, []byte(`service: my-app
image: ghcr.io/acme/my-app
destination: production
servers:
  web:
    hosts:
      - app1.example.com
    command: ./server
    app_port: 3000
env:
  secret:
    - DATABASE_URL
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	ssh := &recordingSSHRunner{}
	cmd := cli.New("v1.2.3-test", cli.WithSSHRunner(ssh))

	var stderr bytes.Buffer
	exitCode := cmd.Run(context.Background(), []string{"deploy", "--config", configPath, "--version", "abc123"}, io.Discard, &stderr)

	if exitCode != 1 {
		t.Fatalf("expected exit code 1, got %d", exitCode)
	}
	if !strings.Contains(stderr.String(), "serve.secrets.yml") {
		t.Fatalf("expected error to name the missing secrets file, got %q", stderr.String())
	}
	if len(ssh.calls) != 0 {
		t.Fatalf("missing secrets file must fail before host contact, got %#v", ssh.calls)
	}
}

func TestRemoteDeployFailsWhenAnyHostFails(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "serve.yml")
	if err := os.WriteFile(configPath, []byte(`service: my-app
image: ghcr.io/acme/my-app
destination: production
servers:
  web:
    hosts:
      - app1.example.com
      - app2.example.com
    command: ./server
    app_port: 3000
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	ssh := &recordingSSHRunner{failHost: "app2.example.com"}
	cmd := cli.New("v1.2.3-test", cli.WithSSHRunner(ssh))

	var stderr bytes.Buffer
	exitCode := cmd.Run(context.Background(), []string{"deploy", "--config", configPath, "--version", "abc123"}, io.Discard, &stderr)

	if exitCode != 1 {
		t.Fatalf("expected exit code 1, got %d", exitCode)
	}
	if !strings.Contains(stderr.String(), "app2.example.com") {
		t.Fatalf("expected failing host in error, got %q", stderr.String())
	}
}

func TestRemoteDeployInvalidConfigFailsBeforeHostContact(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "serve.yml")
	if err := os.WriteFile(configPath, []byte("image: ghcr.io/acme/my-app\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	ssh := &recordingSSHRunner{}
	cmd := cli.New("v1.2.3-test", cli.WithSSHRunner(ssh))

	exitCode := cmd.Run(context.Background(), []string{"deploy", "--config", configPath, "--version", "abc123"}, io.Discard, io.Discard)

	if exitCode != 1 {
		t.Fatalf("expected exit code 1, got %d", exitCode)
	}
	if len(ssh.calls) != 0 {
		t.Fatalf("invalid config must fail before any host contact, got calls %#v", ssh.calls)
	}
}

func TestAgentReconcilePokesDaemonSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "servepoke")
	if err != nil {
		t.Fatalf("make temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "agent.sock")

	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	requests := make(chan string, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Method + " " + r.URL.Path
		w.WriteHeader(http.StatusOK)
	})}
	go server.Serve(listener)
	defer server.Close()

	cmd := cli.New("v1.2.3-test")
	var stdout bytes.Buffer
	exitCode := cmd.Run(context.Background(), []string{"agent", "reconcile", "--socket", socket}, &stdout, io.Discard)

	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", exitCode)
	}
	select {
	case request := <-requests:
		if request != "POST /v1/reconcile" {
			t.Fatalf("expected POST /v1/reconcile, got %q", request)
		}
	case <-time.After(time.Second):
		t.Fatalf("daemon socket never received a request")
	}
}

func TestAgentStatusPrintsDaemonStatus(t *testing.T) {
	socket := startStubAgentSocket(t, map[string]string{
		"GET /v1/status": `[{"service":"my-app","destination":"production","containers":[{"name":"my-app-web-production-abc123-r1","role":"web","version":"abc123","status":"running"}]}]`,
	})

	cmd := cli.New("v1.2.3-test")
	var stdout bytes.Buffer
	exitCode := cmd.Run(context.Background(), []string{"agent", "status", "--socket", socket}, &stdout, io.Discard)

	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", exitCode)
	}
	for _, expected := range []string{"my-app", "production", "web", "abc123", "running"} {
		if !strings.Contains(stdout.String(), expected) {
			t.Fatalf("expected status output to contain %q, got %q", expected, stdout.String())
		}
	}
}

func TestAgentLogsStreamsFromDaemonSocket(t *testing.T) {
	socket := startStubAgentSocket(t, map[string]string{
		"GET /v1/logs": "hello from container\n",
	})

	cmd := cli.New("v1.2.3-test")
	var stdout bytes.Buffer
	exitCode := cmd.Run(context.Background(), []string{"agent", "logs", "--container", "my-app-web-production-abc123-r1", "--socket", socket}, &stdout, io.Discard)

	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", exitCode)
	}
	if stdout.String() != "hello from container\n" {
		t.Fatalf("expected streamed logs, got %q", stdout.String())
	}
}

func TestRemoteStatusRunsAgentStatusOnEachConfigHost(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "serve.yml")
	if err := os.WriteFile(configPath, []byte(`service: my-app
image: ghcr.io/acme/my-app
destination: production
servers:
  web:
    hosts:
      - app1.example.com
      - app2.example.com
    command: ./server
    app_port: 3000
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	ssh := &recordingSSHRunner{outputFunc: func(host string, command string) string {
		return "status-from-" + host + "\n"
	}}
	cmd := cli.New("v1.2.3-test", cli.WithSSHRunner(ssh))

	var stdout bytes.Buffer
	exitCode := cmd.Run(context.Background(), []string{"status", "--config", configPath}, &stdout, io.Discard)

	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", exitCode)
	}
	for _, host := range []string{"app1.example.com", "app2.example.com"} {
		call := ssh.callFor(t, host, "serve agent status")
		if !strings.Contains(call.command, "sudo serve agent status --json") {
			t.Fatalf("status command for %s = %q", host, call.command)
		}
		if !strings.Contains(stdout.String(), "status-from-"+host) {
			t.Fatalf("expected agent output for %s, got %q", host, stdout.String())
		}
		if !strings.Contains(stdout.String(), host) {
			t.Fatalf("expected host header for %s, got %q", host, stdout.String())
		}
	}
}

func TestRemoteLogsRunsAgentLogsOnHost(t *testing.T) {
	ssh := &recordingSSHRunner{outputFunc: func(string, string) string { return "remote log line\n" }}
	cmd := cli.New("v1.2.3-test", cli.WithSSHRunner(ssh))

	var stdout bytes.Buffer
	exitCode := cmd.Run(context.Background(), []string{"logs", "--host", "app1.example.com", "--container", "my-app-web-production-abc123-r1"}, &stdout, io.Discard)

	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", exitCode)
	}
	call := ssh.callFor(t, "app1.example.com", "serve agent logs")
	if !strings.Contains(call.command, "sudo serve agent logs --container 'my-app-web-production-abc123-r1'") {
		t.Fatalf("logs command = %q", call.command)
	}
	if !strings.Contains(stdout.String(), "remote log line") {
		t.Fatalf("expected remote log output, got %q", stdout.String())
	}
}

func TestRemoteLogsQuotesContainerForRemoteShell(t *testing.T) {
	ssh := &recordingSSHRunner{}
	cmd := cli.New("v1.2.3-test", cli.WithSSHRunner(ssh))

	exitCode := cmd.Run(context.Background(), []string{
		"logs", "--host", "app1.example.com", "--container", "app; touch /tmp/host-pwned",
	}, io.Discard, io.Discard)

	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", exitCode)
	}
	call := ssh.callFor(t, "app1.example.com", "serve agent logs")
	if !strings.Contains(call.command, "--container 'app; touch /tmp/host-pwned'") {
		t.Fatalf("container was not shell-quoted: %q", call.command)
	}
}

func TestRemoteCommandsRejectSSHOptionAsHost(t *testing.T) {
	ssh := &recordingSSHRunner{}
	cmd := cli.New("v1.2.3-test", cli.WithSSHRunner(ssh))

	exitCode := cmd.Run(context.Background(), []string{
		"logs", "--host", "-oProxyCommand=touch /tmp/local-pwned", "--container", "app",
	}, io.Discard, io.Discard)

	if exitCode != 1 {
		t.Fatalf("expected exit code 1, got %d", exitCode)
	}
	if len(ssh.calls) != 0 {
		t.Fatalf("unsafe host reached SSH runner: %#v", ssh.calls)
	}
}

func TestRemoteEventsRunsAgentEventsOnHost(t *testing.T) {
	ssh := &recordingSSHRunner{}
	cmd := cli.New("v1.2.3-test", cli.WithSSHRunner(ssh))

	exitCode := cmd.Run(context.Background(), []string{"events", "--host", "app1.example.com", "--once"}, io.Discard, io.Discard)

	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", exitCode)
	}
	call := ssh.callFor(t, "app1.example.com", "serve agent events")
	if !strings.Contains(call.command, "sudo serve agent events --once") {
		t.Fatalf("events command = %q", call.command)
	}
}

func TestExecRunsCommandInLocalContainer(t *testing.T) {
	rt := fake.NewRuntime()
	id := createManagedContainer(t, rt, "my-app-web-production-abc123-r1", "web")
	if err := rt.StartContainer(context.Background(), id); err != nil {
		t.Fatalf("start container: %v", err)
	}
	rt.SetExecResult("my-app-web-production-abc123-r1", "total 0\n", nil)
	cmd := cli.New("v1.2.3-test", cli.WithRuntime(rt))

	var stdout bytes.Buffer
	exitCode := cmd.Run(context.Background(), []string{"exec", "--container", "my-app-web-production-abc123-r1", "--", "ls", "-la"}, &stdout, io.Discard)

	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", exitCode)
	}
	if !strings.Contains(stdout.String(), "total 0") {
		t.Fatalf("expected exec output, got %q", stdout.String())
	}
	found := false
	for _, op := range rt.Operations() {
		if op == "exec_container:my-app-web-production-abc123-r1:ls -la" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected exec operation, got %v", rt.Operations())
	}
}

func TestExecRemoteRunsServeExecOverSSH(t *testing.T) {
	ssh := &recordingSSHRunner{outputFunc: func(string, string) string { return "remote exec output\n" }}
	cmd := cli.New("v1.2.3-test", cli.WithSSHRunner(ssh))

	var stdout bytes.Buffer
	exitCode := cmd.Run(context.Background(), []string{"exec", "--host", "app1.example.com", "--container", "my-app-web-production-abc123-r1", "--", "ls", "-la"}, &stdout, io.Discard)

	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", exitCode)
	}
	call := ssh.callFor(t, "app1.example.com", "serve exec")
	if call.command != "sudo serve exec --container 'my-app-web-production-abc123-r1' -- 'ls' '-la'" {
		t.Fatalf("exec command = %q", call.command)
	}
	if !strings.Contains(stdout.String(), "remote exec output") {
		t.Fatalf("expected remote output, got %q", stdout.String())
	}
}

func TestExecRemoteQuotesEveryArgumentForTheRemoteShell(t *testing.T) {
	ssh := &recordingSSHRunner{}
	cmd := cli.New("v1.2.3-test", cli.WithSSHRunner(ssh))

	exitCode := cmd.Run(context.Background(), []string{
		"exec", "--host", "app1.example.com",
		"--container", "app; touch /tmp/host-pwned",
		"--", "sh", "-c", "printf '%s' \"$HOME\"; touch /tmp/host-pwned",
	}, io.Discard, io.Discard)

	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", exitCode)
	}
	call := ssh.callFor(t, "app1.example.com", "serve exec")
	want := "sudo serve exec --container 'app; touch /tmp/host-pwned' -- 'sh' '-c' 'printf '\"'\"'%s'\"'\"' \"$HOME\"; touch /tmp/host-pwned'"
	if call.command != want {
		t.Fatalf("remote command\n got: %s\nwant: %s", call.command, want)
	}
}

func TestExecRequiresContainerAndCommand(t *testing.T) {
	cmd := cli.New("v1.2.3-test", cli.WithRuntime(fake.NewRuntime()))

	var stderr bytes.Buffer
	if exitCode := cmd.Run(context.Background(), []string{"exec", "--container", "x"}, io.Discard, &stderr); exitCode != 1 {
		t.Fatalf("expected exit 1 without command, got %d", exitCode)
	}
	if !strings.Contains(stderr.String(), "--") {
		t.Fatalf("expected error to mention -- separator, got %q", stderr.String())
	}

	stderr.Reset()
	if exitCode := cmd.Run(context.Background(), []string{"exec", "--", "ls"}, io.Discard, &stderr); exitCode != 1 {
		t.Fatalf("expected exit 1 without container, got %d", exitCode)
	}
	if !strings.Contains(stderr.String(), "--container") {
		t.Fatalf("expected error to mention --container, got %q", stderr.String())
	}
}

// startStubAgentSocket serves canned responses keyed by "METHOD /path" on a
// Unix socket, returning the socket path.
func startStubAgentSocket(t *testing.T, responses map[string]string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "servestub")
	if err != nil {
		t.Fatalf("make temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "agent.sock")

	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := responses[r.Method+" "+r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	})}
	go server.Serve(listener)
	t.Cleanup(func() {
		server.Close()
		listener.Close()
	})
	return socket
}

type sshCall struct {
	host    string
	command string
	stdin   string
}

type recordingSSHRunner struct {
	calls      []sshCall
	failHost   string
	outputFunc func(host string, command string) string
}

func (r *recordingSSHRunner) Run(ctx context.Context, host string, command string, stdin io.Reader, stdout io.Writer) error {
	_ = ctx
	var contents []byte
	if stdin != nil {
		contents, _ = io.ReadAll(stdin)
	}
	r.calls = append(r.calls, sshCall{host: host, command: command, stdin: string(contents)})
	if r.failHost != "" && host == r.failHost {
		return fmt.Errorf("ssh %s: connection refused", host)
	}
	if r.outputFunc != nil && stdout != nil {
		_, _ = io.WriteString(stdout, r.outputFunc(host, command))
	}
	return nil
}

func (r *recordingSSHRunner) callFor(t *testing.T, host string, fragment string) sshCall {
	t.Helper()
	for _, call := range r.calls {
		if call.host == host && strings.Contains(call.command, fragment) {
			return call
		}
	}
	t.Fatalf("no ssh call on %s containing %q, calls: %#v", host, fragment, r.calls)
	return sshCall{}
}

func TestRollbackAppliesLastGoodState(t *testing.T) {
	rt := fake.NewRuntime()
	stateDir := t.TempDir()
	lastGood := desiredState("abc123")
	if err := agentstate.NewStore(stateDir).SaveLastGood(lastGood); err != nil {
		t.Fatalf("save last-good state: %v", err)
	}
	cmd := cli.New("v1.2.3-test", cli.WithRuntime(rt))

	var stdout bytes.Buffer
	exitCode := cmd.Run(context.Background(), []string{"rollback", "--service", "my-app", "--destination", "production", "--state-dir", stateDir}, &stdout, io.Discard)

	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", exitCode)
	}
	if !strings.Contains(stdout.String(), "Rolled back my-app production to abc123") {
		t.Fatalf("expected rollback summary, got %q", stdout.String())
	}
	containers := listManagedContainers(t, rt)
	if len(containers) != 1 || containers[0].Name != "my-app-web-production-abc123-r1" || !containers[0].Running {
		t.Fatalf("expected last-good container to be running, got %#v", containers)
	}
}

func TestRollbackLogsStartedAndCompletedEvents(t *testing.T) {
	rt := fake.NewRuntime()
	stateDir := t.TempDir()
	if err := agentstate.NewStore(stateDir).SaveLastGood(desiredState("abc123")); err != nil {
		t.Fatalf("save last-good state: %v", err)
	}
	cmd := cli.New("v1.2.3-test", cli.WithRuntime(rt))

	var stdout bytes.Buffer
	exitCode := cmd.Run(context.Background(), []string{"rollback", "--service", "my-app", "--destination", "production", "--state-dir", stateDir}, &stdout, io.Discard)

	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", exitCode)
	}

	found := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		var decoded map[string]any
		if err := json.Unmarshal([]byte(line), &decoded); err != nil {
			continue
		}
		event, _ := decoded["event"].(string)
		if event != "rollback_started" && event != "rollback_completed" {
			continue
		}
		if decoded["service"] != "my-app" || decoded["destination"] != "production" || decoded["version"] != "abc123" {
			t.Fatalf("rollback event missing identity fields: %s", line)
		}
		found[event] = true
	}
	if !found["rollback_started"] || !found["rollback_completed"] {
		t.Fatalf("expected rollback_started and rollback_completed events, got %q", stdout.String())
	}
}

func TestPruneRemovesStoppedManagedContainers(t *testing.T) {
	rt := fake.NewRuntime()
	stoppedID := createManagedContainer(t, rt, "my-app-web-production-old-r1", "web")
	runningID := createManagedContainer(t, rt, "my-app-web-production-new-r1", "web")
	if err := rt.StartContainer(context.Background(), runningID); err != nil {
		t.Fatalf("start container: %v", err)
	}
	cmd := cli.New("v1.2.3-test", cli.WithRuntime(rt))

	var stdout bytes.Buffer
	exitCode := cmd.Run(context.Background(), []string{"prune", "--force"}, &stdout, io.Discard)

	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", exitCode)
	}
	if !strings.Contains(stdout.String(), "Pruned 1 container") {
		t.Fatalf("expected prune summary, got %q", stdout.String())
	}
	containers := listManagedContainers(t, rt)
	if len(containers) != 1 || containers[0].ID != runningID {
		t.Fatalf("expected only running container %s to remain after pruning stopped %s, got %#v", runningID, stoppedID, containers)
	}
}

func writeDesiredState(t *testing.T, dir string, desired planner.DesiredState) string {
	t.Helper()
	path := filepath.Join(dir, "desired.json")
	contents, err := json.Marshal(desired)
	if err != nil {
		t.Fatalf("marshal desired state: %v", err)
	}
	if err := os.WriteFile(path, contents, 0o644); err != nil {
		t.Fatalf("write desired state: %v", err)
	}
	return path
}

func desiredState(version string) planner.DesiredState {
	return planner.DesiredState{
		Service:          "my-app",
		Destination:      "production",
		Host:             "localhost",
		Version:          version,
		Network:          "serve",
		RetainContainers: 5,
		Containers: []planner.Container{
			{
				Name:          "my-app-web-production-" + version + "-r1",
				Role:          "web",
				ContainerType: "app",
				Image:         "ghcr.io/acme/my-app:" + version,
				Command:       []string{"./server"},
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

func createManagedContainer(t *testing.T, rt *fake.Runtime, name string, role string) runtime.ContainerID {
	t.Helper()
	service := "my-app"
	if strings.HasPrefix(name, "other-") {
		service = "other"
	}
	id, err := rt.CreateContainer(context.Background(), runtime.ContainerSpec{
		Name:  name,
		Image: "ghcr.io/acme/" + service + ":abc123",
		Labels: map[string]string{
			"serve.managed":        "true",
			"serve.service":        service,
			"serve.destination":    "production",
			"serve.role":           role,
			"serve.version":        "abc123",
			"serve.replica":        "1",
			"serve.container_type": "app",
		},
	})
	if err != nil {
		t.Fatalf("create managed container: %v", err)
	}
	return id
}

func listManagedContainers(t *testing.T, rt *fake.Runtime) []runtime.ContainerState {
	t.Helper()
	containers, err := rt.ListContainers(context.Background(), runtime.ContainerFilters{Labels: map[string]string{"serve.managed": "true"}})
	if err != nil {
		t.Fatalf("list containers: %v", err)
	}
	return containers
}

type recordingRunner struct {
	name string
	args []string
	err  error
}

func (r *recordingRunner) Run(ctx context.Context, name string, args ...string) error {
	_ = ctx
	r.name = name
	r.args = append([]string(nil), args...)
	return r.err
}
