package setup

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type restartRunner struct {
	calls [][]string
	out   map[string]string
	err   map[string]error
}

func (r *restartRunner) LookPath(name string) (string, error) { return name, nil }

func (r *restartRunner) Run(env []string, name string, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	return r.out[args[2]], r.err[args[2]]
}

// Every home with an env file gets its running listener replaced; the
// summary says what happened per home.
func TestRestartListeners(t *testing.T) {
	r := &restartRunner{
		out: map[string]string{
			"/h/a/slack-mcp-server.env": `{"ok":true,"was_running":true,"previous_version":"v1","version":"v2"}`,
			"/h/b/slack-mcp-server.env": `{"ok":true,"was_running":false}`,
			"/h/d/slack-mcp-server.env": "warning: noise\n" + `{"ok":true,"was_running":true,"already_current":true,"version":"v2"}`,
			"/h/c/slack-mcp-server.env": "listener did not start",
		},
		err: map[string]error{"/h/c/slack-mcp-server.env": errors.New("exit 1")},
	}
	var w bytes.Buffer
	failed := RestartListeners(&w, r, "/bin/smcp", []Home{
		{Path: "/h/a", HasEnv: true}, {Path: "/h/b", HasEnv: true}, {Path: "/h/c", HasEnv: true}, {Path: "/h/d", HasEnv: true}, {Path: "/h/none"},
	})
	assert.True(t, failed)
	assert.Len(t, r.calls, 4, "a home without an env file is skipped")
	assert.Equal(t, []string{"/bin/smcp", "chat", "--env-file", "/h/a/slack-mcp-server.env", "listener", "restart", "--if-running"}, r.calls[0])
	got := w.String()
	assert.Contains(t, got, "/h/a: restarted on the new binary (v1 → v2)")
	assert.Contains(t, got, "/h/b: not running")
	assert.Contains(t, got, "/h/c: restart FAILED: listener did not start")
	assert.Contains(t, got, "/h/d: already running this build (v2)", "the JSON is the last line, after any noise")

	ok := &restartRunner{out: map[string]string{"/h/a/slack-mcp-server.env": `{"ok":true,"was_running":false}`}}
	assert.False(t, RestartListeners(&bytes.Buffer{}, ok, "/bin/smcp", []Home{{Path: "/h/a", HasEnv: true}}))
	assert.False(t, strings.Contains(got, "/h/none"))
}

type streamRunner struct{ restartRunner }

func (r *streamRunner) RunStream(w io.Writer, env []string, name string, args ...string) (string, error) {
	fmt.Fprintln(w, "progress for", args[2])
	return r.Run(env, name, args...)
}

// A runner that can stream shows each home's progress as it runs, then the
// result on its own line.
func TestRestartListenersStreamsProgress(t *testing.T) {
	r := &streamRunner{restartRunner{out: map[string]string{
		"/h/a/slack-mcp-server.env": `{"ok":true,"was_running":true,"previous_version":"v1","version":"v2"}`,
	}, err: map[string]error{"/h/b/slack-mcp-server.env": errors.New("exit status 1")}}}
	var w bytes.Buffer
	failed := RestartListeners(&w, r, "/bin/smcp", []Home{{Path: "/h/a", HasEnv: true}, {Path: "/h/b", HasEnv: true}})
	assert.True(t, failed)
	assert.Equal(t, "\nListeners\n"+
		"  /h/a:\n    progress for /h/a/slack-mcp-server.env\n    restarted on the new binary (v1 → v2); watches carried over\n"+
		"  /h/b:\n    progress for /h/b/slack-mcp-server.env\n    restart FAILED: exit status 1 (the reason is printed above)\n", w.String())
}

// A session file counts as changed only when its content differs: JSON by
// value (reordered keys are the same), anything else byte for byte.
func TestSameContent(t *testing.T) {
	assert.True(t, sameContent("settings.json", []byte(`{"a":1,"b":[2]}`), []byte("{\n  \"b\": [2],\n  \"a\": 1\n}")))
	assert.False(t, sameContent("settings.json", []byte(`{"a":1}`), []byte(`{"a":2}`)))
	assert.True(t, sameContent("config.toml", []byte("x = 1\n"), []byte("x = 1\n")))
	assert.False(t, sameContent("config.toml", []byte("x = 1\n"), []byte("x = 2\n")))
	assert.False(t, sameContent("hooks.json", nil, []byte(`{}`)), "a file that appeared is a change")
	assert.True(t, sameContent("hooks.json", nil, nil))
}

// Setup's own writes that end where they started (re-registering the MCP
// server, then restoring a line) leave a home not stale.
func TestSnapshotSessionFiles(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("a = 1\n"), 0o600))
	before := snapshotSessionFiles(home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("a = 2\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("a = 1\n"), 0o600))
	after := snapshotSessionFiles(home)
	for name := range before {
		assert.True(t, sameContent(name, before[name], after[name]), name)
	}
}
