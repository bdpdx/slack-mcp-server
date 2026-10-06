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
