package cluster

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

var releaseTag = regexp.MustCompile(`^v\d+\.\d+\.\d+(-(alpha|beta|rc)[.\d]*)?$`)

// SelfUpgrade replaces the running binary with the given release, verified
// against the release's checksums. Development builds (versions that are not
// release tags) are never replaced.
func SelfUpgrade(ctx context.Context, releasesURL, version string) error {
	if !releaseTag.MatchString(version) {
		return fmt.Errorf("the primary runs %q, which is not a release; install the same build on this node by hand", version)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	asset := "saba-" + runtime.GOOS + "-" + runtime.GOARCH
	base := strings.TrimRight(releasesURL, "/") + "/download/" + version + "/"
	client := &http.Client{Timeout: 5 * time.Minute}
	get := func(url string) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
		}
		return io.ReadAll(io.LimitReader(resp.Body, 512<<20))
	}
	sums, err := get(base + "checksums.txt")
	if err != nil {
		return fmt.Errorf("could not download the release checksums: %w", err)
	}
	want := ""
	for _, line := range strings.Split(string(sums), "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[1] == asset {
			want = f[0]
		}
	}
	if want == "" {
		return fmt.Errorf("the checksums of %s do not list %s", version, asset)
	}
	bin, err := get(base + asset)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(bin)
	if hex.EncodeToString(sum[:]) != want {
		return fmt.Errorf("checksum mismatch for %s %s", asset, version)
	}
	tmp := exe + ".new"
	if err := os.WriteFile(tmp, bin, 0o755); err != nil {
		return err
	}
	out, err := exec.CommandContext(ctx, tmp, "--version").Output()
	if err != nil || !bytes.Contains(out, []byte(version)) {
		os.Remove(tmp)
		return errors.New("the downloaded binary does not run or reports another version")
	}
	return os.Rename(tmp, exe)
}

func osArch() string { return runtime.GOOS + "-" + runtime.GOARCH }
