package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/egoist/saba/internal/api"
)

// Client is a typed client for the daemon API.
type Client struct {
	Target Target
	http   *http.Client

	mu      sync.Mutex
	lastSSH *sshConn
}

func New(t Target) *Client {
	c := &Client{Target: t}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			if t.Local() {
				sock := t.Socket
				if sock == "" {
					sock = socketPath()
				}
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			}
			conn, err := dialSSH(ctx, t.SSH)
			if err == nil {
				c.mu.Lock()
				c.lastSSH = conn
				c.mu.Unlock()
			}
			return conn, err
		},
		DisableCompression: true,
		MaxIdleConns:       2,
		IdleConnTimeout:    30 * time.Second,
	}
	c.http = &http.Client{Transport: tr}
	return c
}

// Close releases idle connections (and their ssh processes).
func (c *Client) Close() { c.http.CloseIdleConnections() }

// sshConn adapts an `ssh host saba dial-stdio` process to net.Conn.
type sshConn struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr *syncBuffer
	once   sync.Once
}

// remoteDial finds saba even when a non-login shell has a minimal PATH.
const remoteDial = `command -v saba >/dev/null 2>&1 && exec saba dial-stdio; exec /usr/local/bin/saba dial-stdio`

func dialSSH(ctx context.Context, dest string) (*sshConn, error) {
	cmd := exec.Command("ssh", append(sshArgs(dest, false), remoteDial)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	// A plain os.Pipe (rather than cmd.Stderr = buffer) so that a multiplexing
	// master that outlives this command cannot block Wait.
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = pw
	buf := &syncBuffer{}
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return nil, fmt.Errorf("cannot run ssh: %w", err)
	}
	pw.Close()
	go func() {
		_, _ = io.Copy(buf, pr)
		pr.Close()
	}()
	return &sshConn{cmd: cmd, stdin: stdin, stdout: stdout, stderr: buf}, nil
}

func (c *sshConn) Read(p []byte) (int, error)  { return c.stdout.Read(p) }
func (c *sshConn) Write(p []byte) (int, error) { return c.stdin.Write(p) }

func (c *sshConn) Close() error {
	c.once.Do(func() {
		c.stdin.Close()
		done := make(chan struct{})
		go func() { _ = c.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = c.cmd.Process.Kill()
			<-done
		}
	})
	return nil
}

type addr string

func (a addr) Network() string { return "ssh" }
func (a addr) String() string  { return string(a) }

func (c *sshConn) LocalAddr() net.Addr              { return addr("local") }
func (c *sshConn) RemoteAddr() net.Addr             { return addr("remote") }
func (c *sshConn) SetDeadline(time.Time) error      { return nil }
func (c *sshConn) SetReadDeadline(time.Time) error  { return nil }
func (c *sshConn) SetWriteDeadline(time.Time) error { return nil }

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.b.Len() < 16*1024 {
		s.b.Write(p)
	}
	return len(p), nil
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// transportError converts a low-level failure into a structured error,
// using ssh's stderr to explain what went wrong.
func (c *Client) transportError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	detail := err.Error()
	if !c.Target.Local() {
		c.mu.Lock()
		conn := c.lastSSH
		c.mu.Unlock()
		if conn != nil {
			// Give ssh a moment to exit and flush its diagnostics.
			conn.Close()
			time.Sleep(50 * time.Millisecond)
			if s := conn.stderr.String(); s != "" {
				detail = s
			}
		}
	}
	return unreachable(c.Target, detail)
}

func (c *Client) request(ctx context.Context, method, path string, query url.Values, body io.Reader, contentType string) (*http.Response, error) {
	u := "http://saba" + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, c.transportError(ctx, err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		var er api.ErrorResponse
		if json.Unmarshal(b, &er) == nil && er.Error != nil {
			return nil, er.Error
		}
		return nil, api.Errorf(api.CodeInternal, "server returned %s: %s", resp.Status, bytes.TrimSpace(b))
	}
	return resp, nil
}

func (c *Client) json(ctx context.Context, method, path string, query url.Values, in, out any) error {
	var body io.Reader
	ct := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, ct = bytes.NewReader(b), "application/json"
	}
	resp, err := c.request(ctx, method, path, query, body, ct)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return api.Errorf(api.CodeInternal, "cannot decode server response: %v", err)
	}
	return nil
}

// ErrStop can be returned by a stream callback to end the stream without error.
var ErrStop = errors.New("stop")

