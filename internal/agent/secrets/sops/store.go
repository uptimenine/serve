package sops

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/uptimenine/serve/internal/agent/secrets"
)

type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

type Store struct {
	runner Runner
}

func NewStore(runner Runner) *Store {
	return &Store{runner: runner}
}

func NewDefaultStore() *Store {
	return NewStore(ExecRunner{})
}

func (s *Store) Resolve(ctx context.Context, req secrets.Request) (map[string]string, error) {
	path, err := s.pathFromRef(req.Ref)
	if err != nil {
		return nil, err
	}

	// Desired state carries the whole encrypted secrets file so hosts can
	// decrypt without having the repository checkout. Materialize it as a
	// private temp file for the sops binary and remove it afterwards.
	if req.EncryptedFile != "" {
		tmp, err := os.CreateTemp("", "serve-secrets-*.yml")
		if err != nil {
			return nil, fmt.Errorf("create temp secrets file: %w", err)
		}
		tmpPath := tmp.Name()
		defer os.Remove(tmpPath)
		if _, err := tmp.WriteString(req.EncryptedFile); err != nil {
			tmp.Close()
			return nil, fmt.Errorf("write temp secrets file: %w", err)
		}
		if err := tmp.Close(); err != nil {
			return nil, fmt.Errorf("close temp secrets file: %w", err)
		}
		path = tmpPath
	}

	output, err := s.runner.Run(ctx, "sops", "--decrypt", "--output-type", "yaml", path)
	if err != nil {
		return nil, fmt.Errorf("decrypt sops secrets %q: %w", req.Ref, err)
	}

	decoded := map[string]any{}
	if err := yaml.Unmarshal(output, &decoded); err != nil {
		return nil, fmt.Errorf("parse decrypted sops secrets %q: %w", req.Ref, err)
	}

	resolved := make(map[string]string, len(req.Names))
	for _, name := range req.Names {
		value, ok := decoded[name]
		if !ok {
			return nil, fmt.Errorf("secret %s not found in %s", name, req.Ref)
		}
		stringValue, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("secret %s in %s must be a string", name, req.Ref)
		}
		resolved[name] = stringValue
	}
	return resolved, nil
}

func (s *Store) pathFromRef(ref string) (string, error) {
	if !strings.HasPrefix(ref, "sops:") {
		return "", fmt.Errorf("expected sops: secret reference, got %q", ref)
	}
	path := strings.TrimSpace(strings.TrimPrefix(ref, "sops:"))
	if path == "" {
		return "", fmt.Errorf("expected sops: secret reference to include a path")
	}
	return path, nil
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	output, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && len(exitErr.Stderr) > 0 {
			return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, err
	}
	return output, nil
}
