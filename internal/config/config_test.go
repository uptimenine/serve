package config_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/uptimenine/serve/internal/config"
)

func TestLoadMissingFileReturnsClearError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "serve.yml")

	_, err := config.Load(path)

	if err == nil {
		t.Fatal("expected missing config file error, got nil")
	}

	message := err.Error()
	if !strings.Contains(message, "config file not found") {
		t.Fatalf("expected error to explain the file is missing, got %q", message)
	}
	if !strings.Contains(message, path) {
		t.Fatalf("expected error to include missing path %q, got %q", path, message)
	}
}

func TestLoadAcceptsMinimalValidConfig(t *testing.T) {
	path := writeConfig(t, "serve.yml", `
service: my-app
image: ghcr.io/acme/my-app
`)

	cfg, err := config.Load(path)

	if err != nil {
		t.Fatalf("expected valid config, got error: %v", err)
	}
	if cfg.Service != "my-app" {
		t.Fatalf("expected service %q, got %q", "my-app", cfg.Service)
	}
	if cfg.Image != "ghcr.io/acme/my-app" {
		t.Fatalf("expected image %q, got %q", "ghcr.io/acme/my-app", cfg.Image)
	}
}

func TestLoadRejectsMissingService(t *testing.T) {
	path := writeConfig(t, "serve.yml", `
image: ghcr.io/acme/my-app
`)

	_, err := config.Load(path)

	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	if !strings.Contains(err.Error(), "service is required") {
		t.Fatalf("expected missing service error, got %q", err.Error())
	}
}

func TestLoadRejectsMissingImage(t *testing.T) {
	path := writeConfig(t, "serve.yml", `
service: my-app
`)

	_, err := config.Load(path)

	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	if !strings.Contains(err.Error(), "image is required") {
		t.Fatalf("expected missing image error, got %q", err.Error())
	}
}

func TestLoadRejectsUnsafeIdentifiers(t *testing.T) {
	for _, test := range []struct {
		name   string
		config string
		want   string
	}{
		{"service", "service: ../escape\nimage: app\n", "service"},
		{"destination", "service: app\nimage: app\ndestination: prod/blue\n", "destination"},
		{"role", "service: app\nimage: app\nservers:\n  web/api: {}\n", "servers.web/api"},
		{"accessory", "service: app\nimage: app\naccessories:\n  ../db:\n    image: postgres\n", "accessories.../db"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := config.Load(writeConfig(t, "serve.yml", test.config))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Load error = %v, want identifier error containing %q", err, test.want)
			}
		})
	}
}

func TestLoadRejectsSSHOptionAsServerHost(t *testing.T) {
	path := writeConfig(t, "serve.yml", `
service: app
image: app
servers:
  web:
    hosts:
      - -oProxyCommand=touch /tmp/local-pwned
`)

	_, err := config.Load(path)

	if err == nil || !strings.Contains(err.Error(), "servers.web.hosts") {
		t.Fatalf("Load error = %v, want unsafe host validation error", err)
	}
}

func TestLoadAcceptsStandardSSHDestinations(t *testing.T) {
	path := writeConfig(t, "serve.yml", `
service: app
image: app
servers:
  web:
    hosts:
      - deploy@app.example.com
      - host_alias
      - "[2001:db8::1]"
`)

	if _, err := config.Load(path); err != nil {
		t.Fatalf("expected standard SSH destinations to validate: %v", err)
	}
}

func TestLoadRejectsInvalidHealthPolicyNumbers(t *testing.T) {
	path := writeConfig(t, "serve.yml", `
service: app
image: app
servers:
  web:
    healthcheck:
      interval: -1s
      timeout: -2s
      retries: -1
`)

	_, err := config.Load(path)

	if err == nil {
		t.Fatal("expected invalid health policy error")
	}
	for _, want := range []string{"healthcheck.interval", "healthcheck.timeout", "healthcheck.retries"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %s", err, want)
		}
	}
}

func TestLoadRejectsInvalidRestartPolicy(t *testing.T) {
	path := writeConfig(t, "serve.yml", `
service: my-app
image: ghcr.io/acme/my-app
servers:
  web:
    restart:
      policy: sometimes
`)

	_, err := config.Load(path)

	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	if !strings.Contains(err.Error(), "servers.web.restart.policy") {
		t.Fatalf("expected error to identify invalid restart policy path, got %q", err.Error())
	}
}

func TestLoadRejectsAgentRestartControlsWithDockerController(t *testing.T) {
	for _, field := range []string{"initial_backoff", "max_backoff", "window"} {
		t.Run(field, func(t *testing.T) {
			path := writeConfig(t, "serve.yml", fmt.Sprintf(`
service: app
image: app
servers:
  web:
    restart:
      controller: docker
      policy: always
      %s: 1s
`, field))

			_, err := config.Load(path)

			want := field + " is only supported by the agent restart controller"
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("Load error = %v, want %q", err, want)
			}
		})
	}
}

