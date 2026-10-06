package filesdir

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestUnitPath(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)

	p, err := Path(env(nil))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, "Downloads", "slack-mcp"), p)

	p, err = Path(env(map[string]string{EnvVar: "~/x/y"}))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, "x", "y"), p)

	p, err = Path(env(map[string]string{EnvVar: "/tmp/z"}))
	require.NoError(t, err)
	assert.Equal(t, "/tmp/z", p)
}

func TestUnitEnsureMakesThePrivateFolder(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "slack-mcp")
	require.NoError(t, Ensure(dir))
	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())

	require.NoError(t, os.Chmod(dir, 0o755))
	require.NoError(t, Ensure(dir))
	info, _ = os.Stat(dir)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm(), "an existing folder is tightened")
}

func TestUnitSafeName(t *testing.T) {
	assert.Equal(t, "report.pdf", SafeName("report.pdf"))
	assert.Equal(t, "_.._etc_passwd", SafeName("/../etc/passwd"))
	assert.Equal(t, "bashrc", SafeName("..bashrc"))
	assert.Equal(t, "a_b", SafeName("a\nb"))
	assert.Equal(t, "file", SafeName(""))
	assert.Equal(t, "file", SafeName(".."))
	long := SafeName(strings.Repeat("x", 300) + ".txt")
	assert.Len(t, long, 200)
	assert.True(t, strings.HasSuffix(long, ".txt"))
}

func TestUnitSaveNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	p1, n, err := Save(dir, "a.txt", strings.NewReader("one"), 10)
	require.NoError(t, err)
	assert.Equal(t, int64(3), n)
	assert.Equal(t, filepath.Join(dir, "a.txt"), p1)

	p2, _, err := Save(dir, "a.txt", strings.NewReader("two"), 10)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "a (1).txt"), p2)

	got, _ := os.ReadFile(p1)
	assert.Equal(t, "one", string(got), "the first file is untouched")
	info, _ := os.Stat(p2)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	p3, _, err := Save(dir, "../../escape.sh", strings.NewReader("x"), 10)
	require.NoError(t, err)
	assert.Equal(t, dir, filepath.Dir(p3), "a hostile name stays in the folder")
}

func TestUnitSaveTooLargeLeavesNothing(t *testing.T) {
	dir := t.TempDir()
	_, _, err := Save(dir, "big.bin", strings.NewReader(strings.Repeat("x", 11)), 10)
	assert.ErrorIs(t, err, ErrTooLarge)
	entries, _ := os.ReadDir(dir)
	assert.Empty(t, entries)
}

func TestUnitOpenOnlyInsideTheFolder(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "files")
	require.NoError(t, Ensure(dir))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ok.txt"), []byte("hello"), 0o600))
	secret := filepath.Join(root, "secret")
	require.NoError(t, os.WriteFile(secret, []byte("key"), 0o600))
	require.NoError(t, os.Symlink(secret, filepath.Join(dir, "link")))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0o700))

	f, info, err := Open(dir, "ok.txt", 10)
	require.NoError(t, err)
	f.Close()
	assert.Equal(t, int64(5), info.Size())

	f, _, err = Open(dir, filepath.Join(dir, "ok.txt"), 10)
	require.NoError(t, err, "absolute paths inside the folder work")
	f.Close()

	for _, p := range []string{"../secret", secret, "link", "sub", "missing.txt"} {
		_, _, err := Open(dir, p, 10)
		assert.Error(t, err, p)
	}
	_, _, err = Open(dir, "../secret", 10)
	assert.ErrorContains(t, err, "outside the files folder")
	_, _, err = Open(dir, "link", 10)
	assert.ErrorContains(t, err, "outside the files folder", "a symlink out of the folder is refused")

	_, _, err = Open(dir, "ok.txt", 4)
	assert.ErrorIs(t, err, ErrTooLarge)
}
