package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/egoist/saba/internal/api"
	"github.com/egoist/saba/internal/client"
	"github.com/egoist/saba/internal/daemon"
	"github.com/egoist/saba/internal/shellwords"
)

const releasesURL = "https://github.com/egoist/saba/releases"

var releaseTagRe = regexp.MustCompile(`^v\d+\.\d+\.\d+(-(alpha|beta|rc)[.\d]*)?$`)

// releaseTag picks the release a server should run: the requested one, else
// the CLI's own version when it is a release build (so client and server
// match), else "" for the latest release.
func releaseTag(requested, cliVersion string) (string, error) {
	if requested != "" && requested != "latest" {
		given := requested
		if !strings.HasPrefix(requested, "v") {
			requested = "v" + requested
		}
		if !releaseTagRe.MatchString(requested) {
			return "", api.Errorf(api.CodeInvalid, "invalid version %q", given).
				WithHint("Use a release tag like v1.2.3, or \"latest\". Releases: %s", releasesURL)
		}
		return requested, nil
	}
	if requested == "" && releaseTagRe.MatchString(cliVersion) {
		return cliVersion, nil
	}
	return "", nil
}

// releaseAsset returns the download URL of a file attached to a release.
func releaseAsset(base, tag, file string) string {
	if tag == "" {
		return base + "/latest/download/" + file
	}
	return base + "/download/" + tag + "/" + file
}

// installScript is the shell script run on the server to download a release
// binary to dest. The binary is verified against the release's checksums and
// must run before it replaces the current one, so a bad download never takes
// a working installation down.
func installScript(dest, url, sumsURL, asset string) string {
	q := shellwords.Quote
	return strings.Join([]string{
		"set -e",
		`get() { if command -v curl >/dev/null 2>&1; then curl -fsSL "$1"; elif command -v wget >/dev/null 2>&1; then wget -qO- "$1"; else echo "neither curl nor wget is installed" >&2; return 1; fi; }`,
		"new=" + q(dest+".new"),
		"get " + q(url) + ` > "$new" || { echo "could not download ` + url + `" >&2; exit 1; }`,
		"if command -v sha256sum >/dev/null 2>&1 && sums=$(get " + q(sumsURL) + " 2>/dev/null); then",
		`  want=$(printf '%s\n' "$sums" | awk -v f=` + q(asset) + ` '$2 == f { print $1 }')`,
		`  got=$(sha256sum "$new" | awk '{ print $1 }')`,
		`  if [ -n "$want" ] && [ "$want" != "$got" ]; then echo "checksum mismatch for ` + asset + `" >&2; exit 1; fi`,
		"fi",
		`chmod 755 "$new"`,
		`"$new" --version >/dev/null || { echo "the downloaded file is not a working saba binary" >&2; exit 1; }`,
		`mv -f "$new" ` + q(dest),
	}, "\n")
}

// ---- contexts --------------------------------------------------------------

