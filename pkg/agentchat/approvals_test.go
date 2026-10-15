package agentchat

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func approvalsStatus(t *testing.T, c *cli, arg string) map[string]any {
	out := c.stdout.(*bytes.Buffer)
	out.Reset()
	require.NoError(t, c.approvals([]string{arg}))
	var got map[string]any
	require.NoError(t, json.Unmarshal(out.Bytes(), &got))
	return got
}

func TestApprovalsToggle(t *testing.T) {
	h := testHome(t)
	c := &cli{home: h, stdout: &bytes.Buffer{}}
	assert.Equal(t, true, approvalsStatus(t, c, "status")["slack_approvals"], "on by default")
	assert.Equal(t, false, approvalsStatus(t, c, "off")["slack_approvals"])
	assert.False(t, slackApprovals(h))
	assert.Equal(t, false, approvalsStatus(t, c, "off")["slack_approvals"], "off twice is fine")
	assert.Equal(t, false, approvalsStatus(t, c, "status")["slack_approvals"])
	assert.Equal(t, true, approvalsStatus(t, c, "on")["slack_approvals"])
	assert.Equal(t, true, approvalsStatus(t, c, "on")["slack_approvals"], "on twice is fine")
	assert.Contains(t, approvalsStatus(t, c, "status")["message"], "Slack, then the terminal")

	assert.Error(t, c.approvals(nil))
	assert.Error(t, c.approvals([]string{"maybe"}))
}

// With Slack approvals off, both hooks leave the prompt to the terminal
// without touching Slack or the listener.
func TestHooksSkipSlackWhenApprovalsOff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected Slack call %s", r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	api := slack.OptionAPIURL(srv.URL + "/")
	h := testHome(t)
	asked := 0
	h.ControlSocket = serveListener(t, func(context.Context, ControlRequest) ControlResponse {
		asked++
		return ControlResponse{OK: true}
	})
	out := &bytes.Buffer{}
	c := &cli{home: h, stdout: out, stderr: &bytes.Buffer{}, bot: slack.New("xoxb-x", api), user: slack.New("xoxp-x", api)}
	require.NoError(t, c.approvals([]string{"off"}))
	out.Reset()

	ev := hookEvent{SessionID: "s1", ToolName: "Bash", ToolInput: json.RawMessage(`{"command":"ls"}`), Reason: "blocked"}
	assert.Equal(t, 0, c.approvalHook(context.Background(), ev, time.Minute))
	assert.Equal(t, 0, c.deniedHook(context.Background(), ev, time.Minute))
	assert.Empty(t, out.String(), "no decision: the terminal handles it")
	assert.Zero(t, asked, "the listener is never asked")

	require.NoError(t, c.approvals([]string{"on"}))
	c.approvalHook(context.Background(), ev, time.Minute)
	assert.NotZero(t, asked, "on: the hook looks for the session's channels")
}
