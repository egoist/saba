// Package runtime abstracts the container engine saba drives.
package runtime

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"time"
)

// ErrNotFound is returned when a container or image does not exist.
var ErrNotFound = errors.New("not found")

type RunOptions struct {
	Name    string
	Image   string
	Command []string
	Env     map[string]string
	Labels  map[string]string
	Network string
	Aliases []string
	// Ports are publish specs: "127.0.0.1:20001:3000".
	Ports []string
	// Volumes are "source:target" mounts.
	Volumes []string
	Memory  string
	CPUs    string
	// Restart policy ("unless-stopped", "no", ...).
	Restart     string
	StopTimeout time.Duration
}

type BuildOptions struct {
	ContextDir string
	Dockerfile string
	Tag        string
	Target     string
	Args       map[string]string
	Labels     map[string]string
}

type Container struct {
	ID        string
	Name      string
	Image     string
	State     string // created, running, restarting, exited, paused, dead
	Health    string // "", starting, healthy, unhealthy
	ExitCode  int
	OOMKilled bool
	Restarts  int
	StartedAt time.Time
	Labels    map[string]string
	Error     string
}

func (c *Container) Running() bool { return c.State == "running" }

type Image struct {
	ID           string
	ExposedPorts []int
}

type LogOptions struct {
	Tail   int
	Since  string
	Follow bool
}

type LogLine struct {
	Time time.Time
	Text string
}

type Stat struct {
	Container string
	CPU       string
	Memory    string
	MemoryPct string
	NetIO     string
	BlockIO   string
	PIDs      string
}

// Runtime is the set of container operations saba needs.
type Runtime interface {
	Version(ctx context.Context) (string, error)
	EnsureNetwork(ctx context.Context, name string) error

	Pull(ctx context.Context, image string, out io.Writer) error
	Build(ctx context.Context, opts BuildOptions, out io.Writer) error
	InspectImage(ctx context.Context, ref string) (*Image, error)
	TagImage(ctx context.Context, src, dst string) error
	RemoveImage(ctx context.Context, ref string) error

	// Run starts a detached container and returns its ID.
	Run(ctx context.Context, opts RunOptions) (string, error)
	// RunOnce runs a container to completion, removes it and returns its exit code.
	RunOnce(ctx context.Context, opts RunOptions, stdout, stderr io.Writer) (int, error)
	Start(ctx context.Context, name string) error
	// SetRestart changes a container's restart policy.
	SetRestart(ctx context.Context, name, policy string) error
	Stop(ctx context.Context, name string, timeout time.Duration) error
	// Remove force-removes a container. Removing a missing container is not an error.
	Remove(ctx context.Context, name string) error
	Inspect(ctx context.Context, name string) (*Container, error)
	List(ctx context.Context, labels map[string]string) ([]Container, error)
	Logs(ctx context.Context, name string, opts LogOptions, fn func(LogLine)) error
	Exec(ctx context.Context, name string, cmd []string, stdout, stderr io.Writer) (int, error)
	Stats(ctx context.Context, names []string) ([]Stat, error)

	RemoveVolume(ctx context.Context, name string) error
	Prune(ctx context.Context) (string, error)
}

// LineWriter is an io.Writer that calls fn once per complete line.
type LineWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
	fn  func(string)
}

func NewLineWriter(fn func(string)) *LineWriter { return &LineWriter{fn: fn} }

func (w *LineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	for {
		b := w.buf.Bytes()
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			// Guard against pathological output with no newlines.
			if w.buf.Len() > 64*1024 {
				w.fn(w.buf.String())
				w.buf.Reset()
			}
			break
		}
		line := string(bytes.TrimRight(b[:i], "\r"))
		w.buf.Next(i + 1)
		w.fn(line)
	}
	return len(p), nil
}

// Flush emits any trailing partial line.
func (w *LineWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.buf.Len() > 0 {
		w.fn(w.buf.String())
		w.buf.Reset()
	}
}
