package setup

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// backup copies an existing file to <path>.bak-<timestamp> (or
// <path>.bak-<timestamp>-N if that exists) before setup changes it, and
// returns the copy's path ("" if path does not exist).
func backup(path string, now time.Time) (string, error) {
	return backupInto(path, filepath.Dir(path), 0, now)
}

// backupInto is backup with the copy placed in dir; a non-zero perm
// replaces the source's file mode.
func backupInto(path, dir string, perm os.FileMode, now time.Time) (string, error) {
	src, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer src.Close()
	info, err := src.Stat()
	if err != nil {
		return "", err
	}
	if perm == 0 {
		perm = info.Mode().Perm()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	base := filepath.Join(dir, filepath.Base(path)) + ".bak-" + now.Format("20060102150405")
	dst := base
	var out *os.File
	for n := 2; ; n++ {
		out, err = os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
		if !errors.Is(err, os.ErrExist) {
			break
		}
		dst = fmt.Sprintf("%s-%d", base, n)
	}
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		return "", err
	}
	return dst, out.Close()
}

// writeAtomic replaces path with data in one rename, so an interrupted
// setup never leaves a partial file.
func writeAtomic(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".setup-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), perm); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// replaceFile backs up path (if it exists) and writes data atomically,
// skipping both when the content is unchanged. It reports whether it wrote.
// A symlinked path (e.g. managed by a dotfile manager) is written through:
// the link's final target is backed up and replaced and the link is kept.
func replaceFile(path string, data []byte, perm os.FileMode, now time.Time) (bool, error) {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	if old, err := os.ReadFile(path); err == nil && string(old) == string(data) {
		return false, nil
	}
	if _, err := backup(path, now); err != nil {
		return false, err
	}
	return true, writeAtomic(path, data, perm)
}