func (c *CLI) contextCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "context",
		Aliases: []string{"ctx"},
		Short:   "Manage the servers this CLI can talk to",
		Long: `A context is a named server. saba reaches it over SSH, so anything that works
with "ssh <target>" works here (Host aliases from ~/.ssh/config included).

Which server a command uses, in order of precedence:
  1. --host / SABA_HOST         an SSH target, or "local"
  2. --context / SABA_CONTEXT   a named context
  3. context = "..."            in ./saba.toml
  4. the current context        (saba context use <name>)
  5. a daemon on this machine   (when running on the server itself)`,
		Example: `  saba context add prod root@203.0.113.10
  saba context use prod
  saba context ls
  saba -c staging status`,
		Args: maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error { return c.contextList() },
	}
	list := &cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "List contexts", Args: maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error { return c.contextList() },
	}
	var use bool
	add := &cobra.Command{
		Use:   "add <name> <ssh-target>",
		Short: "Add a server (the first one becomes the current context)",
		Args:  exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, target := args[0], args[1]
			if err := api.ValidateName(name); err != nil {
				return err
			}
			cfg, err := client.LoadConfig()
			if err != nil {
				return err
			}
			cfg.Contexts[name] = client.Target{SSH: target}
			if use || cfg.Current == "" {
				cfg.Current = name
			}
			if err := cfg.Save(); err != nil {
				return err
			}
			return c.result(map[string]any{"name": name, "ssh": target, "current": cfg.Current == name}, func() {
				c.ui.success("Context %s → %s saved", c.ui.bold(name), target)
				if cfg.Current == name {
					c.ui.note("It is now the current context. Verify with `saba server info`.")
				}
			})
		},
	}
	add.Flags().BoolVar(&use, "use", false, "Make it the current context")
	useCmd := &cobra.Command{
		Use:   "use <name>",
		Short: "Set the current context",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := client.LoadConfig()
			if err != nil {
				return err
			}
			if _, ok := cfg.Contexts[args[0]]; !ok && args[0] != "local" {
				return api.Errorf(api.CodeNotFound, "context %q does not exist", args[0]).
					WithHint("Known contexts: %s. Add one with `saba context add <name> <user@host>`.", orDash(strings.Join(cfg.Names(), ", ")))
			}
			cfg.Current = args[0]
			if err := cfg.Save(); err != nil {
				return err
			}
			return c.result(map[string]string{"current": args[0]}, func() { c.ui.success("Now using %s", c.ui.bold(args[0])) })
		},
	}
	remove := &cobra.Command{
		Use: "remove <name>", Aliases: []string{"rm"}, Short: "Forget a context (the server is not touched)", Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := client.LoadConfig()
			if err != nil {
				return err
			}
			if _, ok := cfg.Contexts[args[0]]; !ok {
				return api.Errorf(api.CodeNotFound, "context %q does not exist", args[0])
			}
			delete(cfg.Contexts, args[0])
			if cfg.Current == args[0] {
				cfg.Current = ""
			}
			if err := cfg.Save(); err != nil {
				return err
			}
			return c.result(map[string]string{"removed": args[0]}, func() { c.ui.success("Context %s removed", args[0]) })
		},
	}
	cmd.AddCommand(list, add, useCmd, remove)
	return cmd
}

func (c *CLI) contextList() error {
	cfg, err := client.LoadConfig()
	if err != nil {
		return err
	}
	type row struct {
		Name    string `json:"name"`
		SSH     string `json:"ssh,omitempty"`
		Socket  string `json:"socket,omitempty"`
		Current bool   `json:"current"`
	}
	rows := []row{}
	for _, n := range cfg.Names() {
		t := cfg.Contexts[n]
		rows = append(rows, row{Name: n, SSH: t.SSH, Socket: t.Socket, Current: n == cfg.Current})
	}
	return c.result(rows, func() {
		if len(rows) == 0 {
			c.ui.printf("No contexts yet. Set up a server with `saba server setup <user@host>` or add one with `saba context add <name> <user@host>`.\n")
			return
		}
		out := make([][]string, 0, len(rows))
		for _, r := range rows {
			mark := " "
			if r.Current {
				mark = c.ui.green("*")
			}
			target := r.SSH
			if target == "" {
				target = "local " + r.Socket
			}
			out = append(out, []string{mark, r.Name, target})
		}
		c.ui.table([]string{" ", "NAME", "TARGET"}, out)
	})
}

// ---- server ----------------------------------------------------------------

