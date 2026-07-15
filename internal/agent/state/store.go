package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/uptimenine/serve/internal/planner"
)

type Store struct {
	dir string
}

var identityPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

type ActualState struct {
	Service     string            `json:"service"`
	Destination string            `json:"destination"`
	Containers  []ActualContainer `json:"containers"`
}

type ActualContainer struct {
	Name    string `json:"name"`
	Role    string `json:"role"`
	Version string `json:"version"`
	Status  string `json:"status"`
}

func NewStore(dir string) *Store {
	return &Store{dir: dir}
}

func (s *Store) Dir() string {
	return s.dir
}

func (s *Store) SaveDesired(desired planner.DesiredState) error {
	if err := ValidateIdentity(desired.Service, desired.Destination); err != nil {
		return err
	}
	return s.saveJSON(statePath(s.dir, desired.Service, desired.Destination, "desired"), desired)
}

func (s *Store) LoadDesired(service string, destination string) (planner.DesiredState, error) {
	if err := ValidateIdentity(service, destination); err != nil {
		return planner.DesiredState{}, err
	}
	var desired planner.DesiredState
	if err := loadJSON(statePath(s.dir, service, destination, "desired"), &desired); err != nil {
		return planner.DesiredState{}, err
	}
	return desired, nil
}

// ListDesired loads every desired state saved in the store directory.
func (s *Store) ListDesired() ([]planner.DesiredState, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read state directory: %w", err)
	}

	var states []planner.DesiredState
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".desired.json") {
			continue
		}
		var desired planner.DesiredState
		if err := loadJSON(filepath.Join(s.dir, entry.Name()), &desired); err != nil {
			return nil, err
		}
		if err := ValidateIdentity(desired.Service, desired.Destination); err != nil {
			return nil, fmt.Errorf("invalid desired state %q: %w", entry.Name(), err)
		}
		states = append(states, desired)
	}
	return states, nil
}

func (s *Store) SaveActual(actual ActualState) error {
	if err := ValidateIdentity(actual.Service, actual.Destination); err != nil {
		return err
	}
	return s.saveJSON(statePath(s.dir, actual.Service, actual.Destination, "actual"), actual)
}

func (s *Store) LoadActual(service string, destination string) (ActualState, error) {
	if err := ValidateIdentity(service, destination); err != nil {
		return ActualState{}, err
	}
	var actual ActualState
	path := statePath(s.dir, service, destination, "actual")
	if err := loadJSON(path, &actual); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ActualState{Service: service, Destination: destination}, nil
		}
		return ActualState{}, err
	}
	return actual, nil
}

func (s *Store) SaveLastGood(desired planner.DesiredState) error {
	if err := ValidateIdentity(desired.Service, desired.Destination); err != nil {
		return err
	}
	return s.saveJSON(statePath(s.dir, desired.Service, desired.Destination, "last-good"), desired)
}

func (s *Store) LoadLastGood(service string, destination string) (planner.DesiredState, error) {
	if err := ValidateIdentity(service, destination); err != nil {
		return planner.DesiredState{}, err
	}
	var desired planner.DesiredState
	if err := loadJSON(statePath(s.dir, service, destination, "last-good"), &desired); err != nil {
		return planner.DesiredState{}, err
	}
	return desired, nil
}

func (s *Store) saveJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary state file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	encoder := json.NewEncoder(tmp)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		tmp.Close()
		return fmt.Errorf("encode state file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary state file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace state file: %w", err)
	}

	return nil
}

func loadJSON(path string, target any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	if err := json.NewDecoder(file).Decode(target); err != nil {
		return fmt.Errorf("decode state file %q: %w", path, err)
	}
	return nil
}

func statePath(dir string, service string, destination string, kind string) string {
	return filepath.Join(dir, fmt.Sprintf("%s.%s.%s.json", service, destination, kind))
}

func ValidateIdentity(service string, destination string) error {
	if !identityPattern.MatchString(service) {
		return fmt.Errorf("invalid service identifier %q", service)
	}
	if !identityPattern.MatchString(destination) {
		return fmt.Errorf("invalid destination identifier %q", destination)
	}
	return nil
}