func TestLoadRejectsDockerMaxAttemptsWithoutOnFailurePolicy(t *testing.T) {
	path := writeConfig(t, "serve.yml", `
service: app
image: app
servers:
  web:
    restart:
      controller: docker
      policy: always
      max_attempts: 3
`)

	_, err := config.Load(path)

	if err == nil || !strings.Contains(err.Error(), "max_attempts requires policy on-failure") {
		t.Fatalf("Load error = %v, want unsupported Docker max attempts error", err)
	}
}

func TestLoadRejectsNegativeRestartControls(t *testing.T) {
	path := writeConfig(t, "serve.yml", `
service: app
image: app
servers:
  web:
    restart:
      initial_backoff: -1s
      max_backoff: -2s
      max_attempts: -3
      window: -4s
`)

	_, err := config.Load(path)

	if err == nil {
		t.Fatal("expected negative restart controls to be rejected")
	}
	for _, field := range []string{"initial_backoff", "max_backoff", "max_attempts", "window"} {
		if !strings.Contains(err.Error(), "servers.web.restart."+field) {
			t.Fatalf("error %q does not mention restart field %s", err, field)
		}
	}
}

func TestLoadRejectsNegativeReplicas(t *testing.T) {
	path := writeConfig(t, "serve.yml", `
service: app
image: app
servers:
  web:
    replicas: -1
`)

	_, err := config.Load(path)

	if err == nil || !strings.Contains(err.Error(), "servers.web.replicas") {
		t.Fatalf("Load error = %v, want negative replicas error", err)
	}
}

func TestLoadRequiresAccessoryImage(t *testing.T) {
	path := writeConfig(t, "serve.yml", `
service: app
image: app
accessories:
  database:
    hosts: [app.example.com]
`)

	_, err := config.Load(path)

	if err == nil || !strings.Contains(err.Error(), "accessories.database.image") {
		t.Fatalf("Load error = %v, want missing accessory image error", err)
	}
}

func TestLoadDefaultsRestartControllerToAgent(t *testing.T) {
	path := writeConfig(t, "serve.yml", `
service: my-app
image: ghcr.io/acme/my-app
servers:
  web:
    restart:
      policy: always
`)

	cfg, err := config.Load(path)

	if err != nil {
		t.Fatalf("expected valid config, got error: %v", err)
	}
	if cfg.Servers["web"].Restart.Controller != "agent" {
		t.Fatalf("expected restart controller %q, got %q", "agent", cfg.Servers["web"].Restart.Controller)
	}
}

func TestLoadDefaultsPrivateNetworkAndRetainContainers(t *testing.T) {
	path := writeConfig(t, "serve.yml", `
service: my-app
image: ghcr.io/acme/my-app
`)

	cfg, err := config.Load(path)

	if err != nil {
		t.Fatalf("expected valid config, got error: %v", err)
	}
	if cfg.Networking.PrivateNetwork != "serve" {
		t.Fatalf("expected private network %q, got %q", "serve", cfg.Networking.PrivateNetwork)
	}
	if cfg.RetainContainers != 5 {
		t.Fatalf("expected retain_containers %d, got %d", 5, cfg.RetainContainers)
	}
}

func TestLoadMergesDestinationOverlay(t *testing.T) {
	dir := t.TempDir()
	basePath := filepath.Join(dir, "serve.yml")
	writeFile(t, basePath, `
service: my-app
image: ghcr.io/acme/my-app
destination: production
servers:
  web:
    hosts:
      - base.example.com
    restart:
      policy: always
env:
  clear:
    RACK_ENV: staging
`)
	writeFile(t, filepath.Join(dir, "serve.production.yml"), `
servers:
  web:
    hosts:
      - prod.example.com
    restart:
      max_attempts: 3
env:
  clear:
    RACK_ENV: production
    FEATURE_FLAG: enabled
`)

	cfg, err := config.Load(basePath, config.WithDestination("production"))

	if err != nil {
		t.Fatalf("expected valid merged config, got error: %v", err)
	}
	web := cfg.Servers["web"]
	if len(web.Hosts) != 1 || web.Hosts[0] != "prod.example.com" {
		t.Fatalf("expected overlay hosts to replace base hosts, got %#v", web.Hosts)
	}
	if web.Restart.Policy != "always" {
		t.Fatalf("expected base restart policy to be preserved, got %q", web.Restart.Policy)
	}
	if web.Restart.MaxAttempts != 3 {
		t.Fatalf("expected overlay restart max attempts 3, got %d", web.Restart.MaxAttempts)
	}
	if cfg.Env.Clear["RACK_ENV"] != "production" {
		t.Fatalf("expected overlay env value, got %q", cfg.Env.Clear["RACK_ENV"])
	}
	if cfg.Env.Clear["FEATURE_FLAG"] != "enabled" {
		t.Fatalf("expected overlay env key to be merged, got %q", cfg.Env.Clear["FEATURE_FLAG"])
	}
}

func writeConfig(t *testing.T, name string, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	writeFile(t, path, contents)
	return path
}

func writeFile(t *testing.T, path string, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.TrimPrefix(contents, "\n")), 0o644); err != nil {
		t.Fatalf("write test file: %v", err)
	}
}
