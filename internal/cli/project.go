package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/egoist/saba/internal/api"
)

const ProjectFile = "saba.toml"

// Project is a parsed saba.toml.
type Project struct {
	Name string
	// Context pins the project to a named server context.
	Context string
	// Env holds non-secret variables declared in the file.
	Env map[string]string
	// Config is everything else, sent to the server as a config merge patch.
	Config map[string]any
	Path   string
}

// LoadProject reads saba.toml from dir. It returns os.ErrNotExist if there is none.
func LoadProject(dir string) (*Project, error) {
	path := filepath.Join(dir, ProjectFile)
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	raw := map[string]any{}
	if err := toml.Unmarshal(b, &raw); err != nil {
		return nil, api.Errorf(api.CodeInvalid, "cannot parse %s: %v", path, err).
			WithHint("Fix the TOML syntax. `saba init` writes a commented example.")
	}
	p := &Project{Path: path, Env: map[string]string{}, Config: map[string]any{}}
	for k, v := range raw {
		switch k {
		case "name":
			s, ok := v.(string)
			if !ok {
				return nil, api.Errorf(api.CodeInvalid, "%s: `name` must be a string", path)
			}
			p.Name = s
		case "context", "server":
			s, ok := v.(string)
			if !ok {
				return nil, api.Errorf(api.CodeInvalid, "%s: `%s` must be a string", path, k)
			}
			p.Context = s
		case "env":
			m, ok := v.(map[string]any)
			if !ok {
				return nil, api.Errorf(api.CodeInvalid, "%s: `env` must be a table of KEY = \"value\" pairs", path)
			}
			for ek, ev := range m {
				switch val := ev.(type) {
				case string:
					p.Env[ek] = val
				case int64, float64, bool:
					p.Env[ek] = fmt.Sprint(val)
				default:
					return nil, api.Errorf(api.CodeInvalid, "%s: env.%s must be a string, number or boolean", path, ek)
				}
			}
		default:
			p.Config[k] = v
		}
	}
	if p.Name != "" {
		if err := api.ValidateName(p.Name); err != nil {
			return nil, err
		}
	}
	return p, nil
}

func projectTemplate(name string, port int, image string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# saba.toml — how this app is deployed. Full reference: `saba guide`\n")
	fmt.Fprintf(&b, "name = %q\n", name)
	b.WriteString("# context = \"prod\"             # pin this project to a server (see `saba context ls`)\n\n")
	if image != "" {
		fmt.Fprintf(&b, "image = %q            # deploy this image instead of building the directory\n", image)
	} else {
		b.WriteString("# image = \"nginx:alpine\"       # deploy a prebuilt image instead of building this directory\n")
	}
	if port > 0 {
		fmt.Fprintf(&b, "port = %d                    # port the app listens on (also passed as $PORT)\n", port)
	} else {
		b.WriteString("# port = 3000                  # port the app listens on; default: the image's EXPOSE\n")
	}
	b.WriteString(`# domains = ["example.com"]    # default: <name>.<server base domain>
# replicas = 1
# command = "node server.js"   # override the image's default command
# release_command = "npm run migrate"   # runs to completion before the new version gets traffic
# strategy = "rolling"         # "rolling" (zero downtime) or "recreate" (stop old, then start new)
# volumes = ["data:/data"]     # persistent volume "data" mounted at /data
# worker = false               # true for apps that do not serve HTTP

# [health]
# path = "/healthz"            # must answer 2xx/3xx before traffic is switched
# timeout = "60s"

# [resources]
# memory = "512m"
# cpus = "1"

# [env]                        # non-secret variables; use ` + "`saba env set`" + ` for secrets
# NODE_ENV = "production"

# [[cron]]
# name = "cleanup"
# schedule = "0 3 * * *"       # five-field cron, UTC
# command = "node scripts/cleanup.js"
`)
	return b.String()
}
