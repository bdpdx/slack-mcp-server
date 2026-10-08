package setup

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
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
