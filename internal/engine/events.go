package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/egoist/saba/internal/api"
)

// EventLog is an append-only, replayable stream of deployment events. Events
// are kept in memory for live followers and mirrored to an NDJSON file so the
// log of any past deployment can be read back later.
type EventLog struct {
	mu     sync.Mutex
	events []api.Event
	done   bool
	wake   chan struct{}
	file   *os.File
	enc    *json.Encoder
}

func newEventLog(path string) *EventLog {
	l := &EventLog{wake: make(chan struct{})}
	if path != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err == nil {
			if f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600); err == nil {
				l.file = f
				l.enc = json.NewEncoder(f)
			}
		}
	}
	return l
}

func (l *EventLog) Append(ev api.Event) {
	if ev.Time.IsZero() {
		ev.Time = time.Now().UTC()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.done {
		return
	}
	l.events = append(l.events, ev)
	if l.enc != nil {
		_ = l.enc.Encode(ev)
	}
	close(l.wake)
	l.wake = make(chan struct{})
}

// Close marks the stream complete and releases followers.
func (l *EventLog) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.done {
		return
	}
	l.done = true
	if l.file != nil {
		l.file.Close()
	}
	close(l.wake)
}

// Stream replays all events to fn and, if follow is set, keeps delivering new
// ones until the log is closed or ctx ends.
func (l *EventLog) Stream(ctx context.Context, follow bool, fn func(api.Event) error) error {
	next := 0
	for {
		l.mu.Lock()
		batch := l.events[next:]
		next = len(l.events)
		done, wake := l.done, l.wake
		l.mu.Unlock()
		for _, ev := range batch {
			if err := fn(ev); err != nil {
				return err
			}
		}
		if len(batch) > 0 {
			continue
		}
		if done || !follow {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wake:
		}
	}
}

// readEventFile replays a finished deployment's event file.
func readEventFile(path string, fn func(api.Event) error) error {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return api.Errorf(api.CodeNotFound, "no logs are stored for this deployment")
		}
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var ev api.Event
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		if err := fn(ev); err != nil {
			return err
		}
	}
	return sc.Err()
}
