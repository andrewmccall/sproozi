package modelauth

import (
	"context"
	"errors"
	"sync"
)

// FileSessionStore keeps a gateway-held ChatGPT sign-in in one owner-private
// file, for a local gateway that is the session's only refresh owner (ADR
// 0047). Replace writes only when the file still holds the session the
// refresh started from.
type FileSessionStore struct {
	Path string
	mu   *sync.Mutex
}

// NewFileSessionStore returns a store for path.
func NewFileSessionStore(path string) FileSessionStore {
	return FileSessionStore{Path: path, mu: &sync.Mutex{}}
}

func (s FileSessionStore) Load(context.Context) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return LoadFile(s.Path)
}

func (s FileSessionStore) Replace(_ context.Context, before, after Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := LoadFile(s.Path)
	if err != nil {
		return err
	}
	if !before.SameCredentials(current) {
		return errors.New("ChatGPT credential set changed during refresh")
	}
	if err = after.Validate(); err != nil {
		return err
	}
	return WritePrivateJSON(s.Path, after)
}
