package agentchat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSlackIdentity answers auth.test and users.info for a user token and a
// bot token, each in its own workspace.
func fakeSlackIdentity(t *testing.T, userTeam, botTeam string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		bot := strings.Contains(r.Header.Get("Authorization")+r.FormValue("token"), "xoxb-")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/auth.test") && bot:
			_, _ = w.Write([]byte(`{"ok":true,"user":"claudeb","user_id":"UBOT","team_id":"` + botTeam + `"}`))
		case strings.HasSuffix(r.URL.Path, "/auth.test"):
			_, _ = w.Write([]byte(`{"ok":true,"user":"owner","user_id":"UOWNER","team_id":"` + userTeam + `"}`))
		case strings.HasSuffix(r.URL.Path, "/users.info"):
			_, _ = w.Write([]byte(`{"ok":true,"user":{"id":"UBOT","name":"claudeb","real_name":"claude-b","profile":{"real_name":"claude-b"}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func whoamiCLI(t *testing.T, srv *httptest.Server, socket string) (*cli, *bytes.Buffer) {
	out := &bytes.Buffer{}
	api := slack.OptionAPIURL(srv.URL + "/")
	return &cli{home: Home{Dir: filepath.Join(t.TempDir(), "home"), ControlSocket: socket}, stdout: out, stderr: &bytes.Buffer{},
		bot: slack.New("xoxb-secret-bot", api), user: slack.New("xoxp-secret-user", api)}, out
}

// serveListener answers the control socket with handle until the test ends.
func serveListener(t *testing.T, handle ControlHandler) string {
	path := shortSocketPath(t)
	ln, err := ListenControl(path)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go ServeControl(ctx, ln, handle)
	return path
}

func TestWhoamiReportsTheHomesAgentWithoutTokens(t *testing.T) {
	srv := fakeSlackIdentity(t, "T1", "T1")
	socket := serveListener(t, func(_ context.Context, req ControlRequest) ControlResponse {
		return ControlResponse{OK: true, Version: "v9.9.9"}
	})
	c, out := whoamiCLI(t, srv, socket)
	require.NoError(t, c.whoami(context.Background()))
	assert.NotContains(t, out.String(), "xox")
	var got map[string]any
	require.NoError(t, json.Unmarshal(out.Bytes(), &got))
	assert.Equal(t, "claude-b", got["agent"])
	assert.Equal(t, "UBOT", got["agent_id"])
	assert.Equal(t, "owner", got["owner"])
	assert.Equal(t, "UOWNER", got["owner_id"])
	assert.Equal(t, "T1", got["workspace_id"])
	assert.Equal(t, c.home.Dir, got["home"])
	assert.Equal(t, true, got["listener_running"])
	assert.Equal(t, "v9.9.9", got["listener_version"])
	assert.Contains(t, got, "binary_version")
}

func TestWhoamiListenerVersionUnknownWhenDownOrOld(t *testing.T) {
	srv := fakeSlackIdentity(t, "T1", "T1")
	// No listener: still succeeds, the listener is simply not running.
	c, out := whoamiCLI(t, srv, filepath.Join(t.TempDir(), "absent.sock"))
	require.NoError(t, c.whoami(context.Background()))
	var got map[string]any
	require.NoError(t, json.Unmarshal(out.Bytes(), &got))
	assert.Equal(t, false, got["listener_running"])
	assert.Equal(t, "unknown", got["listener_version"])

	// A listener from before the version field answers without one.
	old := serveListener(t, func(_ context.Context, req ControlRequest) ControlResponse {
		return ControlResponse{OK: true}
	})
	c, out = whoamiCLI(t, srv, old)
	require.NoError(t, c.whoami(context.Background()))
	got = nil
	require.NoError(t, json.Unmarshal(out.Bytes(), &got))
	assert.Equal(t, true, got["listener_running"])
	assert.Equal(t, "unknown", got["listener_version"])
}

func TestWhoamiRefusesTokensFromDifferentWorkspaces(t *testing.T) {
	srv := fakeSlackIdentity(t, "T1", "T2")
	c, out := whoamiCLI(t, srv, filepath.Join(t.TempDir(), "absent.sock"))
	err := c.whoami(context.Background())
	assert.ErrorContains(t, err, "different workspaces (T1, T2)")
	assert.Empty(t, out.String())
}

func TestSendControlMarksRefusals(t *testing.T) {
	socket := serveListener(t, func(_ context.Context, req ControlRequest) ControlResponse {
		return ControlResponse{Error: "claude-b is not in p's succession order"}
	})
	_, err := SendControl(context.Background(), socket, ControlRequest{Op: "cohort-register"})
	var refused *RefusedError
	require.True(t, errors.As(err, &refused))
	assert.Equal(t, "claude-b is not in p's succession order", err.Error())
	assert.False(t, listenerDown(err))

	_, err = SendControl(context.Background(), filepath.Join(t.TempDir(), "absent.sock"), ControlRequest{Op: "status"})
	assert.True(t, listenerDown(err))
	assert.False(t, listenerDown(nil))
}

// A running listener's refusal reaches the user as its reason, not as
// "no listener is running".
func TestCohortCommandsReportListenerRefusals(t *testing.T) {
	t.Setenv("CODEX_THREAD_ID", "thread-1")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	socket := serveListener(t, func(_ context.Context, req ControlRequest) ControlResponse {
		return ControlResponse{Error: "this session is not registered in p"}
	})
	c := &cli{home: Home{ControlSocket: socket}, stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}
	err := c.cohort(context.Background(), []string{"duty", "--project", "p", "off"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "this session is not registered in p")
	assert.NotContains(t, err.Error(), "no slack-agent-chat listener")

	c.home.ControlSocket = filepath.Join(t.TempDir(), "absent.sock")
	err = c.cohort(context.Background(), []string{"duty", "--project", "p", "off"})
	assert.ErrorContains(t, err, "no slack-agent-chat listener is running")
}

// A definite claim-check refusal is "not eligible", not "unavailable"; only a
// missing listener or a refusal for missing evidence is unavailable.
func TestClaimEligibleSeparatesRefusalFromUnavailable(t *testing.T) {
	t.Setenv("CODEX_THREAD_ID", "thread-1")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	definite := serveListener(t, func(_ context.Context, req ControlRequest) ControlResponse {
		return ControlResponse{Error: "codex-b is off duty"}
	})
	c := &cli{home: Home{ControlSocket: definite}}
	err := c.claimEligible(context.Background(), "p", "codex-b", 1, "claude-b", false)
	assert.ErrorContains(t, err, "not eligible to claim: codex-b is off duty")
	assert.False(t, errors.Is(err, ErrAdmitUnavailable))

	unreadable := serveListener(t, func(_ context.Context, req ControlRequest) ControlResponse {
		return ControlResponse{Error: "slack unreadable", Unavailable: true}
	})
	c.home.ControlSocket = unreadable
	err = c.claimEligible(context.Background(), "p", "codex-b", 1, "claude-b", false)
	assert.True(t, errors.Is(err, ErrAdmitUnavailable))

	c.home.ControlSocket = filepath.Join(t.TempDir(), "absent.sock")
	err = c.claimEligible(context.Background(), "p", "codex-b", 1, "claude-b", false)
	assert.True(t, errors.Is(err, ErrAdmitUnavailable))

	// A running listener older than this binary does not know the op: that
	// is missing evidence (retry), not a definite refusal.
	old := serveListener(t, func(_ context.Context, req ControlRequest) ControlResponse {
		return ControlResponse{Error: "unknown op " + req.Op}
	})
	c.home.ControlSocket = old
	err = c.claimEligible(context.Background(), "p", "codex-b", 1, "claude-b", false)
	assert.True(t, errors.Is(err, ErrAdmitUnavailable))
	assert.ErrorContains(t, err, "unknown op cohort-claim-check")
}

func TestWhoamiCountsARefusingListenerAsRunning(t *testing.T) {
	srv := fakeSlackIdentity(t, "T1", "T1")
	socket := serveListener(t, func(_ context.Context, req ControlRequest) ControlResponse {
		return ControlResponse{Error: "unknown op " + req.Op}
	})
	c, out := whoamiCLI(t, srv, socket)
	require.NoError(t, c.whoami(context.Background()))
	var got map[string]any
	require.NoError(t, json.Unmarshal(out.Bytes(), &got))
	assert.Equal(t, true, got["listener_running"])
	assert.Equal(t, "unknown", got["listener_version"])
}

func TestWhoamiRefusesAMissingWorkspace(t *testing.T) {
	srv := fakeSlackIdentity(t, "", "")
	c, out := whoamiCLI(t, srv, filepath.Join(t.TempDir(), "absent.sock"))
	assert.ErrorContains(t, c.whoami(context.Background()), "bot auth.test returned no workspace")
	assert.Empty(t, out.String())
}

func TestRegisterAgentDefaultsToTheHomesBot(t *testing.T) {
	got, err := registerAgent("", "claude-b")
	require.NoError(t, err)
	assert.Equal(t, "claude-b", got)

	got, err = registerAgent("claude-b", "claude-b")
	require.NoError(t, err)
	assert.Equal(t, "claude-b", got)

	_, err = registerAgent("claude", "claude-b")
	assert.ErrorContains(t, err, "--agent claude is not this home's agent (claude-b)")

	_, err = registerAgent("", "")
	assert.ErrorContains(t, err, "cannot resolve this home's agent")
}