func (c *CLI) serverCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Set up and inspect servers",
	}

	info := &cobra.Command{
		Use: "info", Short: "Show the server's version and configuration", Args: maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := c.api()
			if err != nil {
				return err
			}
			info, err := cl.Info(cmd.Context())
			if err != nil {
				return err
			}
			return c.result(info, func() {
				field := func(k, v string) { c.ui.printf("%s %s\n", c.ui.dim(pad(k, 12)), v) }
				field("Server", cl.Target.String()+" ("+info.Hostname+")")
				field("saba", info.Version+", up "+info.Uptime)
				field("Docker", info.Docker)
				field("Apps", strconv.Itoa(info.Apps))
				if info.TLS == "auto" {
					field("HTTPS", "automatic (Let's Encrypt) on "+info.HTTPSAddr+", HTTP on "+info.HTTPAddr)
				} else {
					field("HTTPS", "off — plain HTTP on "+info.HTTPAddr)
				}
				field("Base domain", orDash(info.Config.BaseDomain))
				field("ACME email", orDash(info.Config.ACMEEmail))
			})
		},
	}

	config := &cobra.Command{
		Use:   "config",
		Short: "Show or change server settings",
		Long: `Server settings:
  base_domain     apps without custom domains are served at <app>.<base_domain>
  acme_email      contact address for Let's Encrypt (applies after a daemon restart)
  keep_versions   old versions kept per app for rollbacks (default 5)`,
		Example: "  saba server config\n  saba server config set base_domain apps.example.com",
		Args:    maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := c.api()
			if err != nil {
				return err
			}
			info, err := cl.Info(cmd.Context())
			if err != nil {
				return err
			}
			return c.result(info.Config, func() {
				c.ui.printf("base_domain   = %s\nacme_email    = %s\nkeep_versions = %d\n",
					orDash(info.Config.BaseDomain), orDash(info.Config.ACMEEmail), max(info.Config.KeepVersions, 5))
			})
		},
	}
	config.AddCommand(&cobra.Command{
		Use: "set <key> <value>", Short: "Change a server setting", Args: exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := c.api()
			if err != nil {
				return err
			}
			var value any = args[1]
			if args[0] == "keep_versions" {
				n, err := strconv.Atoi(args[1])
				if err != nil {
					return api.Errorf(api.CodeInvalid, "keep_versions must be a number")
				}
				value = n
			}
			key := strings.ReplaceAll(args[0], "-", "_")
			cfg, err := cl.PatchConfig(cmd.Context(), map[string]any{key: value})
			if err != nil {
				return err
			}
			return c.result(cfg, func() { c.ui.success("%s = %s", key, args[1]) })
		},
	})

	prune := &cobra.Command{
		Use: "prune", Short: "Free disk space (dangling images and old build cache)", Args: maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := c.api()
			if err != nil {
				return err
			}
			res, err := cl.Prune(cmd.Context())
			if err != nil {
				return err
			}
			return c.result(res, func() {
				if res.Output != "" {
					fmt.Fprintln(c.stdout, res.Output)
				}
				c.ui.success("Pruned")
			})
		},
	}

	cmd.AddCommand(c.setupCmd(), info, config, prune)
	return cmd
}

type setupFlags struct {
	name, email, baseDomain, binary, tls, version string
	installDocker, noContext                      bool
}

func (c *CLI) setupCmd() *cobra.Command {
	var f setupFlags
	cmd := &cobra.Command{
		Use:   "setup <user@host>",
		Short: "Install (or upgrade) saba on a server over SSH",
		Long: `Prepare a fresh Linux server:

  1. check for Docker (install it with --install-docker)
  2. download saba from GitHub releases to /usr/local/bin/saba
  3. install and start the saba daemon as a systemd service
  4. save the server as a context on this machine

The server downloads the build for its own architecture, so it does not matter
what machine you run this from. By default it installs the same version as this
CLI (the latest release for development builds); choose another with --version.

Requirements: SSH access as root or a user with passwordless sudo, and ports
80 and 443 free. Running it again upgrades saba in place; apps keep running.`,
		Example: `  saba server setup root@203.0.113.10
  saba server setup deploy@example.com --name prod --email ops@example.com --install-docker
  saba server setup root@host --base-domain apps.example.com   # needs a wildcard DNS record`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return c.setup(cmd.Context(), args[0], &f) },
	}
	fl := cmd.Flags()
	fl.StringVar(&f.name, "name", "", "Context name to save the server as (default: derived from the host)")
	fl.StringVar(&f.email, "email", "", "Contact email for Let's Encrypt certificates")
	fl.StringVar(&f.baseDomain, "base-domain", "auto", "Base domain for app URLs (\"auto\" = <public-ip>.sslip.io)")
	fl.StringVar(&f.tls, "tls", "auto", "\"auto\" for Let's Encrypt HTTPS, \"off\" for plain HTTP")
	fl.StringVar(&f.version, "version", "", "Release to install, e.g. v1.2.3 or \"latest\" (default: this CLI's version)")
	fl.StringVar(&f.binary, "binary", "", "Upload this local Linux binary instead of downloading a release (for unreleased builds)")
	fl.BoolVar(&f.installDocker, "install-docker", false, "Install Docker with the official script if it is missing")
	fl.BoolVar(&f.noContext, "no-context", false, "Do not save the server as a context")
	return cmd
}

