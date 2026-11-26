// Package buildpack decides how to turn a source directory into an image.
// A Dockerfile always wins; otherwise a Dockerfile is generated for the
// detected stack.
package buildpack

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/egoist/saba/internal/api"
)

type Plan struct {
	// Builder names what was detected: dockerfile, node, bun, go, python, rust, static.
	Builder string
	// Dockerfile is the absolute path of the Dockerfile to build with.
	Dockerfile string
	// Port is the port the generated image listens on (0 for user Dockerfiles).
	Port int
	// Notes are shown to the user in the build log.
	Notes []string
}

// Detection is the result of inspecting a directory without writing anything.
type Detection struct {
	Builder    string
	Dockerfile string // generated content; empty for user Dockerfiles
	Port       int
	Notes      []string
}

// Resolve picks the Dockerfile for a build. Generated files are written to
// workDir, outside the build context.
func Resolve(contextDir, workDir string, cfg *api.BuildConfig) (*Plan, error) {
	if cfg != nil && cfg.Dockerfile != "" {
		p := filepath.Join(contextDir, filepath.FromSlash(cfg.Dockerfile))
		if !inside(contextDir, p) {
			return nil, api.Errorf(api.CodeInvalid, "build.dockerfile must be inside the project directory")
		}
		if !exists(p) {
			return nil, api.Errorf(api.CodeBuildFailed, "Dockerfile %q not found in the uploaded source", cfg.Dockerfile).
				WithHint("Check `build.dockerfile` in saba.toml. Note that files ignored by git are not uploaded.")
		}
		return &Plan{Builder: "dockerfile", Dockerfile: p}, nil
	}
	det, err := Detect(contextDir)
	if err != nil {
		return nil, err
	}
	if det.Dockerfile == "" {
		return &Plan{Builder: det.Builder, Dockerfile: filepath.Join(contextDir, "Dockerfile")}, nil
	}
	path := filepath.Join(workDir, "Dockerfile.saba")
	if err := os.WriteFile(path, []byte(det.Dockerfile), 0o644); err != nil {
		return nil, err
	}
	// BuildKit prefers <dockerfile>.dockerignore over the context's own file.
	if !exists(filepath.Join(contextDir, ".dockerignore")) {
		_ = os.WriteFile(path+".dockerignore", []byte(defaultIgnore), 0o644)
	}
	notes := append([]string{fmt.Sprintf("No Dockerfile found; generated one for a %s app. Add your own Dockerfile to take full control.", det.Builder)}, det.Notes...)
	return &Plan{Builder: det.Builder, Dockerfile: path, Port: det.Port, Notes: notes}, nil
}

const defaultIgnore = `.git
node_modules
.venv
venv
__pycache__
target
.env
.env.*
*.log
.DS_Store
`

// Detect inspects dir and reports how it would be built.
func Detect(dir string) (*Detection, error) {
	has := func(names ...string) bool {
		for _, n := range names {
			if exists(filepath.Join(dir, n)) {
				return true
			}
		}
		return false
	}
	web := procfileWeb(dir)

	switch {
	case has("Dockerfile"):
		return &Detection{Builder: "dockerfile"}, nil
	case has("package.json"):
		return detectNode(dir, web)
	case has("go.mod"):
		return detectGo(dir), nil
	case has("requirements.txt", "pyproject.toml"):
		return detectPython(dir, web)
	case has("Cargo.toml"):
		return detectRust(dir)
	case has("index.html"):
		return static("."), nil
	case has("public/index.html"):
		return static("public"), nil
	case has("dist/index.html"):
		return static("dist"), nil
	}
	return nil, api.Errorf(api.CodeBuildFailed, "could not work out how to build this project").
		WithHint("Add a Dockerfile, or deploy a prebuilt image with `saba deploy --image <ref>`. Auto-detection supports Node.js, Bun, Go, Python, Rust and static sites.")
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func inside(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && !strings.HasPrefix(rel, "..")
}

var procfileRe = regexp.MustCompile(`(?m)^web:\s*(.+)$`)

func procfileWeb(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, "Procfile"))
	if err != nil {
		return ""
	}
	if m := procfileRe.FindSubmatch(b); m != nil {
		return strings.TrimSpace(string(m[1]))
	}
	return ""
}

func shellCmd(cmd string) string {
	b, _ := json.Marshal([]string{"sh", "-c", cmd})
	return string(b)
}

