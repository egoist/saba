package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/egoist/saba/internal/api"
	"github.com/egoist/saba/internal/client"
	"github.com/egoist/saba/internal/shellwords"
)

// remoteHost prepares a machine over SSH: shared by `saba server setup` and
// `saba node add`.
type remoteHost struct {
	c      *CLI
	ctx    context.Context
	target string
	role   string // "server" or "node", for messages
	ui     *UI
	sudo   string // "sudo -n " when not root
	arch   string
	facts  map[string]string
}

func (c *CLI) newRemote(ctx context.Context, target, role string) *remoteHost {
	return &remoteHost{c: c, ctx: ctx, target: target, role: role, ui: newUI(c.stderr, c.noColor), facts: map[string]string{}}
}

func (h *remoteHost) step(format string, args ...any) {
	if h.c.jsonOut {
		return
	}
	fmt.Fprintf(h.c.stderr, "%s %s\n", h.ui.cyan("▶"), fmt.Sprintf(format, args...))
}

// run executes a shell snippet on the host, optionally feeding stdin.
func (h *remoteHost) run(script string, stdin io.Reader, show bool) (string, error) {
	argv := client.SSHCommand(h.target, false, script)
	cmd := exec.CommandContext(h.ctx, argv[0], argv[1:]...)
	var out, errb bytes.Buffer
	cmd.Stdin = stdin
	cmd.Stdout, cmd.Stderr = &out, &errb
	if show && !h.c.jsonOut {
		cmd.Stdout = io.MultiWriter(&out, prefixWriter{h.c.stderr, "  " + h.ui.dim("│ ")})
		cmd.Stderr = io.MultiWriter(&errb, prefixWriter{h.c.stderr, "  " + h.ui.dim("│ ")})
	}
	// A persistent ssh master must not keep Run waiting on inherited pipes.
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Run(); err != nil && err != exec.ErrWaitDelay {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		return out.String(), fmt.Errorf("%s", remoteError(msg))
	}
	return out.String(), nil
}

// probe connects and checks the OS, architecture, init system and privileges.
func (h *remoteHost) probe() error {
	h.step("Connecting to %s", h.target)
	out, err := h.run(`echo "os=$(uname -s) arch=$(uname -m) uid=$(id -u)"; command -v docker >/dev/null 2>&1 && echo docker=yes || echo docker=no; command -v systemctl >/dev/null 2>&1 && echo systemd=yes || echo systemd=no`, nil, false)
	if err != nil {
		return api.Errorf(api.CodeUnreachable, "cannot connect to %s: %v", h.target, err).
			WithHint("Check that `ssh %s` works non-interactively (key-based auth, host key accepted).", h.target)
	}
	for _, f := range strings.Fields(out) {
		if k, v, ok := strings.Cut(f, "="); ok {
			h.facts[k] = v
		}
	}
	if h.facts["os"] != "Linux" {
		return api.Errorf(api.CodeInvalid, "the %s runs %s; saba needs Linux", h.role, orDash(h.facts["os"]))
	}
	if h.facts["systemd"] != "yes" {
		cmd := "saba daemon"
		if h.role == "node" {
			cmd = "saba agent run"
		}
		return api.Errorf(api.CodeInvalid, "the %s does not use systemd", h.role).
			WithHint("Install saba by hand and run `%s` under your init system.", cmd)
	}
	h.arch = map[string]string{"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[h.facts["arch"]]
	if h.arch == "" {
		return api.Errorf(api.CodeInvalid, "unsupported architecture %q", h.facts["arch"])
	}
	if h.facts["uid"] != "0" {
		h.sudo = "sudo -n "
		if _, err := h.run("sudo -n true", nil, false); err != nil {
			return api.Errorf(api.CodeInvalid, "%s is not root and cannot use sudo without a password", h.target).
				WithHint("Connect as root, or allow passwordless sudo for this user.")
		}
	}
	return nil
}

func (h *remoteHost) ensureDocker(install bool) error {
	if h.facts["docker"] == "yes" {
		return nil
	}
	if !install {
		return api.Errorf(api.CodeInvalid, "Docker is not installed on the %s", h.role).
			WithHint("Re-run with --install-docker to install it with the official script (https://get.docker.com).")
	}
	h.step("Installing Docker")
	if _, err := h.run("curl -fsSL https://get.docker.com | "+h.sudo+"sh", nil, true); err != nil {
		return api.Errorf(api.CodeRuntime, "installing Docker failed: %v", err)
	}
	return nil
}

// installBinary puts saba at /usr/local/bin/saba: downloaded from GitHub
// releases by the host itself (tag "" = latest), or uploaded from a local
// build with binary.
func (h *remoteHost) installBinary(binary, tag string) error {
	const dest = "/usr/local/bin/saba"
	if binary != "" {
		file, err := os.Open(binary)
		if err != nil {
			return api.Errorf(api.CodeInvalid, "cannot read binary %s: %v", binary, err)
		}
		defer file.Close()
		h.step("Uploading %s (linux/%s)", binary, h.arch)
		upload := h.sudo + `sh -c 'cat > ` + dest + `.new && chmod 755 ` + dest + `.new && ` + dest + `.new --version >/dev/null && mv -f ` + dest + `.new ` + dest + `'`
		if _, err := h.run(upload, file, false); err != nil {
			return api.Errorf(api.CodeRuntime, "installing the uploaded binary failed: %v", err).
				WithHint("The binary must be built for the %s: linux/%s.", h.role, h.arch)
		}
		return nil
	}
	asset := "saba-linux-" + h.arch
	url := releaseAsset(releasesURL, tag, asset)
	label := tag
	if label == "" {
		label = "latest release"
	}
	h.step("Downloading saba %s (linux/%s) from GitHub", label, h.arch)
	script := installScript(dest, url, releaseAsset(releasesURL, tag, "checksums.txt"), asset)
	if _, err := h.run(h.sudo+"sh -c "+shellwords.Quote(script), nil, false); err != nil {
		return api.Errorf(api.CodeRuntime, "installing saba on the %s failed: %v", h.role, err).
			WithDetail("url", url).
			WithHint("Check that the release exists (%s) and that the %s can reach github.com. Pick a release with --version <tag>.", releasesURL, h.role)
	}
	return nil
}

// remoteError picks the line that explains a remote failure: a saba error
// ("✗ message") when the remote command was saba, else the last line.
func remoteError(output string) string {
	for _, line := range strings.Split(output, "\n") {
		if msg, ok := strings.CutPrefix(strings.TrimSpace(line), "✗ "); ok {
			return msg
		}
	}
	return lastLine(orDash(output))
}
