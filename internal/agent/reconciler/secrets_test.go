package reconciler_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/uptimenine/serve/internal/agent/reconciler"
	"github.com/uptimenine/serve/internal/agent/secrets"
	secretfake "github.com/uptimenine/serve/internal/agent/secrets/fake"
	"github.com/uptimenine/serve/internal/runtime"
	"github.com/uptimenine/serve/internal/runtime/fake"
)

func TestReconcileUsesPrivateEnvFileOnlyWhileRuntimeCreatesContainer(t *testing.T) {
	rt := newEnvCapturingRuntime()
	envDir := t.TempDir()
	store := secretfake.NewStore(map[string]string{
		"DATABASE_URL":    "postgres://user:password@db/prod",
		"SECRET_KEY_BASE": "super-secret-key",
	})
	desired := desiredState("abc123")
	desired.Containers[0].SecretsRef = "sops:serve.secrets.yml"
	desired.Containers[0].SecretNames = []string{"DATABASE_URL", "SECRET_KEY_BASE"}
	desired.Containers[0].SecretCiphertext = map[string]string{
		"DATABASE_URL":    "ENC[AES256_GCM,data:database]",
		"SECRET_KEY_BASE": "ENC[AES256_GCM,data:key]",
	}

	_, err := reconciler.NewWithSecrets(rt, store, secrets.NewEnvFileWriter(envDir)).Reconcile(context.Background(), desired)

	if err != nil {
		t.Fatalf("reconcile desired state: %v", err)
	}
	spec, ok := rt.CreatedSpec(desired.Containers[0].Name)
	if !ok || len(spec.EnvFiles) != 1 {
		t.Fatalf("expected one env file to reach runtime, got %#v", spec.EnvFiles)
	}
	envFile := spec.EnvFiles[0]
	contents := rt.capturedContents(envFile)
	if !strings.Contains(contents, "DATABASE_URL=postgres://user:password@db/prod\n") {
		t.Fatalf("expected resolved DATABASE_URL in env file, got %q", contents)
	}
	if !strings.Contains(contents, "SECRET_KEY_BASE=super-secret-key\n") {
		t.Fatalf("expected resolved SECRET_KEY_BASE in env file, got %q", contents)
	}
	if _, err := os.Stat(envFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plaintext env file still exists after container create: %v", err)
	}

	containers, err := rt.ListContainers(context.Background(), runtime.ContainerFilters{Labels: map[string]string{"serve.managed": "true"}})
	if err != nil {
		t.Fatalf("list containers: %v", err)
	}
	if len(containers) != 1 {
		t.Fatalf("expected one container, got %#v", containers)
	}
	if strings.Contains(strings.Join(containers[0].Command, " "), "super-secret-key") {
		t.Fatalf("secret leaked into container command: %#v", containers[0].Command)
	}
}

func TestReconcileAbortsCreateWhenSecretResolutionFails(t *testing.T) {
	rt := fake.NewRuntime()
	envDir := t.TempDir()
	store := secretfake.NewFailingStore("kms decrypt failed")
	desired := desiredState("abc123")
	desired.Containers[0].SecretsRef = "sops:serve.secrets.yml"
	desired.Containers[0].SecretNames = []string{"DATABASE_URL"}
	desired.Containers[0].SecretCiphertext = map[string]string{"DATABASE_URL": "ENC[AES256_GCM,data:database]"}

	_, err := reconciler.NewWithSecrets(rt, store, secrets.NewEnvFileWriter(envDir)).Reconcile(context.Background(), desired)

	if err == nil {
		t.Fatal("expected secret resolution error, got nil")
	}
	if !strings.Contains(err.Error(), "kms decrypt failed") {
		t.Fatalf("expected decrypt error, got %v", err)
	}
	containers, listErr := rt.ListContainers(context.Background(), runtime.ContainerFilters{Labels: map[string]string{"serve.managed": "true"}})
	if listErr != nil {
		t.Fatalf("list containers: %v", listErr)
	}
	if len(containers) != 0 {
		t.Fatalf("expected no container created after secret failure, got %#v", containers)
	}
	entries, readErr := os.ReadDir(envDir)
	if readErr != nil {
		t.Fatalf("read env dir: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no partial env file after secret failure, got %#v", entries)
	}
}

