package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Docker drives the Docker Engine through the `docker` CLI. Shelling out keeps
// saba compatible with every Docker version and with BuildKit's build UX.
type Docker struct {
	Bin string
	// Host is the engine to talk to (a DOCKER_HOST value). Empty means the local one.
	Host string

	driverOnce sync.Once
	logDriver  string
}

func NewDocker() *Docker { return &Docker{Bin: "docker"} }

var _ Runtime = (*Docker)(nil)

func (d *Docker) cmd(ctx context.Context, args ...string) *exec.Cmd {
	c := exec.CommandContext(ctx, d.Bin, args...)
	c.Env = append(os.Environ(), "DOCKER_BUILDKIT=1", "DOCKER_CLI_HINTS=false")
	if d.Host != "" {
		c.Env = append(c.Env, "DOCKER_HOST="+d.Host, "DOCKER_CONTEXT=")
	}
	c.WaitDelay = 5 * time.Second
	return c
}

// output runs a docker command and returns trimmed stdout.
func (d *Docker) output(ctx context.Context, args ...string) (string, error) {
	c := d.cmd(ctx, args...)
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &stdout, &stderr
	if err := c.Run(); err != nil {
		return "", wrapErr(args, stderr.String(), err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

func wrapErr(args []string, stderr string, err error) error {
	msg := strings.TrimSpace(stderr)
	msg = strings.TrimPrefix(msg, "Error response from daemon: ")
	msg = strings.TrimPrefix(msg, "Error: ")
	if isNotFound(msg) {
		return fmt.Errorf("%w: %s", ErrNotFound, msg)
	}
	if msg == "" {
		msg = err.Error()
	}
	var ee *exec.Error
	if errors.As(err, &ee) {
		return fmt.Errorf("docker CLI is not available: %v", err)
	}
	return fmt.Errorf("docker %s: %s", args[0], msg)
}

func isNotFound(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "no such container") || strings.Contains(m, "no such object") ||
		strings.Contains(m, "no such image") || strings.Contains(m, "no such volume") ||
		strings.Contains(m, "no such network")
}

func (d *Docker) Version(ctx context.Context) (string, error) {
	return d.output(ctx, "version", "--format", "{{.Server.Version}}")
}

func (d *Docker) EnsureNetwork(ctx context.Context, name string) error {
	if _, err := d.output(ctx, "network", "inspect", "--format", "{{.Id}}", name); err == nil {
		return nil
	}
	_, err := d.output(ctx, "network", "create", name)
	if err != nil && strings.Contains(err.Error(), "already exists") {
		return nil
	}
	return err
}

func (d *Docker) stream(ctx context.Context, out io.Writer, args ...string) error {
	c := d.cmd(ctx, args...)
	var tail tailBuffer
	w := io.MultiWriter(out, &tail)
	c.Stdout, c.Stderr = w, w
	if err := c.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return wrapErr(args, tail.String(), err)
	}
	return nil
}

func (d *Docker) Pull(ctx context.Context, image string, out io.Writer) error {
	return d.stream(ctx, out, "pull", image)
}

func (d *Docker) Build(ctx context.Context, o BuildOptions, out io.Writer) error {
	args := []string{"build", "--progress=plain", "-t", o.Tag}
	if o.Dockerfile != "" {
		args = append(args, "-f", o.Dockerfile)
	}
	if o.Target != "" {
		args = append(args, "--target", o.Target)
	}
	for _, k := range sortedKeys(o.Args) {
		args = append(args, "--build-arg", k+"="+o.Args[k])
	}
	for _, k := range sortedKeys(o.Labels) {
		args = append(args, "--label", k+"="+o.Labels[k])
	}
	args = append(args, o.ContextDir)
	c := d.cmd(ctx, args...)
	c.Stdout, c.Stderr = out, out
	if err := c.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return fmt.Errorf("docker build exited with code %d", ee.ExitCode())
		}
		return fmt.Errorf("docker build: %v", err)
	}
	return nil
}

func (d *Docker) InspectImage(ctx context.Context, ref string) (*Image, error) {
	out, err := d.output(ctx, "image", "inspect", "--format", "{{json .}}", ref)
	if err != nil {
		return nil, err
	}
	var raw struct {
		ID     string `json:"Id"`
		Config struct {
			ExposedPorts map[string]any
		}
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("parsing image inspect output: %w", err)
	}
	img := &Image{ID: raw.ID}
	for p := range raw.Config.ExposedPorts {
		num, proto, _ := strings.Cut(p, "/")
		if proto != "" && proto != "tcp" {
			continue
		}
		if n, err := strconv.Atoi(num); err == nil {
			img.ExposedPorts = append(img.ExposedPorts, n)
		}
	}
	sort.Ints(img.ExposedPorts)
	return img, nil
}

