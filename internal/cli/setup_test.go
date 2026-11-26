package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/egoist/saba/internal/shellwords"
)

func TestReleaseTag(t *testing.T) {
	cases := []struct {
		requested, cli, want string
		wantErr              bool
	}{
		{"", "v1.2.3", "v1.2.3", false}, // release CLI: the server gets the same version
		{"", "v1.2.3-rc.1", "v1.2.3-rc.1", false},
		{"", "dev", "", false},               // development builds install the latest release
		{"", "v1.2.3-4-gabc1234", "", false}, // git describe between tags
		{"", "3fa6b35-dirty", "", false},
		{"latest", "v1.2.3", "", false}, // explicit latest wins over the CLI version
		{"v2.0.0", "v1.2.3", "v2.0.0", false},
		{"2.0.0", "dev", "v2.0.0", false},
		{"main", "dev", "", true},
		{"v1; rm -rf /", "dev", "", true}, // never reaches a shell
	}
	for _, c := range cases {
		got, err := releaseTag(c.requested, c.cli)
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("releaseTag(%q, %q) = %q, %v; want %q (error: %v)", c.requested, c.cli, got, err, c.want, c.wantErr)
		}
	}
	if got := releaseAsset(releasesURL, "", "saba-linux-arm64"); got != "https://github.com/egoist/saba/releases/latest/download/saba-linux-arm64" {
		t.Errorf("latest URL = %s", got)
	}
	if got := releaseAsset(releasesURL, "v1.2.3", "saba-linux-amd64"); got != "https://github.com/egoist/saba/releases/download/v1.2.3/saba-linux-amd64" {
		t.Errorf("tagged URL = %s", got)
	}
}

// Runs the real server-side install script against a local "release".
func TestInstallScript(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		if _, err := exec.LookPath("wget"); err != nil {
			t.Skip("neither curl nor wget available")
		}
	}
	const asset = "saba-linux-amd64"
	good := "#!/bin/sh\necho saba v9.9.9\n"
	sum := func(s string) string {
		h := sha256.Sum256([]byte(s))
		return hex.EncodeToString(h[:])
	}
	// One fake release per scenario; all defined before the server starts.
	files := map[string]string{
		// complete release with checksums
		"/download/v9.9.9/" + asset:      good,
		"/download/v9.9.9/checksums.txt": sum("other") + "  saba-darwin-arm64\n" + sum(good) + "  " + asset + "\n",
		// binary does not match its checksum
		"/download/v1.0.0/" + asset:      "#!/bin/sh\necho tampered\n",
		"/download/v1.0.0/checksums.txt": sum(good) + "  " + asset + "\n",
		// not an executable at all, and no checksum file
		"/download/v2.0.0/" + asset: "<html>not a binary</html>",
		// latest, without a checksum file
		"/latest/download/" + asset: "#!/bin/sh\necho saba v10.0.0\n",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "saba")
	run := func(tag string) (string, error) {
		script := installScript(dest, releaseAsset(srv.URL, tag, asset), releaseAsset(srv.URL, tag, "checksums.txt"), asset)
		// Quoted exactly as it is sent through ssh to the remote shell.
		out, err := exec.Command("sh", "-c", "sh -c "+shellwords.Quote(script)).CombinedOutput()
		return string(out), err
	}
	installed := func() string {
		b, _ := os.ReadFile(dest)
		return string(b)
	}

	// A release with checksums installs and is executable.
	if out, err := run("v9.9.9"); err != nil {
		t.Fatalf("install failed: %v\n%s", err, out)
	}
	if out, err := exec.Command(dest, "--version").Output(); err != nil || strings.TrimSpace(string(out)) != "saba v9.9.9" {
		t.Fatalf("installed binary does not run: %q %v", out, err)
	}

	// Every failure below must leave the working installation in place.
	if out, err := run("v1.0.0"); err == nil || !strings.Contains(out, "checksum mismatch") {
		t.Fatalf("a checksum mismatch must fail: %v\n%s", err, out)
	}
	if out, err := run("v0.0.1"); err == nil || !strings.Contains(out, "could not download") {
		t.Fatalf("a missing release must fail: %v\n%s", err, out)
	}
	if out, err := run("v2.0.0"); err == nil || !strings.Contains(out, "not a working saba binary") {
		t.Fatalf("a non-executable download must fail: %v\n%s", err, out)
	}
	if installed() != good {
		t.Fatalf("a failed install replaced the working binary with %q", installed())
	}

	// Releases without a checksum file still install; "latest" uses its own URL.
	if out, err := run(""); err != nil {
		t.Fatalf("latest install failed: %v\n%s", err, out)
	}
	if !strings.Contains(installed(), "v10.0.0") {
		t.Fatalf("latest was not installed: %q", installed())
	}
}