// events reads an NDJSON stream. An "error" event ends the stream with that error.
func (c *Client) events(ctx context.Context, resp *http.Response, fn func(api.Event) error) error {
	defer resp.Body.Close()
	br := bufio.NewReaderSize(resp.Body, 64*1024)
	for {
		line, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var ev api.Event
			if jerr := json.Unmarshal(line, &ev); jerr != nil {
				return api.Errorf(api.CodeInternal, "cannot decode server event: %v", jerr)
			}
			if ferr := fn(ev); ferr != nil {
				if errors.Is(ferr, ErrStop) {
					return nil
				}
				return ferr
			}
			if ev.Type == "error" && ev.Error != nil {
				return ev.Error
			}
		}
		if err != nil {
			if err == io.EOF || ctx.Err() != nil {
				return nil
			}
			return c.transportError(ctx, err)
		}
	}
}

func appPath(app string, rest string) string { return "/v1/apps/" + url.PathEscape(app) + rest }

func (c *Client) Info(ctx context.Context) (*api.ServerInfo, error) {
	var out api.ServerInfo
	return &out, c.json(ctx, "GET", "/v1/info", nil, nil, &out)
}

func (c *Client) PatchConfig(ctx context.Context, patch map[string]any) (*api.ServerConfig, error) {
	var out api.ServerConfig
	return &out, c.json(ctx, "PATCH", "/v1/config", nil, patch, &out)
}

func (c *Client) Doctor(ctx context.Context) (*api.DoctorReport, error) {
	var out api.DoctorReport
	return &out, c.json(ctx, "GET", "/v1/doctor", nil, nil, &out)
}

func (c *Client) Apps(ctx context.Context) ([]*api.App, error) {
	var out []*api.App
	return out, c.json(ctx, "GET", "/v1/apps", nil, nil, &out)
}

