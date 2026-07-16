package secrets_test

import (
	"os"
	"testing"

	"github.com/uptimenine/serve/internal/agent/secrets"
)

func TestEnvFileWriterCreatesUniquePrivateFiles(t *testing.T) {
	writer := secrets.NewEnvFileWriter(t.TempDir())

	first, err := writer.Write("my-app", "web", map[string]string{"TOKEN": "production"})
	if err != nil {
		t.Fatalf("write first env file: %v", err)
	}
	second, err := writer.Write("my-app", "web", map[string]string{"TOKEN": "staging"})
	if err != nil {
		t.Fatalf("write second env file: %v", err)
	}

	if first == second {
		t.Fatalf("concurrent deployments would share env file %q", first)
	}
	for _, path := range []string{first, second} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat env file %s: %v", path, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("env file %s mode = %v, want 0600", path, info.Mode().Perm())
		}
	}
}
