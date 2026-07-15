package planner_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/uptimenine/serve/internal/config"
	"github.com/uptimenine/serve/internal/planner"
)

func TestPlanCreatesOneWebContainerPerReplicaWithLabels(t *testing.T) {
	cfg := baseConfig()
	cfg.Servers = map[string]config.ServerConfig{
		"web": {
			Hosts:    []string{"app1.example.com"},
			Command:  "./server",
			AppPort:  3000,
			Replicas: 2,
			Restart:  config.RestartConfig{Policy: "always", Controller: "agent"},
		},
	}

	state, err := planner.Plan(cfg, planner.Options{
		Host:       "app1.example.com",
		Version:    "abc123",
		SecretsRef: "sops:serve.secrets.yml",
	})

	if err != nil {
		t.Fatalf("expected plan, got error: %v", err)
	}
	if state.Service != "my-app" || state.Destination != "production" || state.Host != "app1.example.com" {
		t.Fatalf("unexpected desired-state identity: %#v", state)
	}
	if state.Version != "abc123" || state.Network != "serve" || state.RetainContainers != 5 {
		t.Fatalf("unexpected desired-state metadata: %#v", state)
	}
	if len(state.Containers) != 2 {
		t.Fatalf("expected 2 containers, got %d: %#v", len(state.Containers), state.Containers)
	}

	first := state.Containers[0]
	if first.Name != "my-app-web-production-abc123-r1" {
		t.Fatalf("expected deterministic name for first replica, got %q", first.Name)
	}
	if first.Image != "ghcr.io/acme/my-app:abc123" {
		t.Fatalf("expected image tagged with version, got %q", first.Image)
	}
	if !reflect.DeepEqual(first.Command, []string{"./server"}) {
		t.Fatalf("expected command split into argv, got %#v", first.Command)
	}
	if len(first.Ports) != 1 || first.Ports[0].Name != "http" || first.Ports[0].ContainerPort != 3000 {
		t.Fatalf("expected HTTP app port, got %#v", first.Ports)
	}
	if !first.Proxy {
		t.Fatal("expected web container to be marked as a proxy target")
	}

	expectedLabels := map[string]string{
		"serve.managed":        "true",
		"serve.service":        "my-app",
		"serve.destination":    "production",
		"serve.role":           "web",
		"serve.version":        "abc123",
		"serve.replica":        "1",
		"serve.container_type": "app",
	}
	if !reflect.DeepEqual(first.Labels, expectedLabels) {
		t.Fatalf("expected labels %#v, got %#v", expectedLabels, first.Labels)
	}

	second := state.Containers[1]
	if second.Name != "my-app-web-production-abc123-r2" || second.Replica != 2 {
		t.Fatalf("expected deterministic second replica, got %#v", second)
	}
}

func TestPlanRejectsVersionThatCannotBeUsedInAContainerName(t *testing.T) {
	_, err := planner.Plan(baseConfig(), planner.Options{Host: "app1.example.com", Version: "feature/bad"})

	if err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("Plan error = %v, want invalid version error", err)
	}
}

func TestPlanIncludesOnlyRolesAndAccessoriesForHost(t *testing.T) {
	cfg := baseConfig()
	cfg.Servers = map[string]config.ServerConfig{
		"web": {
			Hosts:    []string{"app1.example.com"},
			Command:  "./server",
			AppPort:  3000,
			Replicas: 1,
		},
		"worker": {
			Hosts:    []string{"worker1.example.com"},
			Command:  "./worker",
			Replicas: 1,
		},
	}
	cfg.Accessories = map[string]config.AccessoryConfig{
		"postgres": {
			Image: "postgres:16",
			Hosts: []string{"worker1.example.com"},
		},
		"redis": {
			Image:        "redis:7",
			Hosts:        []string{"app1.example.com"},
			Aliases:      []string{"cache"},
			InternalPort: 6379,
			Volumes:      []string{"redis-data:/data"},
		},
	}

	state, err := planner.Plan(cfg, planner.Options{Host: "app1.example.com", Version: "abc123"})

	if err != nil {
		t.Fatalf("expected plan, got error: %v", err)
	}
	if len(state.Containers) != 2 {
		t.Fatalf("expected web app plus redis accessory, got %#v", state.Containers)
	}
	if state.Containers[0].Role != "web" {
		t.Fatalf("expected web role first, got %#v", state.Containers[0])
	}
	redis := state.Containers[1]
	if redis.Role != "redis" || redis.ContainerType != "accessory" {
		t.Fatalf("expected redis accessory, got %#v", redis)
	}
	if redis.Image != "redis:7" {
		t.Fatalf("expected accessory image unchanged, got %q", redis.Image)
	}
	if !reflect.DeepEqual(redis.Aliases, []string{"cache"}) {
		t.Fatalf("expected accessory aliases, got %#v", redis.Aliases)
	}
	if !reflect.DeepEqual(redis.Volumes, []string{"redis-data:/data"}) {
		t.Fatalf("expected accessory volumes, got %#v", redis.Volumes)
	}
	for _, container := range state.Containers {
		if container.Role == "worker" || container.Role == "postgres" {
			t.Fatalf("did not expect off-host container in desired state: %#v", container)
		}
	}
}