func TestReconcileRematerializesSecretFileWhenRecreatingContainer(t *testing.T) {
	rt := newEnvCapturingRuntime()
	envDir := t.TempDir()
	store := secretfake.NewStore(map[string]string{"DATABASE_URL": "postgres://user:password@db/prod"})
	desired := desiredState("abc123")
	desired.Containers[0].SecretsRef = "sops:serve.secrets.yml"
	desired.Containers[0].SecretNames = []string{"DATABASE_URL"}
	desired.Containers[0].SecretCiphertext = map[string]string{"DATABASE_URL": "ENC[AES256_GCM,data:database]"}
	reconcile := reconciler.NewWithSecrets(rt, store, secrets.NewEnvFileWriter(envDir))

	if _, err := reconcile.Reconcile(context.Background(), desired); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}
	containers, err := rt.ListContainers(context.Background(), runtime.ContainerFilters{Labels: map[string]string{"serve.managed": "true"}})
	if err != nil {
		t.Fatalf("list containers: %v", err)
	}
	if err := rt.RemoveContainer(context.Background(), containers[0].ID); err != nil {
		t.Fatalf("remove container to force recreate: %v", err)
	}

	if _, err := reconcile.Reconcile(context.Background(), desired); err != nil {
		t.Fatalf("reconcile after container removal: %v", err)
	}

	if captured := rt.capturedFiles(); len(captured) != 2 {
		t.Fatalf("expected a fresh secret file for each create, got %#v", captured)
	}
	for path, contents := range rt.capturedFiles() {
		if !strings.Contains(contents, "DATABASE_URL=postgres://user:password@db/prod\n") {
			t.Fatalf("env file %s did not contain resolved secret: %q", path, contents)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("plaintext env file %s still exists: %v", path, err)
		}
	}
}

func TestReconcilePassesEmbeddedSecretsFileToStore(t *testing.T) {
	rt := fake.NewRuntime()
	envDir := t.TempDir()
	store := secretfake.NewStore(map[string]string{"DATABASE_URL": "postgres://db"})
	desired := desiredState("abc123")
	desired.SecretsFile = "DATABASE_URL: ENC[AES256_GCM,data:database]\n"
	desired.Containers[0].SecretsRef = "sops:serve.secrets.yml"
	desired.Containers[0].SecretNames = []string{"DATABASE_URL"}

	_, err := reconciler.NewWithSecrets(rt, store, secrets.NewEnvFileWriter(envDir)).Reconcile(context.Background(), desired)

	if err != nil {
		t.Fatalf("reconcile desired state: %v", err)
	}
	request := store.LastRequest()
	if request.EncryptedFile != desired.SecretsFile {
		t.Fatalf("expected embedded secrets file to reach the store, got %#v", request)
	}
	if request.Ref != "sops:serve.secrets.yml" {
		t.Fatalf("expected secrets ref to reach the store, got %#v", request)
	}
}

type envCapturingRuntime struct {
	*fake.Runtime
	mu       sync.Mutex
	captured map[string]string
}

func newEnvCapturingRuntime() *envCapturingRuntime {
	return &envCapturingRuntime{Runtime: fake.NewRuntime(), captured: map[string]string{}}
}

func (r *envCapturingRuntime) CreateContainer(ctx context.Context, spec runtime.ContainerSpec) (runtime.ContainerID, error) {
	for _, path := range spec.EnvFiles {
		contents, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		r.mu.Lock()
		r.captured[path] = string(contents)
		r.mu.Unlock()
	}
	return r.Runtime.CreateContainer(ctx, spec)
}

func (r *envCapturingRuntime) capturedContents(path string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.captured[path]
}

func (r *envCapturingRuntime) capturedFiles() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	files := make(map[string]string, len(r.captured))
	for path, contents := range r.captured {
		files[path] = contents
	}
	return files
}
