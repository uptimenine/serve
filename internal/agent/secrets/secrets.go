package secrets

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Request describes one secret resolution. EncryptedFile carries the whole
// encrypted secrets file from the desired state so agents can decrypt on
// hosts that do not have the file on disk; when empty, resolvers fall back
// to the path in Ref (local dev mode).
type Request struct {
	Ref           string
	Names         []string
	Ciphertext    map[string]string
	EncryptedFile string
}

type Store interface {
	Resolve(ctx context.Context, req Request) (map[string]string, error)
}

type EnvFileWriter struct {
	dir string
}

func NewEnvFileWriter(dir string) *EnvFileWriter {
	return &EnvFileWriter{dir: dir}
}

func (w *EnvFileWriter) Write(service string, role string, values map[string]string) (string, error) {
	if len(values) == 0 {
		return "", nil
	}
	if strings.TrimSpace(w.dir) == "" {
		return "", fmt.Errorf("env file directory is required")
	}
	if err := os.MkdirAll(w.dir, 0o700); err != nil {
		return "", fmt.Errorf("create env file directory: %w", err)
	}

	file, err := os.CreateTemp(w.dir, "serve-env-*.env")
	if err != nil {
		return "", fmt.Errorf("create env file: %w", err)
	}
	path := file.Name()
	defer func() {
		if file != nil {
			file.Close()
		}
	}()

	if _, err := file.WriteString(formatEnv(values)); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("write env file: %w", err)
	}
	if err := file.Close(); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("close env file: %w", err)
	}
	file = nil
	return path, nil
}

func formatEnv(values map[string]string) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var builder strings.Builder
	for _, key := range keys {
		builder.WriteString(key)
		builder.WriteString("=")
		builder.WriteString(values[key])
		builder.WriteString("\n")
	}
	return builder.String()
}
