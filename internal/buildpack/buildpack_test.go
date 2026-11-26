package buildpack

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/egoist/saba/internal/api"
)

func project(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestDetect(t *testing.T) {
	cases := []struct {
		name    string
		files   map[string]string
		builder string
		port    int
		want    []string
	}{
		{"dockerfile wins", map[string]string{"Dockerfile": "FROM scratch", "package.json": "{}"}, "dockerfile", 0, nil},
		{"npm", map[string]string{"package.json": `{"scripts":{"start":"node s.js","build":"tsc"}}`, "package-lock.json": "{}"},
			"node", 3000, []string{"npm ci", "RUN npm run build", `CMD ["sh","-c","npm run start"]`, "EXPOSE 3000"}},
		{"pnpm", map[string]string{"package.json": `{"scripts":{"start":"node s.js"}}`, "pnpm-lock.yaml": ""},
			"node", 3000, []string{"corepack enable", "pnpm install --frozen-lockfile"}},
		{"bun", map[string]string{"package.json": `{"main":"index.ts"}`, "bun.lock": ""},
			"bun", 3000, []string{"FROM oven/bun:1", "bun install --frozen-lockfile", "bun index.ts"}},
		{"procfile", map[string]string{"package.json": `{}`, "Procfile": "web: node custom.js --flag\n"},
			"node", 3000, []string{"node custom.js --flag"}},
		{"go", map[string]string{"go.mod": "module x", "main.go": "package main\nfunc main(){}"},
			"go", 8080, []string{"go build", "-o /out/app .", "EXPOSE 8080"}},
		{"go cmd dir", map[string]string{"go.mod": "module x", "cmd/api/main.go": "package main\nfunc main(){}"},
			"go", 8080, []string{"-o /out/app ./cmd/api"}},
		{"python", map[string]string{"requirements.txt": "flask", "app.py": ""},
			"python", 8000, []string{"pip install --no-cache-dir -r requirements.txt", "python app.py"}},
		{"rust", map[string]string{"Cargo.toml": "[package]\nname = \"svc\"\n"},
			"rust", 8080, []string{"target/release/svc"}},
		{"static", map[string]string{"index.html": "<h1>hi</h1>"}, "static", 80, []string{"FROM nginx:alpine", "COPY . /usr/share/nginx/html"}},
		{"static dist", map[string]string{"dist/index.html": "x"}, "static", 80, []string{"COPY dist /usr/share/nginx/html"}},
	}
	for _, c := range cases {
		det, err := Detect(project(t, c.files))
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if det.Builder != c.builder || det.Port != c.port {
			t.Errorf("%s: got %s/%d, want %s/%d", c.name, det.Builder, det.Port, c.builder, c.port)
		}
		for _, w := range c.want {
			if !strings.Contains(det.Dockerfile, w) {
				t.Errorf("%s: Dockerfile lacks %q:\n%s", c.name, w, det.Dockerfile)
			}
		}
	}
}

func TestDetectFailuresAreActionable(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"empty":           {"README.md": "hi"},
		"node no start":   {"package.json": `{"name":"x"}`},
		"python no entry": {"requirements.txt": "flask"},
	} {
		_, err := Detect(project(t, files))
		var ae *api.Error
		if !errors.As(err, &ae) || ae.Code != api.CodeBuildFailed || ae.Hint == "" {
			t.Errorf("%s: want a build_failed error with a hint, got %v", name, err)
		}
	}
}

func TestResolve(t *testing.T) {
	dir := project(t, map[string]string{"index.html": "x", "docker/Dockerfile.prod": "FROM scratch"})
	work := t.TempDir()

	plan, err := Resolve(dir, work, nil)
	if err != nil || plan.Builder != "static" || filepath.Dir(plan.Dockerfile) != work {
		t.Fatalf("generated Dockerfile should live outside the context: %+v %v", plan, err)
	}
	if _, err := os.Stat(plan.Dockerfile + ".dockerignore"); err != nil {
		t.Fatalf("expected a generated ignore file: %v", err)
	}

	plan, err = Resolve(dir, work, &api.BuildConfig{Dockerfile: "docker/Dockerfile.prod"})
	if err != nil || plan.Builder != "dockerfile" || !strings.HasSuffix(plan.Dockerfile, "docker/Dockerfile.prod") {
		t.Fatalf("custom dockerfile: %+v %v", plan, err)
	}
	if _, err := Resolve(dir, work, &api.BuildConfig{Dockerfile: "../../etc/passwd"}); err == nil {
		t.Fatal("a Dockerfile outside the project must be rejected")
	}
	if _, err := Resolve(dir, work, &api.BuildConfig{Dockerfile: "nope"}); err == nil {
		t.Fatal("a missing Dockerfile must be rejected")
	}
}
