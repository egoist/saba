package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/egoist/saba/internal/api"
)

// sourceFiles lists the files to upload, relative to dir. Inside a git
// checkout that is every tracked or untracked-but-not-ignored file, so
// .gitignore doubles as the upload ignore list. Elsewhere it is everything
// except well-known heavy directories.
func sourceFiles(dir string) (files []string, viaGit bool, err error) {
	if out, gerr := gitOutput(dir, "ls-files", "-z", "--cached", "--others", "--exclude-standard"); gerr == nil {
		seen := map[string]bool{}
		for _, f := range strings.Split(out, "\x00") {
			if f == "" || seen[f] {
				continue
			}
			seen[f] = true
			files = append(files, f)
		}
		sort.Strings(files)
		return files, true, nil
	}
	skip := map[string]bool{".git": true, "node_modules": true, ".venv": true, "__pycache__": true, ".DS_Store": true}
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		if rel == "." {
			return nil
		}
		if skip[d.Name()] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	return files, false, err
}

func gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return out.String(), nil
}

type archive struct {
	file  *os.File
	Files int
	Bytes int64
}

func (a *archive) Close() {
	a.file.Close()
	os.Remove(a.file.Name())
}

// buildArchive writes the source of dir as a gzip-compressed tar to a temp file.
func buildArchive(dir string) (*archive, error) {
	files, _, err := sourceFiles(dir)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, api.Errorf(api.CodeInvalid, "there are no files to deploy in %s", dir).
			WithHint("Files ignored by .gitignore are not uploaded. Deploy an image instead with `--image`.")
	}
	tmp, err := os.CreateTemp("", "saba-src-*.tar.gz")
	if err != nil {
		return nil, err
	}
	a := &archive{file: tmp}
	gz := gzip.NewWriter(tmp)
	tw := tar.NewWriter(gz)
	for _, rel := range files {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		info, err := os.Lstat(path)
		if err != nil {
			continue // listed by git but deleted in the working tree
		}
		link := ""
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			if link, err = os.Readlink(path); err != nil {
				continue
			}
		case !info.Mode().IsRegular():
			continue // directories (submodules), sockets, devices
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			a.Close()
			return nil, err
		}
		hdr.Name = filepath.ToSlash(rel)
		hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
		if err := tw.WriteHeader(hdr); err != nil {
			a.Close()
			return nil, err
		}
		if link == "" {
			f, err := os.Open(path)
			if err != nil {
				a.Close()
				return nil, err
			}
			_, err = io.Copy(tw, f)
			f.Close()
			if err != nil {
				a.Close()
				return nil, err
			}
		}
		a.Files++
	}
	if err := tw.Close(); err != nil {
		a.Close()
		return nil, err
	}
	if err := gz.Close(); err != nil {
		a.Close()
		return nil, err
	}
	if a.Bytes, err = tmp.Seek(0, io.SeekCurrent); err != nil {
		a.Close()
		return nil, err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		a.Close()
		return nil, err
	}
	return a, nil
}

func humanBytes(n int64) string {
	switch {
	case n < 1<<10:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	case n < 1<<30:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%.2f GB", float64(n)/(1<<30))
	}
}