func (c *CLI) setup(ctx context.Context, target string, f *setupFlags) error {
	ui := newUI(c.stderr, c.noColor)
	step := func(format string, args ...any) {
		fmt.Fprintf(c.stderr, "%s %s\n", ui.cyan("▶"), fmt.Sprintf(format, args...))
	}
	// ssh runs a remote shell snippet, optionally feeding stdin.
	ssh := func(script string, stdin io.Reader, show bool) (string, error) {
		argv := client.SSHCommand(target, false, script)
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		var out, errb bytes.Buffer
		cmd.Stdin = stdin
		cmd.Stdout = &out
		cmd.Stderr = &errb
		if show {
			cmd.Stdout = io.MultiWriter(&out, prefixWriter{c.stderr, "  " + ui.dim("│ ")})
			cmd.Stderr = io.MultiWriter(&errb, prefixWriter{c.stderr, "  " + ui.dim("│ ")})
		}
		// A persistent ssh master must not keep Run waiting on inherited pipes.
		cmd.WaitDelay = 2 * time.Second
		err := cmd.Run()
		if err != nil && err != exec.ErrWaitDelay {
			msg := strings.TrimSpace(errb.String())
			if msg == "" {
				msg = strings.TrimSpace(out.String())
			}
			return out.String(), fmt.Errorf("%s", lastLine(orDash(msg)))
		}
		return out.String(), nil
	}

	// Validate input before touching the network.
	tag, err := releaseTag(f.version, Version)
	if err != nil {
		return err
	}
	if f.binary != "" {
		if _, err := os.Stat(f.binary); err != nil {
			return api.Errorf(api.CodeInvalid, "cannot read binary %s: %v", f.binary, err)
		}
	}

	step("Connecting to %s", target)
	probe, err := ssh(`echo "os=$(uname -s) arch=$(uname -m) uid=$(id -u)"; command -v docker >/dev/null 2>&1 && echo docker=yes || echo docker=no; command -v systemctl >/dev/null 2>&1 && echo systemd=yes || echo systemd=no`, nil, false)
	if err != nil {
		return api.Errorf(api.CodeUnreachable, "cannot connect to %s: %v", target, err).
			WithHint("Check that `ssh %s` works non-interactively (key-based auth, host key accepted).", target)
	}
	facts := map[string]string{}
	for _, f := range strings.Fields(probe) {
		if k, v, ok := strings.Cut(f, "="); ok {
			facts[k] = v
		}
	}
	if facts["os"] != "Linux" {
		return api.Errorf(api.CodeInvalid, "the server runs %s; saba servers must run Linux", orDash(facts["os"]))
	}
	if facts["systemd"] != "yes" {
		return api.Errorf(api.CodeInvalid, "the server does not use systemd").
			WithHint("Run the daemon yourself with `saba daemon` under your init system.")
	}
	arch := map[string]string{"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[facts["arch"]]
	if arch == "" {
		return api.Errorf(api.CodeInvalid, "unsupported server architecture %q", facts["arch"])
	}
	sudo := ""
	if facts["uid"] != "0" {
		sudo = "sudo -n "
		if _, err := ssh("sudo -n true", nil, false); err != nil {
			return api.Errorf(api.CodeInvalid, "%s is not root and cannot use sudo without a password", target).
				WithHint("Connect as root, or allow passwordless sudo for this user.")
		}
	}

	if facts["docker"] != "yes" {
		if !f.installDocker {
			return api.Errorf(api.CodeInvalid, "Docker is not installed on the server").
				WithHint("Re-run with --install-docker to install it with the official script (https://get.docker.com).")
		}
		step("Installing Docker")
		if _, err := ssh("curl -fsSL https://get.docker.com | "+sudo+"sh", nil, true); err != nil {
			return api.Errorf(api.CodeRuntime, "installing Docker failed: %v", err)
		}
	}

	// The server downloads its own binary from GitHub releases. --binary
	// uploads a local build instead, for versions that were never released.
	const dest = "/usr/local/bin/saba"
	if f.binary != "" {
		file, err := os.Open(f.binary)
		if err != nil {
			return api.Errorf(api.CodeInvalid, "cannot read binary %s: %v", f.binary, err)
		}
		defer file.Close()
		step("Uploading %s (linux/%s)", f.binary, arch)
		upload := sudo + `sh -c 'cat > ` + dest + `.new && chmod 755 ` + dest + `.new && ` + dest + `.new --version >/dev/null && mv -f ` + dest + `.new ` + dest + `'`
		if _, err := ssh(upload, file, false); err != nil {
			return api.Errorf(api.CodeRuntime, "installing the uploaded binary failed: %v", err).
				WithHint("The binary must be built for the server: linux/%s.", arch)
		}
	} else {
		asset := "saba-linux-" + arch
		url := releaseAsset(releasesURL, tag, asset)
		label := tag
		if label == "" {
			label = "latest release"
		}
		step("Downloading saba %s (linux/%s) from GitHub", label, arch)
		script := installScript(dest, url, releaseAsset(releasesURL, tag, "checksums.txt"), asset)
		if _, err := ssh(sudo+"sh -c "+shellwords.Quote(script), nil, false); err != nil {
			return api.Errorf(api.CodeRuntime, "installing saba on the server failed: %v", err).
				WithDetail("url", url).
				WithHint("Check that the release exists (%s) and that the server can reach github.com. Pick a release with --version <tag>.", releasesURL)
		}
	}

	step("Installing the saba service")
	args := []string{"saba", "daemon", "install", "--tls", f.tls, "--base-domain", f.baseDomain}
	if f.email != "" {
		args = append(args, "--acme-email", f.email)
	}
	if _, err := ssh(sudo+shellwords.Join(args), nil, true); err != nil {
		return api.Errorf(api.CodeRuntime, "installing the service failed: %v", err).
			WithHint("Inspect it on the server: `journalctl -u saba -n 50`.")
	}

	step("Waiting for the daemon")
	cl := client.New(client.Target{Name: target, SSH: target})
	defer cl.Close()
	var info *api.ServerInfo
	deadline := time.Now().Add(45 * time.Second)
	for {
		if info, err = cl.Info(ctx); err == nil {
			break
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return api.Errorf(api.CodeUnreachable, "the daemon did not come up: %v", err).
				WithHint("Inspect it on the server: `ssh %s journalctl -u saba -n 50`. Ports 80 and 443 must be free.", target)
		}
		time.Sleep(time.Second)
	}

	name := f.name
	if !f.noContext {
		cfg, err := client.LoadConfig()
		if err != nil {
			return err
		}
		if name == "" {
			name = contextName(target, cfg)
		}
		cfg.Contexts[name] = client.Target{SSH: target}
		cfg.Current = name
		if err := cfg.Save(); err != nil {
			return err
		}
	}
	out := map[string]any{"server": target, "context": name, "info": info}
	return c.result(out, func() {
		c.ui.success("saba %s is running on %s", info.Version, c.ui.bold(target))
		if name != "" {
			c.ui.note("Saved as context %q and selected it.", name)
		}
		if info.Config.BaseDomain != "" {
			c.ui.note("Apps will be served at https://<app>.%s", info.Config.BaseDomain)
		} else {
			c.ui.note("No base domain could be detected. Set one: `saba server config set base_domain <domain>`.")
		}
		c.ui.printf("\nNext: run `saba deploy` in a project directory.\n")
	})
}

// contextName derives a short context name from an SSH target.
func contextName(target string, cfg *client.Config) string {
	host := strings.TrimPrefix(target, "ssh://")
	if i := strings.LastIndex(host, "@"); i >= 0 {
		host = host[i+1:]
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	name := api.SanitizeName(host)
	if net.ParseIP(host) != nil || name == "" {
		name = "prod"
	}
	if i := strings.IndexByte(name, '-'); net.ParseIP(host) == nil && i > 0 && strings.Contains(host, ".") {
		name = name[:i] // "web1.example.com" → "web1"
	}
	base := name
	for i := 2; ; i++ {
		if t, taken := cfg.Contexts[name]; !taken || t.SSH == target {
			return name
		}
		name = fmt.Sprintf("%s-%d", base, i)
	}
}

type prefixWriter struct {
	w      io.Writer
	prefix string
}

func (p prefixWriter) Write(b []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		fmt.Fprintf(p.w, "%s%s\n", p.prefix, strings.TrimRight(line, "\r"))
	}
	return len(b), nil
}

// ---- daemon ----------------------------------------------------------------

type daemonFlags struct {
	dataDir, socket, http, https, tls, email, baseDomain, acmeDir string
}

func (f *daemonFlags) register(cmd *cobra.Command) {
	fl := cmd.Flags()
	fl.StringVar(&f.dataDir, "data-dir", daemon.DefaultDataDir, "Directory for state, certificates and logs")
	fl.StringVar(&f.socket, "socket", daemon.DefaultSocket, "Unix socket for the control API")
	fl.StringVar(&f.http, "http", ":80", "HTTP listen address")
	fl.StringVar(&f.https, "https", ":443", "HTTPS listen address")
	fl.StringVar(&f.tls, "tls", "auto", "\"auto\" for Let's Encrypt HTTPS, \"off\" for plain HTTP only")
	fl.StringVar(&f.email, "acme-email", "", "Contact email for Let's Encrypt (stored on first start)")
	fl.StringVar(&f.baseDomain, "base-domain", "", "Base domain for app URLs; \"auto\" derives <public-ip>.sslip.io (stored on first start)")
	fl.StringVar(&f.acmeDir, "acme-directory", "", "Alternative ACME directory URL (e.g. Let's Encrypt staging)")
}

func (f *daemonFlags) args() []string {
	args := []string{"daemon", "--data-dir", f.dataDir, "--socket", f.socket, "--http", f.http, "--https", f.https, "--tls", f.tls}
	if f.email != "" {
		args = append(args, "--acme-email", f.email)
	}
	if f.baseDomain != "" {
		args = append(args, "--base-domain", f.baseDomain)
	}
	if f.acmeDir != "" {
		args = append(args, "--acme-directory", f.acmeDir)
	}
	return args
}

const unitPath = "/etc/systemd/system/saba.service"

func (c *CLI) daemonCmd() *cobra.Command {
	var f daemonFlags
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run the saba server daemon (proxy, deploy engine, API)",
		Long: `Run the server side of saba in the foreground. Normally this is started by
systemd (see "saba daemon install" or "saba server setup").

For local experiments, run it unprivileged:
  saba daemon --data-dir ./.saba --socket ./.saba/saba.sock --http :8080 --tls off --base-domain localhost
  SABA_HOST=unix://$PWD/.saba/saba.sock saba status`,
		Args: maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			level := slog.LevelInfo
			if os.Getenv("SABA_DEBUG") != "" {
				level = slog.LevelDebug
			}
			log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
			err := daemon.Run(cmd.Context(), daemon.Options{
				Version: Version, DataDir: f.dataDir, Socket: f.socket,
				HTTPAddr: f.http, HTTPSAddr: f.https, TLS: f.tls,
				ACMEEmail: f.email, BaseDomain: f.baseDomain, ACMEDirectory: f.acmeDir, Log: log,
			})
			if err != nil {
				return api.Errorf(api.CodeRuntime, "%v", err)
			}
			return nil
		},
	}
	f.register(cmd)

	var inst daemonFlags
	var printUnit bool
	install := &cobra.Command{
		Use:   "install",
		Short: "Install and start the daemon as a systemd service (run on the server, as root)",
		Args:  maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			if exe, err = filepath.EvalSymlinks(exe); err != nil {
				return err
			}
			if !printUnit && os.Geteuid() != 0 {
				return api.Errorf(api.CodeInvalid, "installing the service requires root").WithHint("Run it with sudo.")
			}
			unit := fmt.Sprintf(`[Unit]
Description=saba — app platform daemon
Documentation=https://github.com/egoist/saba
After=network-online.target docker.service
Wants=network-online.target docker.service

[Service]
ExecStart=%s
Restart=always
RestartSec=2
LimitNOFILE=1048576
# Containers are managed by Docker and must survive daemon restarts.
KillMode=process

[Install]
WantedBy=multi-user.target
`, shellwords.Join(append([]string{exe}, inst.args()...)))
			if printUnit {
				fmt.Fprint(c.stdout, unit)
				return nil
			}
			if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
				return err
			}
			for _, a := range [][]string{{"daemon-reload"}, {"enable", "saba.service"}, {"restart", "saba.service"}} {
				if out, err := exec.CommandContext(cmd.Context(), "systemctl", a...).CombinedOutput(); err != nil {
					return api.Errorf(api.CodeRuntime, "systemctl %s failed: %s", strings.Join(a, " "), strings.TrimSpace(string(out))).
						WithHint("Inspect the service with `journalctl -u saba -n 50`.")
				}
			}
			return c.result(map[string]any{"installed": true, "unit": unitPath}, func() {
				c.ui.success("saba service installed and started (%s)", unitPath)
			})
		},
	}
	inst.register(install)
	install.Flags().BoolVar(&printUnit, "print-unit", false, "Print the systemd unit instead of installing it")

	uninstall := &cobra.Command{
		Use:   "uninstall",
		Short: "Stop and remove the systemd service (apps and data are kept)",
		Args:  maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			if os.Geteuid() != 0 {
				return api.Errorf(api.CodeInvalid, "removing the service requires root").WithHint("Run it with sudo.")
			}
			_ = exec.CommandContext(cmd.Context(), "systemctl", "disable", "--now", "saba.service").Run()
			os.Remove(unitPath)
			_ = exec.CommandContext(cmd.Context(), "systemctl", "daemon-reload").Run()
			return c.result(map[string]any{"uninstalled": true}, func() {
				c.ui.success("saba service removed")
				c.ui.note("App containers keep running and data stays in %s.", daemon.DefaultDataDir)
			})
		},
	}
	cmd.AddCommand(install, uninstall)
	return cmd
}

