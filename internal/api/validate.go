package api

import (
	"github.com/egoist/saba/internal/cron"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	nameRe    = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,38}[a-z0-9])?$`)
	domainRe  = regexp.MustCompile(`^(\*\.)?([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)*[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	envKeyRe  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	volNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]*$`)
)

// ValidateName checks an app name. Names double as DNS labels and container
// name prefixes, so the rules are strict.
func ValidateName(name string) error {
	if !nameRe.MatchString(name) {
		return Errorf(CodeInvalid, "invalid app name %q", name).
			WithHint("Use 1-40 lowercase letters, digits and dashes, starting with a letter (e.g. \"my-app\").")
	}
	return nil
}

// SanitizeName turns an arbitrary string (like a directory name) into a valid app name.
func SanitizeName(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	if out == "" || out[0] < 'a' || out[0] > 'z' {
		out = "app-" + out
	}
	if len(out) > 40 {
		out = out[:40]
	}
	return strings.Trim(out, "-")
}

func ValidEnvKey(k string) bool { return envKeyRe.MatchString(k) }

// NormalizeDomain lowercases and strips scheme, path and port.
func NormalizeDomain(d string) string {
	d = strings.ToLower(strings.TrimSpace(d))
	d = strings.TrimPrefix(d, "https://")
	d = strings.TrimPrefix(d, "http://")
	if i := strings.IndexAny(d, "/:"); i >= 0 {
		d = d[:i]
	}
	return strings.TrimSuffix(d, ".")
}

// ParseDuration parses "30s", "2m" or a bare number of seconds, with a default.
func ParseDuration(s string, def time.Duration) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return def, nil
	}
	if n, err := strconv.Atoi(s); err == nil {
		return time.Duration(n) * time.Second, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, Errorf(CodeInvalid, "invalid duration %q", s).WithHint("Use values like \"30s\", \"2m\" or \"1h\".")
	}
	return d, nil
}

// Volume is a parsed volume spec.
type Volume struct {
	// Source is a volume name or an absolute host path.
	Source string
	Target string
	Bind   bool
}

func ParseVolume(spec string) (Volume, error) {
	src, dst, ok := strings.Cut(spec, ":")
	if !ok || src == "" || !strings.HasPrefix(dst, "/") {
		return Volume{}, Errorf(CodeInvalid, "invalid volume %q", spec).
			WithHint("Use \"name:/path/in/container\" for a managed volume or \"/host/path:/path/in/container\" for a bind mount.")
	}
	if strings.HasPrefix(src, "/") {
		return Volume{Source: src, Target: dst, Bind: true}, nil
	}
	if !volNameRe.MatchString(src) {
		return Volume{}, Errorf(CodeInvalid, "invalid volume name %q", src).
			WithHint("Volume names use lowercase letters, digits, dashes, dots and underscores.")
	}
	return Volume{Source: src, Target: dst}, nil
}

var publishRe = regexp.MustCompile(`^((\d{1,3}\.){3}\d{1,3}:)?\d{1,5}:\d{1,5}(/(tcp|udp))?$`)

// Validate normalizes the config in place and reports the first problem.
func (c *AppConfig) Validate() error {
	if c.Port < 0 || c.Port > 65535 {
		return Errorf(CodeInvalid, "invalid port %d", c.Port)
	}
	if c.Replicas < 0 || c.Replicas > 50 {
		return Errorf(CodeInvalid, "invalid replicas %d (must be 0-50)", c.Replicas)
	}
	switch c.Strategy {
	case "", StrategyRolling, StrategyRecreate:
	default:
		return Errorf(CodeInvalid, "invalid strategy %q", c.Strategy).WithHint("Use \"rolling\" or \"recreate\".")
	}
	seen := map[string]bool{}
	domains := c.Domains[:0:0]
	for _, d := range c.Domains {
		d = NormalizeDomain(d)
		if !domainRe.MatchString(d) || !strings.Contains(d, ".") {
			return Errorf(CodeInvalid, "invalid domain %q", d).WithHint("Use a hostname like \"app.example.com\" or \"*.example.com\".")
		}
		if !seen[d] {
			seen[d] = true
			domains = append(domains, d)
		}
	}
	c.Domains = domains
	for _, v := range c.Volumes {
		if _, err := ParseVolume(v); err != nil {
			return err
		}
	}
	for _, p := range c.Publish {
		if !publishRe.MatchString(p) {
			return Errorf(CodeInvalid, "invalid publish spec %q", p).WithHint("Use \"hostPort:containerPort\", e.g. \"5432:5432\".")
		}
	}
	seenNode := map[string]bool{}
	nodes := c.Nodes[:0:0]
	for _, n := range c.Nodes {
		n = strings.TrimSpace(n)
		if n != "*" && !nameRe.MatchString(n) {
			return Errorf(CodeInvalid, "invalid node name %q", n).WithHint("Use node names from `saba node list`, or \"*\" for all nodes.")
		}
		if !seenNode[n] {
			seenNode[n] = true
			nodes = append(nodes, n)
		}
	}
	c.Nodes = nodes
	if len(c.Volumes) > 0 && (len(c.Nodes) > 1 || seenNode["*"]) {
		return Errorf(CodeInvalid, "an app with volumes cannot be spread across nodes").
			WithHint("Volumes live on one server. Pin the app to a single node, e.g. nodes = [\"local\"].")
	}
	if len(c.Publish) > 0 && c.Replicas > 1 {
		return Errorf(CodeInvalid, "publish cannot be combined with more than one replica").
			WithHint("A host port can only be bound by one container. Remove `publish` or set replicas to 1.")
	}
	if c.Health != nil {
		if c.Health.Path != "" && !strings.HasPrefix(c.Health.Path, "/") {
			return Errorf(CodeInvalid, "health.path must start with \"/\"")
		}
		if _, err := ParseDuration(c.Health.Timeout, 0); err != nil {
			return err
		}
		if _, err := ParseDuration(c.Health.Interval, 0); err != nil {
			return err
		}
	}
	if _, err := ParseDuration(c.DrainTimeout, 0); err != nil {
		return err
	}
	if _, err := ParseDuration(c.StopTimeout, 0); err != nil {
		return err
	}
	names := map[string]bool{}
	for _, j := range c.Cron {
		if !volNameRe.MatchString(j.Name) {
			return Errorf(CodeInvalid, "invalid cron job name %q", j.Name)
		}
		if names[j.Name] {
			return Errorf(CodeInvalid, "duplicate cron job name %q", j.Name)
		}
		names[j.Name] = true
		if strings.TrimSpace(j.Command) == "" {
			return Errorf(CodeInvalid, "cron job %q has no command", j.Name)
		}
		if _, err := cron.Parse(j.Schedule); err != nil {
			return Errorf(CodeInvalid, "cron job %q: %v", j.Name, err).
				WithHint("Example: \"*/15 * * * *\" (every 15 minutes, UTC) or \"@daily\".")
		}
	}
	return nil
}

// EffectiveReplicas returns the replica count with the default applied.
func (c *AppConfig) EffectiveReplicas() int {
	if c.Replicas <= 0 {
		return 1
	}
	return c.Replicas
}

// EffectiveStrategy returns the strategy that will actually be used. Published
// host ports can only be bound by one container, which forces a recreate.
func (c *AppConfig) EffectiveStrategy() string {
	if c.Strategy == StrategyRecreate || len(c.Publish) > 0 {
		return StrategyRecreate
	}
	return StrategyRolling
}
