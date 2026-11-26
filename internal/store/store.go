// Package store persists the daemon's state as a single JSON document.
//
// The state is small (app configs and deployment metadata), so every update
// rewrites the file atomically. That keeps the on-disk format trivially
// inspectable and makes updates transactional: a failed update leaves both the
// in-memory and on-disk state untouched.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/egoist/saba/internal/api"
)

// AppRecord is the persisted form of an app: the public view plus secrets.
type AppRecord struct {
	api.App
	Env map[string]string `json:"env,omitempty"`
	// EnvSnapshots holds the environment each kept version was deployed with,
	// so rollbacks restore configuration as well as code.
	EnvSnapshots map[int]map[string]string `json:"env_snapshots,omitempty"`
	// Outputs are values other apps can reference as ${{name.KEY}}.
	Outputs map[string]string `json:"outputs,omitempty"`
}

// Deployment returns the deployment with the given version, or nil.
func (a *AppRecord) Deployment(version int) *api.Deployment {
	for _, d := range a.Deployments {
		if d.Version == version {
			return d
		}
	}
	return nil
}

// Current returns the live deployment, or nil.
func (a *AppRecord) Current() *api.Deployment {
	if a.CurrentVersion == 0 {
		return nil
	}
	return a.Deployment(a.CurrentVersion)
}

type State struct {
	SchemaVersion int                   `json:"schema_version"`
	Server        api.ServerConfig      `json:"server"`
	Apps          map[string]*AppRecord `json:"apps"`
}

func (s *State) clone() (*State, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	var out State
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	if out.Apps == nil {
		out.Apps = map[string]*AppRecord{}
	}
	return &out, nil
}

type Store struct {
	path  string
	mu    sync.RWMutex
	state *State
}

func Open(path string) (*Store, error) {
	s := &Store{path: path, state: &State{SchemaVersion: 1, Apps: map[string]*AppRecord{}}}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return s, nil
	case err != nil:
		return nil, err
	}
	if err := json.Unmarshal(b, s.state); err != nil {
		return nil, fmt.Errorf("state file %s is corrupt: %w", path, err)
	}
	if s.state.Apps == nil {
		s.state.Apps = map[string]*AppRecord{}
	}
	return s, nil
}

// Update applies fn to a copy of the state and commits it if fn succeeds.
func (s *Store) Update(fn func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next, err := s.state.clone()
	if err != nil {
		return err
	}
	if err := fn(next); err != nil {
		return err
	}
	if err := s.write(next); err != nil {
		return err
	}
	s.state = next
	return nil
}

func (s *Store) write(st *State) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Snapshot returns a deep copy of the whole state.
func (s *Store) Snapshot() *State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, err := s.state.clone()
	if err != nil {
		panic(err) // state is always JSON-serializable
	}
	return c
}

// App returns a deep copy of one app.
func (s *Store) App(name string) (*AppRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.state.Apps[name]
	if !ok {
		return nil, NotFound(name)
	}
	b, _ := json.Marshal(a)
	var out AppRecord
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateApp applies fn to one existing app.
func (s *Store) UpdateApp(name string, fn func(*AppRecord) error) error {
	return s.Update(func(st *State) error {
		a, ok := st.Apps[name]
		if !ok {
			return NotFound(name)
		}
		if err := fn(a); err != nil {
			return err
		}
		a.UpdatedAt = time.Now().UTC()
		return nil
	})
}

// AppNames returns all app names, sorted.
func (st *State) AppNames() []string {
	names := make([]string, 0, len(st.Apps))
	for n := range st.Apps {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func NotFound(name string) *api.Error {
	return api.Errorf(api.CodeNotFound, "app %q not found", name).
		WithHint("Run `saba apps` to list apps, or `saba deploy` to create one.")
}