func detectNode(dir, web string) (*Detection, error) {
	var pkg struct {
		Main    string            `json:"main"`
		Scripts map[string]string `json:"scripts"`
	}
	b, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &pkg); err != nil {
		return nil, api.Errorf(api.CodeBuildFailed, "package.json is not valid JSON: %v", err)
	}
	has := func(n string) bool { return exists(filepath.Join(dir, n)) }

	builder, base := "node", "node:lts-slim"
	var setup, install, run string
	lock := "package.json"
	switch {
	case has("bun.lock") || has("bun.lockb"):
		builder, base = "bun", "oven/bun:1"
		install, run = "bun install --frozen-lockfile", "bun run"
		lock = "package.json bun.lock*"
	case has("pnpm-lock.yaml"):
		setup = "RUN corepack enable\n"
		install, run = "pnpm install --frozen-lockfile", "pnpm run"
		lock = "package.json pnpm-lock.yaml"
	case has("yarn.lock"):
		setup = "RUN corepack enable\n"
		install, run = "yarn install --frozen-lockfile", "yarn run"
		lock = "package.json yarn.lock"
	case has("package-lock.json"):
		install, run = "npm ci", "npm run"
		lock = "package.json package-lock.json"
	default:
		install, run = "npm install", "npm run"
	}
	for _, extra := range []string{".npmrc", "pnpm-workspace.yaml", ".yarnrc.yml"} {
		if has(extra) {
			lock += " " + extra
		}
	}

	start := web
	switch {
	case start != "":
	case pkg.Scripts["start"] != "":
		start = run + " start"
	case pkg.Main != "":
		if builder == "bun" {
			start = "bun " + pkg.Main
		} else {
			start = "node " + pkg.Main
		}
	case has("server.js"):
		start = "node server.js"
	case has("index.js"):
		start = "node index.js"
	default:
		return nil, api.Errorf(api.CodeBuildFailed, "package.json has no \"start\" script").
			WithHint("Add a \"start\" script (e.g. \"node server.js\"), a Procfile with a `web:` line, or a Dockerfile.")
	}

	var d strings.Builder
	fmt.Fprintf(&d, "FROM %s\nWORKDIR /app\n%s", base, setup)
	fmt.Fprintf(&d, "COPY %s ./\nRUN %s\nCOPY . .\n", lock, install)
	if pkg.Scripts["build"] != "" {
		fmt.Fprintf(&d, "RUN %s build\n", run)
	}
	fmt.Fprintf(&d, "ENV NODE_ENV=production HOST=0.0.0.0 PORT=3000\nEXPOSE 3000\nCMD %s\n", shellCmd(start))
	return &Detection{Builder: builder, Dockerfile: d.String(), Port: 3000,
		Notes: []string{"Start command: " + start + " (the app must listen on $PORT, default 3000)"}}, nil
}

func detectGo(dir string) *Detection {
	pkg := "."
	if !hasGoMain(dir) {
		if entries, err := os.ReadDir(filepath.Join(dir, "cmd")); err == nil {
			for _, en := range entries {
				if en.IsDir() && hasGoMain(filepath.Join(dir, "cmd", en.Name())) {
					pkg = "./cmd/" + en.Name()
					break
				}
			}
		}
	}
	d := fmt.Sprintf(`FROM golang:1 AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app %s

FROM alpine:3
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=build /out/app /usr/local/bin/app
COPY --from=build /src /app
ENV PORT=8080
EXPOSE 8080
CMD ["app"]
`, pkg)
	return &Detection{Builder: "go", Dockerfile: d, Port: 8080,
		Notes: []string{"Building package " + pkg + " (the app must listen on $PORT, default 8080)"}}
}

func hasGoMain(dir string) bool {
	files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
	for _, f := range files {
		if b, err := os.ReadFile(f); err == nil && regexp.MustCompile(`(?m)^package main\b`).Match(b) {
			return true
		}
	}
	return false
}

func detectPython(dir, web string) (*Detection, error) {
	has := func(n string) bool { return exists(filepath.Join(dir, n)) }
	start := web
	switch {
	case start != "":
	case has("manage.py"):
		start = "python manage.py runserver 0.0.0.0:$PORT"
	case has("main.py"):
		start = "python main.py"
	case has("app.py"):
		start = "python app.py"
	default:
		return nil, api.Errorf(api.CodeBuildFailed, "could not find a Python entry point").
			WithHint("Add a Procfile with a `web:` line (e.g. `web: gunicorn app:app --bind 0.0.0.0:$PORT`), a main.py/app.py, or a Dockerfile.")
	}
	var install string
	switch {
	case has("uv.lock"):
		install = "RUN pip install --no-cache-dir uv\nCOPY . .\nRUN uv sync --frozen --no-dev\nENV PATH=/app/.venv/bin:$PATH\n"
	case has("requirements.txt"):
		install = "COPY requirements.txt ./\nRUN pip install --no-cache-dir -r requirements.txt\nCOPY . .\n"
	default:
		install = "COPY . .\nRUN pip install --no-cache-dir .\n"
	}
	d := "FROM python:3-slim\nWORKDIR /app\nENV PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1\n" + install +
		"ENV PORT=8000\nEXPOSE 8000\nCMD " + shellCmd(start) + "\n"
	return &Detection{Builder: "python", Dockerfile: d, Port: 8000,
		Notes: []string{"Start command: " + start + " (the app must listen on 0.0.0.0:$PORT, default 8000)"}}, nil
}

var cargoNameRe = regexp.MustCompile(`(?m)^name\s*=\s*"([^"]+)"`)

func detectRust(dir string) (*Detection, error) {
	b, err := os.ReadFile(filepath.Join(dir, "Cargo.toml"))
	if err != nil {
		return nil, err
	}
	m := cargoNameRe.FindSubmatch(b)
	if m == nil {
		return nil, api.Errorf(api.CodeBuildFailed, "could not read the package name from Cargo.toml").
			WithHint("Add a Dockerfile to build this project.")
	}
	name := string(m[1])
	d := fmt.Sprintf(`FROM rust:1 AS build
WORKDIR /src
COPY . .
RUN cargo build --release --locked || cargo build --release

FROM debian:stable-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=build /src/target/release/%s /usr/local/bin/app
ENV PORT=8080
EXPOSE 8080
CMD ["app"]
`, name)
	return &Detection{Builder: "rust", Dockerfile: d, Port: 8080,
		Notes: []string{"Building binary " + name + " (the app must listen on 0.0.0.0:$PORT, default 8080)"}}, nil
}

func static(root string) *Detection {
	d := fmt.Sprintf(`FROM nginx:alpine
COPY %s /usr/share/nginx/html
EXPOSE 80
`, root)
	return &Detection{Builder: "static", Dockerfile: d, Port: 80,
		Notes: []string{"Serving static files from " + root + " with nginx"}}
}
