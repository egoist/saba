// Package runtimetest provides an in-process container runtime for tests.
package runtimetest

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/egoist/saba/internal/runtime"
)

// Fake is an in-process container runtime: every "container" is an
// HTTP server on the published loopback port whose behaviour is chosen by the
// image name.
//
//	ok        answers 200 with "<image> <SABA_VERSION>"
//	crash     exits with code 3 right after starting
//	broken    answers 500
//	slow      answers after 150ms (to exercise draining)
//	silent    never listens
type Fake struct {
	mu         sync.Mutex
	containers map[string]*fakeContainer
	images     map[string]string // tag → behaviour
	ids        map[string]string // tag → image id (shared by retagged copies)
	// Loads counts images received through LoadImage.
	Loads int
	// RunOnceFunc, if set, decides the exit code of one-off runs.
	RunOnceFunc func(opts runtime.RunOptions) int
}

type fakeContainer struct {
	runtime.Container
	opts   runtime.RunOptions
	server *http.Server
	logs   []string
	data   string // what a "dump" returns and a "restore" replaces
}

// SetData sets the data a container returns for dump commands.
func (f *Fake) SetData(name, data string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.containers[name]; ok {
		c.data = data
	}
}

// Data returns a container's current data.
func (f *Fake) Data(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.containers[name]; ok {
		return c.data
	}
	return ""
}

func New() *Fake {
	return &Fake{containers: map[string]*fakeContainer{}, images: map[string]string{}, ids: map[string]string{}}
}

var _ runtime.Runtime = (*Fake)(nil)

func (f *Fake) Version(context.Context) (string, error)          { return "fake", nil }
func (f *Fake) EnsureNetwork(context.Context, string) error      { return nil }
func (f *Fake) RemoveVolume(context.Context, string) error       { return nil }
func (f *Fake) Prune(context.Context) (string, error)            { return "", nil }
func (f *Fake) SetRestart(context.Context, string, string) error { return nil }

func (f *Fake) Pull(_ context.Context, image string, out io.Writer) error {
	if strings.HasPrefix(image, "missing") {
		return fmt.Errorf("pull access denied for %s", image)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.images[image] = strings.Split(image, ":")[0]
	f.ids[image] = "sha-" + image
	fmt.Fprintf(out, "Status: Downloaded %s\n", image)
	return nil
}

func (f *Fake) Build(_ context.Context, o runtime.BuildOptions, out io.Writer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.images[o.Tag] = "ok"
	f.ids[o.Tag] = "sha-" + o.Tag
	fmt.Fprintln(out, "built "+o.Tag)
	return nil
}

func (f *Fake) InspectImage(_ context.Context, ref string) (*runtime.Image, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.images[ref]; ok {
		return &runtime.Image{ID: f.ids[ref], ExposedPorts: []int{8080}}, nil
	}
	for _, id := range f.ids { // lookup by image id
		if id == ref {
			return &runtime.Image{ID: id, ExposedPorts: []int{8080}}, nil
		}
	}
	return nil, runtime.ErrNotFound
}

// SaveImage and LoadImage carry the tag and behaviour as a tiny text archive.
func (f *Fake) SaveImage(_ context.Context, ref string, w io.Writer) error {
	f.mu.Lock()
	b, ok := f.images[ref]
	f.mu.Unlock()
	if !ok {
		return runtime.ErrNotFound
	}
	_, err := fmt.Fprintf(w, "%s\n%s\n%s", ref, b, f.ids[ref])
	return err
}

func (f *Fake) LoadImage(_ context.Context, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	parts := strings.Split(string(b), "\n")
	if len(parts) != 3 {
		return fmt.Errorf("invalid image archive")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.images[parts[0]] = parts[1]
	f.ids[parts[0]] = parts[2]
	f.Loads++
	return nil
}

func (f *Fake) TagImage(_ context.Context, src, dst string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.images[src]; !ok {
		for tag, id := range f.ids { // src may be an image id
			if id == src {
				src = tag
			}
		}
	}
	b, ok := f.images[src]
	if !ok {
		return runtime.ErrNotFound
	}
	f.images[dst] = b
	f.ids[dst] = f.ids[src]
	return nil
}

func (f *Fake) RemoveImage(_ context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.images, ref)
	delete(f.ids, ref)
	return nil
}

// HasImage reports whether an image tag exists.
func (f *Fake) HasImage(ref string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.images[ref]
	return ok
}

func hostAddr(ports []string) string {
	for _, p := range ports {
		parts := strings.Split(p, ":")
		if len(parts) == 3 && parts[0] == "127.0.0.1" {
			return parts[0] + ":" + parts[1]
		}
	}
	return ""
}

func (f *Fake) Run(_ context.Context, o runtime.RunOptions) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, dup := f.containers[o.Name]; dup {
		return "", fmt.Errorf("container name %s already in use", o.Name)
	}
	behaviour, ok := f.images[o.Image]
	if !ok {
		return "", fmt.Errorf("no such image %s", o.Image)
	}
	c := &fakeContainer{opts: o}
	c.ID = "id-" + o.Name
	c.Name = o.Name
	c.Image = o.Image
	c.Labels = o.Labels
	c.State = "running"
	c.StartedAt = time.Now()
	c.logs = []string{"booting " + behaviour}
	f.containers[o.Name] = c
	if behaviour == "crash" {
		c.State, c.ExitCode = "exited", 3
		c.logs = append(c.logs, "fatal: cannot start")
		return c.ID, nil
	}
	return c.ID, f.serve(c, behaviour)
}

