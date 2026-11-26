package daemon

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

type entry struct {
	name, body, link string
	dir              bool
}

func tarball(t *testing.T, entries []entry) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body)), Typeflag: tar.TypeReg}
		switch {
		case e.dir:
			hdr.Typeflag, hdr.Mode, hdr.Size = tar.TypeDir, 0o755, 0
		case e.link != "":
			hdr.Typeflag, hdr.Linkname, hdr.Size = tar.TypeSymlink, e.link, 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag == tar.TypeReg {
			tw.Write([]byte(e.body))
		}
	}
	tw.Close()
	gz.Close()
	return &buf
}

func TestExtract(t *testing.T) {
	dir := t.TempDir()
	err := extractTarGz(tarball(t, []entry{
		{name: "src", dir: true},
		{name: "src/main.go", body: "package main"},
		{name: "deep/nested/file.txt", body: "hi"},
		{name: "link", link: "src/main.go"},
	}), dir)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "deep/nested/file.txt")); string(b) != "hi" {
		t.Fatalf("file content = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "link")); string(b) != "package main" {
		t.Fatalf("symlink not preserved: %q", b)
	}
}

// The daemon unpacks uploads as root, so archives must never write outside
// the target directory.
func TestExtractRejectsEscapes(t *testing.T) {
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim")
	for name, entries := range map[string][]entry{
		"parent traversal": {{name: "../victim", body: "pwned"}},
		"absolute path":    {{name: victim, body: "pwned"}},
		"write through symlink": {
			{name: "out", link: outside},
			{name: "out/victim", body: "pwned"},
		},
		"symlink through symlink": {
			{name: "out", link: outside},
			{name: "out/victim", link: "/etc/passwd"},
		},
	} {
		dir := t.TempDir()
		err := extractTarGz(tarball(t, entries), dir)
		if _, statErr := os.Lstat(victim); statErr == nil {
			t.Fatalf("%s: wrote outside the target directory", name)
		}
		if err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if err := extractTarGz(bytes.NewBufferString("not gzip"), t.TempDir()); err == nil {
		t.Error("garbage input should fail")
	}
}