// dialStdioCmd bridges stdin/stdout to the daemon socket. It is what the CLI
// runs on the server through SSH, so the daemon never needs a network port.
func (c *CLI) dialStdioCmd() *cobra.Command {
	var socket string
	cmd := &cobra.Command{
		Use:    "dial-stdio",
		Short:  "Proxy stdin/stdout to the daemon socket (used by the CLI over SSH)",
		Hidden: true,
		Args:   maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			if socket == "" {
				socket = os.Getenv("SABA_SOCKET")
			}
			if socket == "" {
				socket = daemon.DefaultSocket
			}
			conn, err := net.Dial("unix", socket)
			if err != nil {
				fmt.Fprintf(os.Stderr, "saba daemon is not running or not accessible at %s: %v\n", socket, err)
				return exitError{1}
			}
			defer conn.Close()
			done := make(chan struct{}, 2)
			go func() {
				_, _ = io.Copy(conn, os.Stdin)
				if uc, ok := conn.(*net.UnixConn); ok {
					_ = uc.CloseWrite()
				}
				done <- struct{}{}
			}()
			go func() {
				_, _ = io.Copy(os.Stdout, conn)
				done <- struct{}{}
			}()
			// The response side ending means the exchange is over.
			<-done
			select {
			case <-done:
			case <-time.After(500 * time.Millisecond):
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&socket, "socket", "", "Daemon socket path")
	return cmd
}
