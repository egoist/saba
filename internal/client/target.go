// Package client talks to a saba daemon, either through its local unix
// socket or through SSH (`ssh <host> saba dial-stdio`), so the only thing a
// server ever exposes is SSH.
package client

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/egoist/saba/internal/api"
)

const DefaultSocket = "/run/saba/saba.sock"

// Target describes how to reach a daemon.
type Target struct {
	// Name is the context name, or a description for ad-hoc targets.
	Name string `toml:"-" json:"name"`
	// SSH is an ssh destination: "user@host", a Host alias, or "ssh://user@host:port".
	SSH string `toml:"ssh,omitempty" json:"ssh,omitempty"`
	// Socket is a local unix socket path. Empty with empty SSH means the default socket.
	Socket string `toml:"socket,omitempty" json:"socket,omitempty"`
}

func (t Target) Local() bool { return t.SSH == "" }

func (t Target) String() string {
	if t.Local() {
		return "local"
	}
	return t.SSH
}

// Config is the per-user CLI configuration (~/.config/saba/config.toml).
type Config struct {
	Current  string            `toml:"current,omitempty"`
	Contexts map[string]Target `toml:"contexts,omitempty"`

	path string
}

func ConfigPath() string {
	if p := os.Getenv("SABA_CONFIG"); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "saba", "config.toml")
}

func LoadConfig() (*Config, error) {
	c := &Config{Contexts: map[string]Target{}, path: ConfigPath()}
	b, err := os.ReadFile(c.path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if err := toml.Unmarshal(b, c); err != nil {
		return nil, api.Errorf(api.CodeInvalid, "cannot parse %s: %v", c.path, err)
	}
	if c.Contexts == nil {
		c.Contexts = map[string]Target{}
	}
	return c, nil
}

func (c *Config) Save() error {
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return err
	}
	var b strings.Builder
	if err := toml.NewEncoder(&b).Encode(c); err != nil {
		return err
	}
	return os.WriteFile(c.path, []byte(b.String()), 0o600)
}

