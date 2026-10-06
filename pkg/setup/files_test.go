package setup

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testNow = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

func TestBackupAndWriteAtomic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "f.json")
	b, err := backup(p, testNow)
	require.NoError(t, err)
	assert.Empty(t, b, "nothing to back up")

	require.NoError(t, writeAtomic(p, []byte("one"), 0o600))
	b, err = backup(p, testNow)
	require.NoError(t, err)
	assert.Equal(t, p+".bak-20261005120000", b)
	require.NoError(t, writeAtomic(p, []byte("two"), 0o600))
	got, _ := os.ReadFile(b)
	assert.Equal(t, "one", string(got))
	info, _ := os.Stat(p)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestStateNeverHoldsTokens(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".install-state.json")
	st, err := LoadState(p)
	require.NoError(t, err)
	st.Bin = "/x/slack-mcp-server"
	st.SetHome(HomeState{Path: "/h/.claude", Type: TypeClaude, Bot: "claude"})
	st.SetHome(HomeState{Path: "/h/.claude", Type: TypeClaude, Bot: "claude2"})
	require.NoError(t, st.Save(p))
	again, err := LoadState(p)
	require.NoError(t, err)
	assert.Len(t, again.Homes, 1, "SetHome replaces by path")
	assert.Equal(t, "claude2", again.Home("/h/.claude").Bot)
	raw, _ := os.ReadFile(p)
	assert.NotContains(t, string(raw), "xox")
	info, _ := os.Stat(p)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestReplaceFileSkipsUnchanged(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.txt")
	require.NoError(t, os.WriteFile(p, []byte("same"), 0o600))
	wrote, err := replaceFile(p, []byte("same"), 0o600, testNow)
	require.NoError(t, err)
	assert.False(t, wrote)
	assert.NoFileExists(t, p+".bak-20261005120000")
}

func TestReplaceFileWritesThroughSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "dotfiles", "settings.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o700))
	require.NoError(t, os.WriteFile(target, []byte("old"), 0o600))
	link := filepath.Join(dir, "settings.json")
	require.NoError(t, os.Symlink(target, link))

	wrote, err := replaceFile(link, []byte("new"), 0o600, testNow)
	require.NoError(t, err)
	assert.True(t, wrote)

	info, err := os.Lstat(link)
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&os.ModeSymlink, "link stays a link")
	data, _ := os.ReadFile(target)
	assert.Equal(t, "new", string(data))
	bak, _ := os.ReadFile(target + ".bak-20261005120000")
	assert.Equal(t, "old", string(bak))
	assert.NoFileExists(t, link+".bak-20261005120000")
}
