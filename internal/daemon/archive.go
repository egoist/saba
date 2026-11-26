package daemon

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const maxContextBytes = 4 << 30 // 4 GiB unpacked

// extractTarGz unpacks a gzip-compressed tar stream into dir, refusing
// entries that would escape it.
func extractTarGz(r io.Reader, dir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("not a gzip stream: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var total int64
	// Symlinks are created last so that no later entry can be written through one.
	type link struct{ target, dest string }
	var links []link
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			root, err := filepath.EvalSymlinks(dir)
			if err != nil {
				return err
			}
			for _, l := range links {
				parent := filepath.Dir(l.target)
				if err := os.MkdirAll(parent, 0o755); err != nil {
					return err
				}
				// An earlier link must not redirect where this one is created.
				if real, err := filepath.EvalSymlinks(parent); err != nil || (real != root && !strings.HasPrefix(real, root+string(filepath.Separator))) {
					return fmt.Errorf("illegal symlink path %q in archive", l.target)
				}
				if err := os.Symlink(l.dest, l.target); err != nil {
					return fmt.Errorf("cannot create symlink %q: %w", l.target, err)
				}
			}
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(filepath.FromSlash(hdr.Name))
		if name == "." {
			continue
		}
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("illegal path %q in archive", hdr.Name)
		}
		target := filepath.Join(dir, name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(hdr.Mode).Perm() | 0o600
			f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
			if err != nil {
				return err
			}
			n, err := io.Copy(f, io.LimitReader(tr, maxContextBytes-total+1))
			f.Close()
			if err != nil {
				return err
			}
			if total += n; total > maxContextBytes {
				return fmt.Errorf("source is larger than %d GiB", maxContextBytes>>30)
			}
		case tar.TypeSymlink:
			// Links are kept as-is; docker resolves them inside the build context.
			links = append(links, link{target, hdr.Linkname})
		}
	}
}