func (c *Config) Names() []string {
	names := make([]string, 0, len(c.Contexts))
	for n := range c.Contexts {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ParseHost interprets an ad-hoc host string: "local", "unix:///path", or an ssh destination.
func ParseHost(s string) Target {
	switch {
	case s == "local":
		return Target{Name: "local", Socket: socketPath()}
	case strings.HasPrefix(s, "unix://"):
		return Target{Name: "local", Socket: strings.TrimPrefix(s, "unix://")}
	default:
		return Target{Name: s, SSH: s}
	}
}

func socketPath() string {
	if p := os.Getenv("SABA_SOCKET"); p != "" {
		return p
	}
	return DefaultSocket
}

// Resolve picks the daemon to talk to. Precedence: explicit host, explicit
// context, the project's pinned context, the current context, a local daemon.
func Resolve(host, context, projectContext string) (Target, error) {
	if host == "" {
		host = os.Getenv("SABA_HOST")
	}
	if host != "" {
		return ParseHost(host), nil
	}
	cfg, err := LoadConfig()
	if err != nil {
		return Target{}, err
	}
	if context == "" {
		context = os.Getenv("SABA_CONTEXT")
	}
	source := "--context"
	if context == "" && projectContext != "" {
		context, source = projectContext, "saba.toml"
	}
	if context == "" && cfg.Current != "" {
		context, source = cfg.Current, "the current context"
	}
	if context != "" {
		if context == "local" {
			if t, ok := cfg.Contexts["local"]; ok {
				t.Name = "local"
				return t, nil
			}
			return Target{Name: "local", Socket: socketPath()}, nil
		}
		t, ok := cfg.Contexts[context]
		if !ok {
			known := strings.Join(cfg.Names(), ", ")
			if known == "" {
				known = "(none)"
			}
			return Target{}, api.Errorf(api.CodeNoContext, "context %q (from %s) is not defined on this machine", context, source).
				WithHint("Known contexts: %s. Add it with `saba context add %s <user@host>`.", known, context)
		}
		t.Name = context
		return t, nil
	}
	if _, err := os.Stat(socketPath()); err == nil {
		return Target{Name: "local", Socket: socketPath()}, nil
	}
	return Target{}, api.Errorf(api.CodeNoContext, "no saba server is configured").
		WithHint("Set up a server with `saba server setup <user@host>`, add an existing one with `saba context add <name> <user@host>`, or set SABA_HOST=<user@host>.")
}

// sshArgs builds the ssh invocation for a destination.
func sshArgs(dest string, tty bool) []string {
	var args []string
	if strings.HasPrefix(dest, "ssh://") {
		rest := strings.TrimPrefix(dest, "ssh://")
		if i := strings.LastIndex(rest, ":"); i > strings.LastIndex(rest, "]") && i > strings.LastIndex(rest, "@") {
			args = append(args, "-p", rest[i+1:])
			rest = rest[:i]
		}
		dest = rest
	}
	if os.Getenv("SABA_SSH_MUX") != "0" {
		if dir, err := os.UserCacheDir(); err == nil {
			dir = filepath.Join(dir, "saba", "ssh")
			if os.MkdirAll(dir, 0o700) == nil {
				// Reuse one connection across commands: later calls skip the handshake.
				args = append(args, "-o", "ControlMaster=auto", "-o", "ControlPath="+filepath.Join(dir, "%C"), "-o", "ControlPersist=120")
			}
		}
	}
	if tty {
		args = append(args, "-t")
	} else {
		args = append(args, "-T")
		// Without a terminal nobody can answer a prompt, so fail fast instead of hanging.
		if !stdinIsTerminal() {
			args = append(args, "-o", "BatchMode=yes")
		}
	}
	args = append(args, "-o", "ConnectTimeout=15", "-o", "ServerAliveInterval=30")
	if extra := strings.Fields(os.Getenv("SABA_SSH_OPTS")); len(extra) > 0 {
		args = append(args, extra...)
	}
	return append(args, dest)
}

// SSHCommand returns the argv for running a remote command, for callers that
// need to hand the terminal over to ssh (interactive exec, setup).
func SSHCommand(dest string, tty bool, remote string) []string {
	return append(append([]string{"ssh"}, sshArgs(dest, tty)...), remote)
}

func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func unreachable(t Target, detail string) *api.Error {
	detail = strings.TrimSpace(detail)
	e := api.Errorf(api.CodeUnreachable, "cannot reach the saba server %s", t)
	if detail != "" {
		e.Message += ": " + lastLine(detail)
		e.WithDetail("output", detail)
	}
	low := strings.ToLower(detail)
	switch {
	case t.Local():
		e.WithHint("Is the daemon running? Check `systemctl status saba` (or start one with `saba daemon`). You may need to be root or in the docker group to use the socket.")
	case strings.Contains(low, "command not found") || strings.Contains(low, "no such file"):
		e.WithHint("saba is not installed on the server. Install it with `saba server setup %s`.", t.SSH)
	case strings.Contains(low, "host key verification failed"):
		e.WithHint("The server's SSH host key is not trusted yet. Run `ssh %s true` once and accept it, or set SABA_SSH_OPTS=\"-o StrictHostKeyChecking=accept-new\".", t.SSH)
	case strings.Contains(low, "permission denied"):
		e.WithHint("SSH authentication failed. Check that `ssh %s` works with your key.", t.SSH)
	case strings.Contains(low, "daemon is not running") || strings.Contains(low, "connection refused") && strings.Contains(low, "saba.sock"):
		e.WithHint("The saba daemon is not running on the server. Run `ssh %s systemctl status saba`, or `saba server setup %s` to (re)install it.", t.SSH, t.SSH)
	default:
		e.WithHint("Check that `ssh %s` works and that saba is installed there (`saba server setup %s`).", t.SSH, t.SSH)
	}
	return e
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

var _ = fmt.Sprintf
