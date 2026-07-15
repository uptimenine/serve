package fake

import (
	"context"
	"fmt"

	"github.com/uptimenine/serve/internal/agent/secrets"
)

type Store struct {
	values      map[string]string
	err         error
	lastRequest secrets.Request
}

func NewStore(values map[string]string) *Store {
	copy := make(map[string]string, len(values))
	for key, value := range values {
		copy[key] = value
	}
	return &Store{values: copy}
}

func NewFailingStore(message string) *Store {
	return &Store{err: fmt.Errorf("%s", message)}
}

func (s *Store) Resolve(ctx context.Context, req secrets.Request) (map[string]string, error) {
	_ = ctx
	s.lastRequest = req
	if s.err != nil {
		return nil, s.err
	}

	resolved := make(map[string]string, len(req.Names))
	for _, name := range req.Names {
		value, ok := s.values[name]
		if !ok {
			return nil, fmt.Errorf("secret %s not found", name)
		}
		resolved[name] = value
	}
	return resolved, nil
}

func (s *Store) LastRequest() secrets.Request {
	return s.lastRequest
}
