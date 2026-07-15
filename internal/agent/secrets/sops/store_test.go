package sops_test

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/uptimenine/serve/internal/agent/secrets"
	"github.com/uptimenine/serve/internal/agent/secrets/sops"
)

func TestStoreDecryptsSOPSReferenceAndReturnsRequestedSecrets(t *testing.T) {
	runner := &recordingRunner{output: []byte(`
DATABASE_URL: postgres://user:password@db/prod
SECRET_KEY_BASE: super-secret-key
UNUSED: not-requested
`)}
	store := sops.NewStore(runner)

	values, err := store.Resolve(context.Background(), secrets.Request{
		Ref:   "sops:/etc/serve/serve.secrets.yml",
		Names: []string{"DATABASE_URL", "SECRET_KEY_BASE"},
		Ciphertext: map[string]string{
			"DATABASE_URL": "ENC[AES256_GCM,data:database-ciphertext]",
		},
	})

	if err != nil {
		t.Fatalf("resolve sops secrets: %v", err)
	}
	expected := map[string]string{
		"DATABASE_URL":    "postgres://user:password@db/prod",
		"SECRET_KEY_BASE": "super-secret-key",
	}
	if !reflect.DeepEqual(values, expected) {
		t.Fatalf("expected values %#v, got %#v", expected, values)
	}
	if runner.name != "sops" {
		t.Fatalf("expected sops command, got %q", runner.name)
	}
	expectedArgs := []string{"--decrypt", "--output-type", "yaml", "/etc/serve/serve.secrets.yml"}
	if !reflect.DeepEqual(runner.args, expectedArgs) {
		t.Fatalf("expected args %#v, got %#v", expectedArgs, runner.args)
	}
	if strings.Contains(strings.Join(runner.args, " "), "database-ciphertext") {
		t.Fatalf("ciphertext should not be passed on command line, got args %#v", runner.args)
	}
}

func TestStoreDecryptsEmbeddedEncryptedFileViaTempFile(t *testing.T) {
	encryptedFile := "DATABASE_URL: ENC[AES256_GCM,data:database-ciphertext]\nsops:\n  kms: []\n"
	runner := &recordingRunner{output: []byte("DATABASE_URL: postgres://user:password@db/prod\n")}
	store := sops.NewStore(runner)

	values, err := store.Resolve(context.Background(), secrets.Request{
		Ref:           "sops:serve.secrets.yml",
		Names:         []string{"DATABASE_URL"},
		EncryptedFile: encryptedFile,
	})

	if err != nil {
		t.Fatalf("resolve embedded sops secrets: %v", err)
	}
	if values["DATABASE_URL"] != "postgres://user:password@db/prod" {
		t.Fatalf("unexpected values %#v", values)
	}

	// The decrypt must run against a temp file holding the embedded
	// ciphertext, not the (host-local) ref path.
	path := runner.args[len(runner.args)-1]
	if path == "serve.secrets.yml" {
		t.Fatalf("expected decrypt of a temp file, got ref path %q", path)
	}
	if runner.decryptedFileContent != encryptedFile {
		t.Fatalf("temp file content = %q, want embedded ciphertext", runner.decryptedFileContent)
	}
	if runner.decryptedFileMode.Perm() != 0o600 {
		t.Fatalf("temp file mode = %v, want 0600", runner.decryptedFileMode.Perm())
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temp file %q must be removed after decrypt, stat err = %v", path, err)
	}
}

func TestStoreRemovesTempFileWhenDecryptFails(t *testing.T) {
	runner := &recordingRunner{err: errors.New("gcp kms permission denied")}
	store := sops.NewStore(runner)

	_, err := store.Resolve(context.Background(), secrets.Request{
		Ref:           "sops:serve.secrets.yml",
		Names:         []string{"DATABASE_URL"},
		EncryptedFile: "DATABASE_URL: ENC[data]\n",
	})

	if err == nil {
		t.Fatal("expected decrypt failure, got nil")
	}
	path := runner.args[len(runner.args)-1]
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("temp file %q must be removed after failed decrypt, stat err = %v", path, statErr)
	}
}

func TestStoreRejectsNonSOPSReference(t *testing.T) {
	store := sops.NewStore(&recordingRunner{})

	_, err := store.Resolve(context.Background(), secrets.Request{Ref: "age:/etc/serve/serve.secrets.yml", Names: []string{"DATABASE_URL"}})

	if err == nil {
		t.Fatal("expected invalid reference error, got nil")
	}
	if !strings.Contains(err.Error(), "expected sops: secret reference") {
		t.Fatalf("expected clear invalid ref error, got %v", err)
	}
}

func TestStoreFailsWhenRequestedSecretIsMissing(t *testing.T) {
	store := sops.NewStore(&recordingRunner{output: []byte("DATABASE_URL: postgres://db\n")})

	_, err := store.Resolve(context.Background(), secrets.Request{Ref: "sops:/etc/serve/serve.secrets.yml", Names: []string{"SECRET_KEY_BASE"}})

	if err == nil {
		t.Fatal("expected missing secret error, got nil")
	}
	if !strings.Contains(err.Error(), "secret SECRET_KEY_BASE not found") {
		t.Fatalf("expected missing secret name in error, got %v", err)
	}
}

func TestStoreSurfacesDecryptFailure(t *testing.T) {
	store := sops.NewStore(&recordingRunner{err: errors.New("gcp kms permission denied")})

	_, err := store.Resolve(context.Background(), secrets.Request{Ref: "sops:/etc/serve/serve.secrets.yml", Names: []string{"DATABASE_URL"}})

	if err == nil {
		t.Fatal("expected decrypt failure, got nil")
	}
	if !strings.Contains(err.Error(), "decrypt sops secrets") || !strings.Contains(err.Error(), "gcp kms permission denied") {
		t.Fatalf("expected decrypt context and cause, got %v", err)
	}
}

type recordingRunner struct {
	name                 string
	args                 []string
	output               []byte
	err                  error
	decryptedFileContent string
	decryptedFileMode    os.FileMode
}

func (r *recordingRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	_ = ctx
	r.name = name
	r.args = append([]string(nil), args...)
	// Capture the decrypted file's content and mode while it still exists,
	// like the real sops binary would observe it.
	if len(args) > 0 {
		path := args[len(args)-1]
		if contents, err := os.ReadFile(path); err == nil {
			r.decryptedFileContent = string(contents)
		}
		if info, err := os.Stat(path); err == nil {
			r.decryptedFileMode = info.Mode()
		}
	}
	if r.err != nil {
		return nil, r.err
	}
	return append([]byte(nil), r.output...), nil
}
