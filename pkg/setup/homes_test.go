package setup

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiscoverHomes(t *testing.T) {
	user := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(user, ".claude"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(user, ".codex"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(user, ".codex", "slack-mcp-server.env"), []byte("SLACK_MCP_XOXB_TOKEN=x\n"), 0o600))
	extra := filepath.Join(user, "agents", "codex-2")
	require.NoError(t, os.MkdirAll(extra, 0o700))
	st := &State{Homes: []HomeState{{Path: extra, Type: TypeCodex}}}

	homes := DiscoverHomes(user, st)
	require.Len(t, homes, 3)
	assert.Equal(t, Home{Path: filepath.Join(user, ".claude"), Type: TypeClaude}, homes[0])
	assert.Equal(t, Home{Path: filepath.Join(user, ".codex"), Type: TypeCodex, HasEnv: true}, homes[1])
	assert.Equal(t, extra, homes[2].Path)
}

func TestExpandPathAndDetectType(t *testing.T) {
	user := t.TempDir()
	p, err := ExpandPath("~/My Agents/codex", user)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(user, "My Agents", "codex"), p)

	claude := filepath.Join(user, "c")
	require.NoError(t, os.MkdirAll(claude, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(claude, "settings.json"), []byte("{}"), 0o600))
	assert.Equal(t, TypeClaude, DetectType(claude))
	codex := filepath.Join(user, "x")
	require.NoError(t, os.MkdirAll(codex, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(codex, "config.toml"), []byte(""), 0o600))
	assert.Equal(t, TypeCodex, DetectType(codex))
	assert.Equal(t, "", DetectType(filepath.Join(user, "none")))
}

func TestExistingBinFromHooks(t *testing.T) {
	user := t.TempDir()
	claude := filepath.Join(user, ".claude")
	require.NoError(t, os.MkdirAll(claude, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(claude, "settings.json"), []byte(
		`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/u/.bin/slack-mcp-server chat --env-file /u/.claude/slack-mcp-server.env stop-hook"}]}]}}`), 0o600))
	assert.Equal(t, "/u/.bin/slack-mcp-server", ExistingBin([]Home{{Path: claude, Type: TypeClaude}}))
	assert.Equal(t, "", ExistingBin(nil))
}