func (c *Client) App(ctx context.Context, name string) (*api.App, error) {
	var out api.App
	if err := c.json(ctx, "GET", appPath(name, ""), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

type DestroyResult struct {
	App         string   `json:"app"`
	Destroyed   bool     `json:"destroyed"`
	VolumesKept []string `json:"volumes_kept"`
}

func (c *Client) Destroy(ctx context.Context, name string, volumes bool) (*DestroyResult, error) {
	var out DestroyResult
	q := url.Values{}
	if volumes {
		q.Set("volumes", "true")
	}
	return &out, c.json(ctx, "DELETE", appPath(name, ""), q, nil, &out)
}

// Deploy sends a deploy request, optionally with a source archive, and
// streams progress events to fn. It returns the final result.
func (c *Client) Deploy(ctx context.Context, name string, req api.DeployRequest, archive io.Reader, detach bool, fn func(api.Event)) (*api.DeployResult, error) {
	var (
		body io.Reader
		ct   string
	)
	reqJSON, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if archive == nil {
		body, ct = bytes.NewReader(reqJSON), "application/json"
	} else {
		pr, pw := io.Pipe()
		mw := multipart.NewWriter(pw)
		go func() {
			err := func() error {
				part, err := mw.CreateFormField("request")
				if err != nil {
					return err
				}
				if _, err := part.Write(reqJSON); err != nil {
					return err
				}
				part, err = mw.CreateFormFile("context", "context.tar.gz")
				if err != nil {
					return err
				}
				if _, err := io.Copy(part, archive); err != nil {
					return err
				}
				return mw.Close()
			}()
			pw.CloseWithError(err)
		}()
		body, ct = pr, mw.FormDataContentType()
	}
	q := url.Values{}
	if detach {
		q.Set("detach", "true")
	}
	resp, err := c.request(ctx, "POST", appPath(name, "/deploy"), q, body, ct)
	if err != nil {
		return nil, err
	}
	var result *api.DeployResult
	err = c.events(ctx, resp, func(ev api.Event) error {
		if ev.Type == "result" {
			result = ev.Result
			if fn != nil {
				fn(ev)
			}
			return ErrStop
		}
		if fn != nil {
			fn(ev)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, api.Errorf(api.CodeUnreachable, "the connection closed before the deployment finished").
			WithHint("The deployment continues on the server. Follow it with `saba logs -a %s --deployment latest --follow`.", name)
	}
	return result, nil
}

// Cancel asks the server to abort an app's deployment and returns its version.
func (c *Client) Cancel(ctx context.Context, name string) (int, error) {
	var out struct {
		Version int `json:"version"`
	}
	return out.Version, c.json(ctx, "POST", appPath(name, "/cancel"), nil, nil, &out)
}

func (c *Client) Stop(ctx context.Context, name string) (*api.App, error) {
	var out api.App
	return &out, c.json(ctx, "POST", appPath(name, "/stop"), nil, nil, &out)
}

func (c *Client) Start(ctx context.Context, name string) (*api.App, error) {
	var out api.App
	return &out, c.json(ctx, "POST", appPath(name, "/start"), nil, nil, &out)
}

func (c *Client) Env(ctx context.Context, name string) (*api.EnvResponse, error) {
	var out api.EnvResponse
	return &out, c.json(ctx, "GET", appPath(name, "/env"), nil, nil, &out)
}

func (c *Client) Outputs(ctx context.Context, name string) (map[string]string, error) {
	out := map[string]string{}
	return out, c.json(ctx, "GET", appPath(name, "/outputs"), nil, nil, &out)
}

func (c *Client) Deployments(ctx context.Context, name string) ([]*api.Deployment, error) {
	var out []*api.Deployment
	return out, c.json(ctx, "GET", appPath(name, "/deployments"), nil, nil, &out)
}

func (c *Client) DeployEvents(ctx context.Context, name, version string, follow bool, fn func(api.Event) error) error {
	q := url.Values{}
	if follow {
		q.Set("follow", "true")
	}
	if version == "" {
		version = "latest"
	}
	resp, err := c.request(ctx, "GET", appPath(name, "/deployments/"+url.PathEscape(version)+"/events"), q, nil, "")
	if err != nil {
		return err
	}
	return c.events(ctx, resp, fn)
}

func (c *Client) Logs(ctx context.Context, name string, lq api.LogsQuery, fn func(api.Event) error) error {
	q := url.Values{}
	if lq.Tail > 0 {
		q.Set("tail", strconv.Itoa(lq.Tail))
	}
	if lq.Since != "" {
		q.Set("since", lq.Since)
	}
	if lq.Follow {
		q.Set("follow", "true")
	}
	if lq.Replica > 0 {
		q.Set("replica", strconv.Itoa(lq.Replica))
	}
	resp, err := c.request(ctx, "GET", appPath(name, "/logs"), q, nil, "")
	if err != nil {
		return err
	}
	return c.events(ctx, resp, fn)
}

func (c *Client) Container(ctx context.Context, name string, replica int) (string, error) {
	var out struct {
		Container string `json:"container"`
	}
	q := url.Values{}
	if replica > 0 {
		q.Set("replica", strconv.Itoa(replica))
	}
	return out.Container, c.json(ctx, "GET", appPath(name, "/container"), q, nil, &out)
}

// Exec runs a command and streams its output. It returns the exit code.
func (c *Client) Exec(ctx context.Context, name string, req api.ExecRequest, stdout, stderr io.Writer) (int, error) {
	b, _ := json.Marshal(req)
	resp, err := c.request(ctx, "POST", appPath(name, "/exec"), nil, bytes.NewReader(b), "application/json")
	if err != nil {
		return -1, err
	}
	code := -1
	finished := false
	err = c.events(ctx, resp, func(ev api.Event) error {
		switch ev.Type {
		case "output":
			w := stdout
			if ev.Stream == "stderr" {
				w = stderr
			}
			_, _ = w.Write(ev.Data)
		case "exit":
			if ev.ExitCode != nil {
				code = *ev.ExitCode
			}
			finished = true
		}
		return nil
	})
	if err != nil {
		return -1, err
	}
	if !finished {
		return -1, api.Errorf(api.CodeUnreachable, "the connection closed before the command finished")
	}
	return code, nil
}

func (c *Client) CronRun(ctx context.Context, name, job string) (*api.CronState, error) {
	var out api.CronState
	return &out, c.json(ctx, "POST", appPath(name, "/cron/"+url.PathEscape(job)+"/run"), nil, nil, &out)
}

func (c *Client) Stats(ctx context.Context, name string) ([]api.Stats, error) {
	var out []api.Stats
	q := url.Values{}
	if name != "" {
		q.Set("app", name)
	}
	return out, c.json(ctx, "GET", "/v1/stats", q, nil, &out)
}

func (c *Client) Prune(ctx context.Context) (*api.PruneResult, error) {
	var out api.PruneResult
	return &out, c.json(ctx, "POST", "/v1/prune", nil, nil, &out)
}

func (c *Client) Backups(ctx context.Context, name string) ([]api.Backup, error) {
	var out []api.Backup
	return out, c.json(ctx, "GET", appPath(name, "/backups"), nil, nil, &out)
}

func (c *Client) BackupCreate(ctx context.Context, name string) (*api.Backup, error) {
	var out api.Backup
	return &out, c.json(ctx, "POST", appPath(name, "/backups"), nil, nil, &out)
}

// BackupDownload streams a stored backup (gzip) to w and returns its file name.
func (c *Client) BackupDownload(ctx context.Context, name, id string, w io.Writer) (string, error) {
	resp, err := c.request(ctx, "GET", appPath(name, "/backups/"+url.PathEscape(id)), nil, nil, "")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	n, err := io.Copy(w, resp.Body)
	if err != nil {
		return "", c.transportError(ctx, err)
	}
	if resp.ContentLength > 0 && n != resp.ContentLength {
		return "", api.Errorf(api.CodeUnreachable, "the download was cut short (%d of %d bytes)", n, resp.ContentLength)
	}
	return resp.Header.Get("X-Saba-Backup-File"), nil
}

func (c *Client) BackupDelete(ctx context.Context, name, id string) error {
	return c.json(ctx, "DELETE", appPath(name, "/backups/"+url.PathEscape(id)), nil, nil, nil)
}

func (c *Client) BackupPolicy(ctx context.Context, name string, p api.BackupPolicy) (*api.BackupPolicy, error) {
	var out api.BackupPolicy
	return &out, c.json(ctx, "PUT", appPath(name, "/backup-policy"), nil, p, &out)
}

type RestoreResult struct {
	App      string      `json:"app"`
	Restored bool        `json:"restored"`
	Backup   *api.Backup `json:"backup,omitempty"`
}

// Restore loads a stored backup (id) or, when dump is non-nil, the given dump.
func (c *Client) Restore(ctx context.Context, name, id string, dump io.Reader) (*RestoreResult, error) {
	q := url.Values{}
	ct := ""
	if dump == nil {
		q.Set("backup", id)
	} else {
		ct = "application/octet-stream"
	}
	resp, err := c.request(ctx, "POST", appPath(name, "/restore"), q, dump, ct)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out RestoreResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, api.Errorf(api.CodeInternal, "cannot decode server response: %v", err)
	}
	return &out, nil
}

func (c *Client) Nodes(ctx context.Context) ([]api.Node, error) {
	var out []api.Node
	return out, c.json(ctx, "GET", "/v1/nodes", nil, nil, &out)
}

func (c *Client) NodeKey(ctx context.Context) (string, error) {
	var out struct {
		PublicKey string `json:"public_key"`
	}
	return out.PublicKey, c.json(ctx, "GET", "/v1/nodes/key", nil, nil, &out)
}

func (c *Client) NodeAdd(ctx context.Context, n api.Node) (*api.Node, error) {
	var out api.Node
	return &out, c.json(ctx, "POST", "/v1/nodes", nil, n, &out)
}

func (c *Client) NodeRemove(ctx context.Context, name string) error {
	return c.json(ctx, "DELETE", "/v1/nodes/"+url.PathEscape(name), nil, nil, nil)
}

// ContainerInfo locates a replica's container for interactive use.
type ContainerInfo struct {
	Container  string `json:"container"`
	Node       string `json:"node,omitempty"`
	DockerHost string `json:"docker_host,omitempty"`
}

func (c *Client) ContainerInfo(ctx context.Context, name string, replica int) (*ContainerInfo, error) {
	var out ContainerInfo
	q := url.Values{}
	if replica > 0 {
		q.Set("replica", strconv.Itoa(replica))
	}
	return &out, c.json(ctx, "GET", appPath(name, "/container"), q, nil, &out)
}

func (c *Client) Certs(ctx context.Context) ([]api.Certificate, error) {
	var out []api.Certificate
	return out, c.json(ctx, "GET", "/v1/certs", nil, nil, &out)
}

func (c *Client) CertAdd(ctx context.Context, certPEM, keyPEM string) (*api.Certificate, error) {
	var out api.Certificate
	return &out, c.json(ctx, "POST", "/v1/certs", nil, map[string]string{"cert": certPEM, "key": keyPEM}, &out)
}

func (c *Client) CertRemove(ctx context.Context, id string) error {
	return c.json(ctx, "DELETE", "/v1/certs/"+url.PathEscape(id), nil, nil, nil)
}

func (c *Client) BackupStorage(ctx context.Context) (*api.S3Config, error) {
	var out api.S3Config
	return &out, c.json(ctx, "GET", "/v1/backup-storage", nil, nil, &out)
}

// SetBackupStorage verifies and saves the S3 destination for backups.
func (c *Client) SetBackupStorage(ctx context.Context, cfg api.S3Config, createBucket bool) (*api.S3Config, error) {
	body := struct {
		api.S3Config
		CreateBucket bool `json:"create_bucket"`
	}{cfg, createBucket}
	var out api.S3Config
	return &out, c.json(ctx, "PUT", "/v1/backup-storage", nil, body, &out)
}

func (c *Client) RemoveBackupStorage(ctx context.Context) error {
	return c.json(ctx, "DELETE", "/v1/backup-storage", nil, nil, nil)
}
