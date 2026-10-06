// Package filesdir is the one local folder through which files move between
// Slack and this machine: attachment_get_data saves downloads into it and
// files_upload reads uploads only from it. Confining both directions to one
// folder means a caller (possibly a prompt-injected agent) can neither plant
// files elsewhere nor upload arbitrary local files such as SSH keys; an agent
// that wants to send a file copies it here with its own, permission-checked
// tools.
package filesdir

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// EnvVar overrides the folder's location.
const EnvVar = "SLACK_MCP_FILES_DIR"

// DefaultRel is the default location, relative to the home directory.
const DefaultRel = "Downloads/slack-mcp"

// ErrTooLarge is returned when a file exceeds the size limit.
var ErrTooLarge = errors.New("file exceeds the maximum size")

// Path returns the configured folder as an absolute path: EnvVar if set
// ("~/" expanded), else ~/Downloads/slack-mcp.
func Path(getenv func(string) string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("finding the home directory: %w", err)
	}
	dir := strings.TrimSpace(getenv(EnvVar))
	switch {
	case dir == "":
		dir = filepath.Join(home, DefaultRel)
	case dir == "~":
		dir = home
	case strings.HasPrefix(dir, "~/"):
		dir = filepath.Join(home, dir[2:])
	}
	return filepath.Abs(dir)
}

// Ensure creates dir if needed and makes it private to this user.
func Ensure(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("securing %s: %w", dir, err)
	}
	return nil
}

// SafeName reduces a Slack filename to a plain name safe to create in the
// folder: no path separators, no leading dots, no control characters.
func SafeName(name string) string {
	name = strings.Map(func(r rune) rune {
		switch {
		case r == '/' || r == '\\' || r < 0x20 || r == 0x7f:
			return '_'
		}
		return r
	}, name)
	name = strings.TrimLeft(strings.TrimSpace(name), ".")
	if name == "" {
		return "file"
	}
	if len(name) > 200 {
		ext := filepath.Ext(name)
		if len(ext) > 20 {
			ext = ""
		}
		name = name[:200-len(ext)] + ext
	}
	return name
}

// Save writes at most max bytes from r into dir under name (made safe),
// never overwriting: an existing name gets " (1)", " (2)", ... before its
// extension. The file is written to a hidden temporary file first, so a
// failed or oversized download leaves nothing behind. It returns the path.
func Save(dir, name string, r io.Reader, max int64) (string, int64, error) {
	tmp, err := os.CreateTemp(dir, ".partial-*")
	if err != nil {
		return "", 0, err
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, io.LimitReader(r, max+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", 0, err
	}
	if n > max {
		return "", 0, ErrTooLarge
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return "", 0, err
	}
	name = SafeName(name)
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 0; i < 1000; i++ {
		candidate := name
		if i > 0 {
			candidate = fmt.Sprintf("%s (%d)%s", stem, i, ext)
		}
		path := filepath.Join(dir, candidate)
		// A hard link fails if the name exists, so nothing is overwritten.
		if err := os.Link(tmp.Name(), path); err == nil {
			return path, n, nil
		} else if !errors.Is(err, os.ErrExist) {
			return "", 0, err
		}
	}
	return "", 0, fmt.Errorf("no free name for %s in %s", name, dir)
}

// Open opens a regular file inside dir for upload. path may be relative to
// dir or absolute; symlinks and ".." are resolved first, and the result must
// still lie inside dir. The file must be at most max bytes.
func Open(dir, path string, max int64) (*os.File, os.FileInfo, error) {
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("files folder %s: %w", dir, err)
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	rel, err := filepath.Rel(realDir, real)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return nil, nil, fmt.Errorf("%s is outside the files folder %s; copy the file there first", path, dir)
	}
	// O_NOFOLLOW: refuse if the resolved path was swapped for a symlink.
	f, err := os.OpenFile(real, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, nil, fmt.Errorf("%s is not a regular file", path)
	}
	if info.Size() > max {
		f.Close()
		return nil, nil, ErrTooLarge
	}
	if info.Size() == 0 {
		f.Close()
		return nil, nil, fmt.Errorf("%s is empty", path)
	}
	return f, info, nil
}