func TestPlanEmbedsSecretCiphertextReferenceWithoutPlaintext(t *testing.T) {
	cfg := baseConfig()
	cfg.Servers = map[string]config.ServerConfig{
		"web": {
			Hosts:    []string{"app1.example.com"},
			Command:  "./server",
			Replicas: 1,
		},
	}
	cfg.Env.Secret = []string{"DATABASE_URL", "SECRET_KEY_BASE"}
	cfg.Secrets.Provider = "sops"
	plaintext := "postgres://user:password@db/prod"

	state, err := planner.Plan(cfg, planner.Options{
		Host:       "app1.example.com",
		Version:    "abc123",
		SecretsRef: "sops:serve.secrets.yml",
		SecretCiphertext: map[string]string{
			"DATABASE_URL":    "ENC[AES256_GCM,data:database-ciphertext]",
			"SECRET_KEY_BASE": "ENC[AES256_GCM,data:key-ciphertext]",
		},
	})

	if err != nil {
		t.Fatalf("expected plan, got error: %v", err)
	}
	container := state.Containers[0]
	if container.SecretsRef != "sops:serve.secrets.yml" {
		t.Fatalf("expected secrets_ref, got %q", container.SecretsRef)
	}
	if !reflect.DeepEqual(container.SecretNames, []string{"DATABASE_URL", "SECRET_KEY_BASE"}) {
		t.Fatalf("expected secret names only, got %#v", container.SecretNames)
	}
	if container.SecretCiphertext["DATABASE_URL"] != "ENC[AES256_GCM,data:database-ciphertext]" {
		t.Fatalf("expected DATABASE_URL ciphertext, got %#v", container.SecretCiphertext)
	}

	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal desired state: %v", err)
	}
	if strings.Contains(string(encoded), plaintext) {
		t.Fatalf("desired state must not contain plaintext secret values: %s", encoded)
	}
}

func TestPlanEmbedsEncryptedSecretsFileForRemoteDecryption(t *testing.T) {
	cfg := baseConfig()
	cfg.Servers = map[string]config.ServerConfig{
		"web": {Hosts: []string{"app1.example.com"}, Command: "./server", Replicas: 1},
	}
	cfg.Env.Secret = []string{"DATABASE_URL"}
	encryptedFile := "DATABASE_URL: ENC[AES256_GCM,data:database-ciphertext]\nsops:\n  kms: []\n"

	state, err := planner.Plan(cfg, planner.Options{
		Host:               "app1.example.com",
		Version:            "abc123",
		SecretsFileContent: encryptedFile,
	})

	if err != nil {
		t.Fatalf("expected plan, got error: %v", err)
	}
	if state.SecretsFile != encryptedFile {
		t.Fatalf("expected desired state to embed the encrypted secrets file, got %q", state.SecretsFile)
	}
}

func TestPlanOmitsSecretsFileWhenNoSecretsConfigured(t *testing.T) {
	cfg := baseConfig()
	cfg.Servers = map[string]config.ServerConfig{
		"web": {Hosts: []string{"app1.example.com"}, Command: "./server", Replicas: 1},
	}

	state, err := planner.Plan(cfg, planner.Options{
		Host:               "app1.example.com",
		Version:            "abc123",
		SecretsFileContent: "should-not-be-embedded",
	})

	if err != nil {
		t.Fatalf("expected plan, got error: %v", err)
	}
	if state.SecretsFile != "" {
		t.Fatalf("secrets file must not be embedded without env.secret, got %q", state.SecretsFile)
	}
}

func TestPlanMapsProxyRouteFromConfig(t *testing.T) {
	cfg := baseConfig()
	cfg.Servers = map[string]config.ServerConfig{
		"web": {Hosts: []string{"app1.example.com"}, Command: "./server", Replicas: 1},
	}
	cfg.Proxy.Hosts = []string{"app.example.com"}
	cfg.Proxy.SSL = "auto"
	cfg.Proxy.DeployTimeout = config.Duration{Duration: 45 * time.Second}
	cfg.Proxy.DrainTimeout = config.Duration{Duration: 60 * time.Second}

	state, err := planner.Plan(cfg, planner.Options{Host: "app1.example.com", Version: "abc123"})

	if err != nil {
		t.Fatalf("expected plan, got error: %v", err)
	}
	if len(state.Proxy.Hosts) != 1 || state.Proxy.Hosts[0] != "app.example.com" {
		t.Fatalf("proxy hosts = %#v", state.Proxy.Hosts)
	}
	if !state.Proxy.SSL {
		t.Fatalf("expected ssl auto to map to SSL=true, got %#v", state.Proxy)
	}
	if state.Proxy.DeployTimeout != "45s" || state.Proxy.DrainTimeout != "1m0s" {
		t.Fatalf("proxy timeouts = %q/%q", state.Proxy.DeployTimeout, state.Proxy.DrainTimeout)
	}
}

func TestResolveVersionMarksDirtyTree(t *testing.T) {
	version, err := planner.ResolveVersion(planner.GitState{SHA: "abc123", Dirty: true}, func() string { return "rand42" })

	if err != nil {
		t.Fatalf("expected version, got error: %v", err)
	}
	if version != "abc123_uncommitted_rand42" {
		t.Fatalf("expected dirty version suffix, got %q", version)
	}
}

func baseConfig() config.Config {
	return config.Config{
		Service:          "my-app",
		Image:            "ghcr.io/acme/my-app",
		Destination:      "production",
		Networking:       config.NetworkingConfig{PrivateNetwork: "serve"},
		RetainContainers: 5,
		Proxy:            config.ProxyConfig{AppRole: "web"},
		Env:              config.EnvConfig{Clear: map[string]string{"RACK_ENV": "production"}},
	}
}
