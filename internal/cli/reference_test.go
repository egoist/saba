package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const referencePath = "../../docs/cli-reference.md"

// The generated CLI reference must match the command definitions. Run
// `make docs` to regenerate it after changing commands or flags.
func TestCLIReferenceIsUpToDate(t *testing.T) {
	want := Reference()
	if os.Getenv("SABA_UPDATE_DOCS") != "" {
		if err := os.WriteFile(referencePath, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(referencePath)
	if err != nil {
		t.Fatalf("cannot read %s: %v (run `make docs`)", referencePath, err)
	}
	if string(got) != want {
		t.Fatalf("%s is out of date; run `make docs`", referencePath)
	}
}

// Every relative link in the docs must point at a file that exists, and
// every `#anchor` at a heading in that file.
func TestDocsLinksResolve(t *testing.T) {
	files, _ := filepath.Glob("../../docs/*.md")
	files = append(files, "../../README.md")
	if len(files) < 2 {
		t.Skip("no docs")
	}
	linkRe := regexp.MustCompile(`\]\(([^)\s]+)\)`)
	headingRe := regexp.MustCompile("(?m)^#{1,6} (.+)$")
	anchors := map[string]map[string]bool{}
	anchorsOf := func(path string) map[string]bool {
		if a, ok := anchors[path]; ok {
			return a
		}
		a := map[string]bool{}
		b, _ := os.ReadFile(path)
		inFence := false
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "```") {
				inFence = !inFence
			}
			if m := headingRe.FindStringSubmatch(line); m != nil && !inFence {
				a[githubAnchor(m[1])] = true
			}
		}
		anchors[path] = a
		return a
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range linkRe.FindAllStringSubmatch(string(b), -1) {
			link := m[1]
			if strings.Contains(link, "://") || strings.HasPrefix(link, "mailto:") {
				continue
			}
			target, frag, _ := strings.Cut(link, "#")
			path := f
			if target != "" {
				path = filepath.Join(filepath.Dir(f), target)
				if _, err := os.Stat(path); err != nil {
					t.Errorf("%s: broken link %q", filepath.Base(f), link)
					continue
				}
			}
			if frag != "" && strings.HasSuffix(path, ".md") && !anchorsOf(path)[frag] {
				t.Errorf("%s: link %q points at a missing heading", filepath.Base(f), link)
			}
		}
	}
}

// githubAnchor approximates GitHub's heading slugs.
func githubAnchor(heading string) string {
	h := strings.ToLower(strings.TrimSpace(heading))
	h = strings.NewReplacer("`", "", "&lt;", "", "&gt;", "").Replace(h)
	var b strings.Builder
	for _, r := range h {
		switch {
		case r == ' ':
			b.WriteRune('-')
		case r == '-' || r == '_' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
		}
	}
	return b.String()
}
