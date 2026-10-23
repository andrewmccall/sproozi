package modelauth

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"

	"github.com/andrewmccall/sproozi/internal/privatefs"
)

// FileSessionStore keeps a gateway-held ChatGPT sign-in in one owner-private
// file, for a local gateway that is the session's only refresh owner (ADR
// 0047). It reads and writes through privatefs, which checks ownership and
// access on every platform, including Windows ACLs. Replace writes only when
// the file still holds the session the refresh started from.
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
	return s.read()
}

// Save writes a session, such as one a sign-in just obtained.
func (s FileSessionStore) Save(session Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.write(session)
}

func (s FileSessionStore) read() (Session, error) {
	var session Session
	data, err := privatefs.ReadFile(s.Path, 1<<20)
	if err != nil {
		return session, err
	}
	if json.Unmarshal(data, &session) != nil {
		return session, errors.New("invalid ChatGPT credential file")
	}
	return session, session.Validate()
}

func (s FileSessionStore) write(session Session) error {
	if err := session.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(session)
	if err != nil {
		return errors.New("could not encode private credentials")
	}
	return privatefs.Replace(filepath.Dir(s.Path), filepath.Base(s.Path), data)
}

func (s FileSessionStore) Replace(_ context.Context, before, after Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.read()
	if err != nil {
		return err
	}
	if !before.SameCredentials(current) {
		return errors.New("ChatGPT credential set changed during refresh")
	}
	return s.write(after)
}