func (d *Docker) SaveImage(ctx context.Context, ref string, w io.Writer) error {
	c := d.cmd(ctx, "save", ref)
	var stderr tailBuffer
	c.Stdout, c.Stderr = w, &stderr
	if err := c.Run(); err != nil {
		return wrapErr([]string{"save"}, stderr.String(), err)
	}
	return nil
}

func (d *Docker) LoadImage(ctx context.Context, r io.Reader) error {
	c := d.cmd(ctx, "load", "--quiet")
	var out tailBuffer
	c.Stdin, c.Stdout, c.Stderr = r, &out, &out
	if err := c.Run(); err != nil {
		return wrapErr([]string{"load"}, out.String(), err)
	}
	return nil
}

func (d *Docker) TagImage(ctx context.Context, src, dst string) error {
	_, err := d.output(ctx, "tag", src, dst)
	return err
}

func (d *Docker) RemoveImage(ctx context.Context, ref string) error {
	_, err := d.output(ctx, "rmi", ref)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// cliEnvRe matches variables that would change the behaviour of the docker CLI
// itself if they were placed in its process environment.
var cliEnvRe = regexp.MustCompile(`(?i)^(PATH|HOME|USER|TMPDIR|PWD|SHELL|LANG|LC_.*|DOCKER_.*|BUILDKIT_.*|BUILDX_.*|COMPOSE_.*|XDG_.*|LD_.*|SSL_CERT_.*|(HTTPS?|NO|ALL|FTP)_PROXY)$`)

func (d *Docker) runArgs(o RunOptions, c *exec.Cmd) []string {
	var args []string
	if o.Name != "" {
		args = append(args, "--name", o.Name)
	}
	if o.Network != "" {
		args = append(args, "--network", o.Network)
		for _, a := range o.Aliases {
			args = append(args, "--network-alias", a)
		}
	}
	for _, k := range sortedKeys(o.Labels) {
		args = append(args, "--label", k+"="+o.Labels[k])
	}
	// Values travel through the CLI's environment rather than argv so secrets
	// do not show up in `ps` and may contain any bytes, including newlines.
	for _, k := range sortedKeys(o.Env) {
		if cliEnvRe.MatchString(k) {
			args = append(args, "-e", k+"="+o.Env[k])
			continue
		}
		args = append(args, "-e", k)
		c.Env = append(c.Env, k+"="+o.Env[k])
	}
	for _, p := range o.Ports {
		args = append(args, "-p", p)
	}
	for _, v := range o.Volumes {
		args = append(args, "-v", v)
	}
	for _, h := range o.ExtraHosts {
		args = append(args, "--add-host", h)
	}
	if o.Memory != "" {
		args = append(args, "--memory", o.Memory)
	}
	if o.CPUs != "" {
		args = append(args, "--cpus", o.CPUs)
	}
	if o.Restart != "" {
		args = append(args, "--restart", o.Restart)
	}
	if o.StopTimeout > 0 {
		args = append(args, "--stop-timeout", strconv.Itoa(int(o.StopTimeout.Seconds())))
	}
	return args
}

func (d *Docker) loggingDriver(ctx context.Context) string {
	d.driverOnce.Do(func() {
		d.logDriver, _ = d.output(ctx, "info", "--format", "{{.LoggingDriver}}")
	})
	return d.logDriver
}

func (d *Docker) Run(ctx context.Context, o RunOptions) (string, error) {
	c := d.cmd(ctx)
	args := []string{"run", "-d", "--init"}
	args = append(args, d.runArgs(o, c)...)
	// Bound log growth where the driver supports it.
	if drv := d.loggingDriver(ctx); drv == "json-file" || drv == "local" {
		args = append(args, "--log-opt", "max-size=20m", "--log-opt", "max-file=3")
	}
	args = append(args, o.Image)
	args = append(args, o.Command...)
	c.Args = append(c.Args, args...)
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &stdout, &stderr
	if err := c.Run(); err != nil {
		return "", wrapErr(args, stderr.String(), err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

func (d *Docker) RunOnce(ctx context.Context, o RunOptions, stdout, stderr io.Writer) (int, error) {
	c := d.cmd(ctx)
	args := []string{"run", "--rm", "--init"}
	args = append(args, d.runArgs(o, c)...)
	args = append(args, o.Image)
	args = append(args, o.Command...)
	c.Args = append(c.Args, args...)
	var tail tailBuffer
	c.Stdout, c.Stderr = stdout, io.MultiWriter(stderr, &tail)
	c.Cancel = func() error {
		// Killing the CLI would leave the container running; remove it instead.
		if o.Name != "" {
			rmCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			_ = d.Remove(rmCtx, o.Name)
		}
		return c.Process.Kill()
	}
	return exitCode(ctx, args, tail.String(), c.Run())
}

// exitCode separates "the command ran and exited non-zero" from "docker failed".
func exitCode(ctx context.Context, args []string, stderr string, err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	if ctx.Err() != nil {
		return -1, ctx.Err()
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code := ee.ExitCode()
		if code == 125 { // docker itself failed
			return code, wrapErr(args, stderr, err)
		}
		return code, nil
	}
	return -1, wrapErr(args, stderr, err)
}

func (d *Docker) Start(ctx context.Context, name string) error {
	_, err := d.output(ctx, "start", name)
	return err
}

func (d *Docker) SetRestart(ctx context.Context, name, policy string) error {
	_, err := d.output(ctx, "update", "--restart", policy, name)
	return err
}

func (d *Docker) Stop(ctx context.Context, name string, timeout time.Duration) error {
	secs := int(timeout.Seconds())
	if secs <= 0 {
		secs = 10
	}
	_, err := d.output(ctx, "stop", "-t", strconv.Itoa(secs), name)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func (d *Docker) Remove(ctx context.Context, name string) error {
	_, err := d.output(ctx, "rm", "-f", name)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

type inspectOut struct {
	ID           string `json:"Id"`
	Name         string
	RestartCount int
	Config       struct {
		Image  string
		Labels map[string]string
	}
	State struct {
		Status    string
		ExitCode  int
		OOMKilled bool
		StartedAt string
		Error     string
		Health    *struct{ Status string }
	}
}

func (r *inspectOut) container() Container {
	c := Container{
		ID:        r.ID,
		Name:      strings.TrimPrefix(r.Name, "/"),
		Image:     r.Config.Image,
		State:     r.State.Status,
		ExitCode:  r.State.ExitCode,
		OOMKilled: r.State.OOMKilled,
		Restarts:  r.RestartCount,
		Labels:    r.Config.Labels,
		Error:     r.State.Error,
	}
	if r.State.Health != nil {
		c.Health = r.State.Health.Status
	}
	if t, err := time.Parse(time.RFC3339Nano, r.State.StartedAt); err == nil && t.Year() > 1 {
		c.StartedAt = t
	}
	return c
}

func (d *Docker) inspect(ctx context.Context, names ...string) ([]Container, error) {
	args := append([]string{"container", "inspect"}, names...)
	out, err := d.output(ctx, args...)
	if err != nil {
		return nil, err
	}
	var raw []inspectOut
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("parsing container inspect output: %w", err)
	}
	cs := make([]Container, len(raw))
	for i := range raw {
		cs[i] = raw[i].container()
	}
	return cs, nil
}

func (d *Docker) Inspect(ctx context.Context, name string) (*Container, error) {
	cs, err := d.inspect(ctx, name)
	if err != nil {
		return nil, err
	}
	if len(cs) == 0 {
		return nil, ErrNotFound
	}
	return &cs[0], nil
}

func (d *Docker) List(ctx context.Context, labels map[string]string) ([]Container, error) {
	args := []string{"ps", "-aq", "--no-trunc"}
	for _, k := range sortedKeys(labels) {
		args = append(args, "--filter", "label="+k+"="+labels[k])
	}
	out, err := d.output(ctx, args...)
	if err != nil {
		return nil, err
	}
	ids := strings.Fields(out)
	if len(ids) == 0 {
		return nil, nil
	}
	cs, err := d.inspect(ctx, ids...)
	if errors.Is(err, ErrNotFound) {
		// A container vanished between `ps` and `inspect`; inspect one by one.
		cs = nil
		for _, id := range ids {
			if c, err := d.Inspect(ctx, id); err == nil {
				cs = append(cs, *c)
			}
		}
		err = nil
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].Name < cs[j].Name })
	return cs, err
}

func (d *Docker) Logs(ctx context.Context, name string, o LogOptions, fn func(LogLine)) error {
	args := []string{"logs", "--timestamps"}
	if o.Tail > 0 {
		args = append(args, "--tail", strconv.Itoa(o.Tail))
	}
	if o.Since != "" {
		args = append(args, "--since", o.Since)
	}
	if o.Follow {
		args = append(args, "--follow")
	}
	args = append(args, name)
	c := d.cmd(ctx, args...)
	stdout, err := c.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := c.StderrPipe()
	if err != nil {
		return err
	}
	if err := c.Start(); err != nil {
		return wrapErr(args, "", err)
	}
	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		errTail tailBuffer
		// stdout and stderr arrive on separate pipes; without --follow the
		// lines are buffered and merged back into chronological order.
		buffered []LogLine
	)
	emit := fn
	if !o.Follow {
		emit = func(l LogLine) { buffered = append(buffered, l) }
	}
	read := func(r io.Reader, isErr bool) {
		defer wg.Done()
		br := bufio.NewReaderSize(r, 64*1024)
		for {
			line, err := br.ReadString('\n')
			if line != "" {
				line = strings.TrimRight(line, "\r\n")
				ll := parseLogLine(line)
				mu.Lock()
				if isErr && ll.Time.IsZero() {
					errTail.Write([]byte(line + "\n"))
				} else {
					emit(ll)
				}
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}
	wg.Add(2)
	go read(stdout, false)
	go read(stderr, true)
	wg.Wait()
	sort.SliceStable(buffered, func(i, j int) bool { return buffered[i].Time.Before(buffered[j].Time) })
	for _, l := range buffered {
		fn(l)
	}
	if err := c.Wait(); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return wrapErr(args, errTail.String(), err)
	}
	return nil
}

func parseLogLine(line string) LogLine {
	ts, rest, ok := strings.Cut(line, " ")
	if !ok {
		ts, rest = line, ""
	}
	if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
		return LogLine{Time: t, Text: rest}
	}
	return LogLine{Text: line}
}

func (d *Docker) Exec(ctx context.Context, name string, cmd []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	args := []string{"exec"}
	if stdin != nil {
		args = append(args, "-i")
	}
	args = append(append(args, name), cmd...)
	c := d.cmd(ctx, args...)
	var tail tailBuffer
	c.Stdin = stdin
	c.Stdout, c.Stderr = stdout, io.MultiWriter(stderr, &tail)
	err := c.Run()
	if err != nil && isNotFound(tail.String()) {
		return -1, fmt.Errorf("%w: %s", ErrNotFound, strings.TrimSpace(tail.String()))
	}
	if err != nil && strings.Contains(tail.String(), "is not running") {
		return -1, fmt.Errorf("container %s is not running", name)
	}
	return exitCode(ctx, args, tail.String(), err)
}

func (d *Docker) Stats(ctx context.Context, names []string) ([]Stat, error) {
	if len(names) == 0 {
		return nil, nil
	}
	args := append([]string{"stats", "--no-stream", "--format", "{{json .}}"}, names...)
	out, err := d.output(ctx, args...)
	if err != nil {
		return nil, err
	}
	var stats []Stat
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var raw struct{ Name, CPUPerc, MemUsage, MemPerc, NetIO, BlockIO, PIDs string }
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			continue
		}
		stats = append(stats, Stat{
			Container: raw.Name, CPU: raw.CPUPerc, Memory: raw.MemUsage, MemoryPct: raw.MemPerc,
			NetIO: raw.NetIO, BlockIO: raw.BlockIO, PIDs: raw.PIDs,
		})
	}
	return stats, nil
}

func (d *Docker) RemoveVolume(ctx context.Context, name string) error {
	_, err := d.output(ctx, "volume", "rm", name)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func (d *Docker) Prune(ctx context.Context) (string, error) {
	img, err := d.output(ctx, "image", "prune", "-f")
	if err != nil {
		return "", err
	}
	bld, _ := d.output(ctx, "builder", "prune", "-f", "--filter", "until=72h")
	return strings.TrimSpace(img + "\n" + bld), nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// tailBuffer keeps the last few KB written to it, for error messages.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

const tailMax = 8 * 1024

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > tailMax {
		t.buf = t.buf[len(t.buf)-tailMax:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