func (f *Fake) serve(c *fakeContainer, behaviour string) error {
	addr := hostAddr(c.opts.Ports)
	if addr == "" || behaviour == "silent" {
		return nil
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	body := behaviour + " v" + c.opts.Env["SABA_VERSION"]
	c.server = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch behaviour {
		case "broken":
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		case "slow":
			if r.URL.Path == "/slow" {
				time.Sleep(150 * time.Millisecond)
			}
		}
		fmt.Fprint(w, body)
	})}
	go c.server.Serve(l)
	return nil
}

func (f *Fake) RunOnce(_ context.Context, o runtime.RunOptions, stdout, _ io.Writer) (int, error) {
	fmt.Fprintf(stdout, "ran %s\n", strings.Join(o.Command, " "))
	f.mu.Lock()
	fn := f.RunOnceFunc
	f.mu.Unlock()
	if fn != nil {
		return fn(o), nil
	}
	return 0, nil
}

func (f *Fake) Start(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.containers[name]
	if !ok {
		return runtime.ErrNotFound
	}
	if c.State == "running" {
		return nil
	}
	c.State = "running"
	c.StartedAt = time.Now()
	return f.serve(c, f.images[c.Image])
}

func (f *Fake) Stop(_ context.Context, name string, _ time.Duration) error {
	f.mu.Lock()
	c, ok := f.containers[name]
	f.mu.Unlock()
	if !ok {
		return nil
	}
	f.mu.Lock()
	srv := c.server
	c.server = nil
	f.mu.Unlock()
	if srv != nil {
		// Abrupt, like a process that dies on SIGTERM: any request still in
		// flight fails. Zero downtime therefore depends on saba draining first.
		_ = srv.Close()
	}
	f.mu.Lock()
	c.State = "exited"
	f.mu.Unlock()
	return nil
}

func (f *Fake) Remove(ctx context.Context, name string) error {
	_ = f.Stop(ctx, name, 0)
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.containers, name)
	return nil
}

func (f *Fake) Inspect(_ context.Context, name string) (*runtime.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.containers[name]
	if !ok {
		return nil, runtime.ErrNotFound
	}
	cp := c.Container
	return &cp, nil
}

func (f *Fake) List(_ context.Context, labels map[string]string) ([]runtime.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []runtime.Container
	for _, c := range f.containers {
		match := true
		for k, v := range labels {
			if c.Labels[k] != v {
				match = false
			}
		}
		if match {
			out = append(out, c.Container)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *Fake) Logs(_ context.Context, name string, _ runtime.LogOptions, fn func(runtime.LogLine)) error {
	f.mu.Lock()
	c, ok := f.containers[name]
	var lines []string
	if ok {
		lines = append(lines, c.logs...)
	}
	f.mu.Unlock()
	if !ok {
		return runtime.ErrNotFound
	}
	for _, l := range lines {
		fn(runtime.LogLine{Time: time.Now(), Text: l})
	}
	return nil
}

// Exec emulates a few commands: anything given stdin stores it as the
// container's data (a restore); a command mentioning "dump" prints that data;
// "false" exits 1; everything else echoes its argv.
func (f *Fake) Exec(_ context.Context, name string, cmd []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	f.mu.Lock()
	c, ok := f.containers[name]
	f.mu.Unlock()
	if !ok {
		return -1, runtime.ErrNotFound
	}
	line := strings.Join(cmd, " ")
	if stdin != nil {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return -1, err
		}
		if strings.Contains(string(b), "CORRUPT") {
			fmt.Fprintln(stderr, "ERROR: syntax error in dump")
			return 3, nil
		}
		f.mu.Lock()
		c.data = string(b)
		f.mu.Unlock()
		return 0, nil
	}
	if strings.Contains(line, "dump") {
		f.mu.Lock()
		data := c.data
		f.mu.Unlock()
		fmt.Fprint(stdout, data)
		return 0, nil
	}
	fmt.Fprintf(stdout, "exec %s\n", strings.Join(cmd, " "))
	if len(cmd) > 0 && cmd[0] == "false" {
		return 1, nil
	}
	return 0, nil
}

func (f *Fake) Stats(context.Context, []string) ([]runtime.Stat, error) { return nil, nil }

// Names returns the names of all containers, sorted.
func (f *Fake) Names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for n := range f.containers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// AddImage registers an image tag with a behaviour.
func (f *Fake) AddImage(tag, behaviour string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.images[tag] = behaviour
	f.ids[tag] = "sha-" + tag
}

// SetRunOnce sets RunOnceFunc safely while containers are running.
func (f *Fake) SetRunOnce(fn func(opts runtime.RunOptions) int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.RunOnceFunc = fn
}
