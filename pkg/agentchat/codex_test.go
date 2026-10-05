package agentchat

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type rpcCall struct {
	Method string
	Params map[string]any
}

// fakeCodex serves the app-server protocol on a Unix socket. respond returns
// (result, errorMessage) per call; a non-empty errorMessage becomes a JSON-RPC error.
type fakeCodex struct {
	mu      sync.Mutex
	calls   []rpcCall
	respond func(method string, params map[string]any) (any, string)
}

func (f *fakeCodex) start(t *testing.T) string {
	path := shortSocketPath(t)
	ln, err := net.Listen("unix", path)
	require.NoError(t, err)
	up := websocket.Upgrader{}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		for {
			var req struct {
				ID     *int64         `json:"id"`
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			if err := ws.ReadJSON(&req); err != nil {
				return
			}
			f.mu.Lock()
			f.calls = append(f.calls, rpcCall{req.Method, req.Params})
			f.mu.Unlock()
			if req.ID == nil {
				continue
			}
			// Interleave a notification to prove the client skips it.
			_ = ws.WriteJSON(map[string]any{"method": "thread/status/changed", "params": map[string]any{}})
			result, errMsg := f.respond(req.Method, req.Params)
			if errMsg != "" {
				_ = ws.WriteJSON(map[string]any{"id": *req.ID, "error": map[string]any{"code": -32600, "message": errMsg}})
			} else {
				_ = ws.WriteJSON(map[string]any{"id": *req.ID, "result": result})
			}
		}
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return path
}

func (f *fakeCodex) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, c.Method)
	}
	return out
}

func (f *fakeCodex) last(method string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.calls) - 1; i >= 0; i-- {
		if f.calls[i].Method == method {
			return f.calls[i].Params
		}
	}
	return nil
}

func status(s string) any {
	return map[string]any{"thread": map[string]any{"id": "th", "status": map[string]any{"type": s}}}
}

func TestDeliverCodexIdleStartsTurn(t *testing.T) {
	f := &fakeCodex{respond: func(m string, _ map[string]any) (any, string) {
		switch m {
		case "thread/read":
			return status("idle"), ""
		case "turn/start":
			return map[string]any{"turn": map[string]any{"id": "t1"}}, ""
		}
		return map[string]any{}, ""
	}}
	sock := f.start(t)
	method, err := DeliverCodex(context.Background(), sock, "th", "cid", "hi")
	require.NoError(t, err)
	assert.Equal(t, "turn/start", method)
	assert.Equal(t, []string{"initialize", "initialized", "thread/read", "turn/start"}, f.methods())
	p := f.last("turn/start")
	assert.Equal(t, "th", p["threadId"])
	assert.Equal(t, "cid", p["clientUserMessageId"])
	raw, _ := json.Marshal(p["input"])
	assert.JSONEq(t, `[{"type":"text","text":"hi"}]`, string(raw))
}

func TestDeliverCodexActiveSteers(t *testing.T) {
	f := &fakeCodex{respond: func(m string, _ map[string]any) (any, string) {
		switch m {
		case "thread/read":
			return status("active"), ""
		case "thread/turns/list":
			return map[string]any{"data": []any{map[string]any{"id": "turn-9", "status": "inProgress"}}}, ""
		case "turn/steer":
			return map[string]any{"turnId": "turn-9"}, ""
		}
		return map[string]any{}, ""
	}}
	sock := f.start(t)
	method, err := DeliverCodex(context.Background(), sock, "th", "cid", "hi")
	require.NoError(t, err)
	assert.Equal(t, "turn/steer", method)
	assert.Equal(t, "turn-9", f.last("turn/steer")["expectedTurnId"])
	lp := f.last("thread/turns/list")
	assert.Equal(t, "desc", lp["sortDirection"])
	assert.EqualValues(t, 1, lp["limit"])
}

func TestDeliverCodexRetriesAfterSteerRejected(t *testing.T) {
	reads := 0
	f := &fakeCodex{respond: func(m string, _ map[string]any) (any, string) {
		switch m {
		case "thread/read":
			reads++
			if reads == 1 {
				return status("active"), ""
			}
			return status("idle"), ""
		case "thread/turns/list":
			return map[string]any{"data": []any{map[string]any{"id": "turn-9", "status": "inProgress"}}}, ""
		case "turn/steer":
			return nil, "no active turn"
		case "turn/start":
			return map[string]any{"turn": map[string]any{"id": "t2"}}, ""
		}
		return map[string]any{}, ""
	}}
	sock := f.start(t)
	method, err := DeliverCodex(context.Background(), sock, "th", "cid", "hi")
	require.NoError(t, err)
	assert.Equal(t, "turn/start", method)
	assert.Equal(t, []string{"initialize", "initialized", "thread/read", "thread/turns/list", "turn/steer", "thread/read", "turn/start"}, f.methods())
}

// SAC-3: an active thread briefly has no in-progress turn while a turn ends;
// retrying immediately can use up every attempt before it settles.
func TestDeliverCodexWaitsBetweenRetries(t *testing.T) {
	old := codexRetryDelay
	codexRetryDelay = 40 * time.Millisecond
	defer func() { codexRetryDelay = old }()
	reads := 0
	f := &fakeCodex{respond: func(m string, _ map[string]any) (any, string) {
		switch m {
		case "thread/read":
			reads++
			if reads == 1 {
				return status("active"), ""
			}
			return status("idle"), ""
		case "thread/turns/list":
			return map[string]any{"data": []any{}}, ""
		}
		return map[string]any{}, ""
	}}
	sock := f.start(t)
	start := time.Now()
	method, err := DeliverCodex(context.Background(), sock, "th", "cid", "hi")
	require.NoError(t, err)
	assert.Equal(t, "turn/start", method)
	assert.GreaterOrEqual(t, time.Since(start), 40*time.Millisecond)
}

// SAC-10: the daemon unloads a thread once no terminal is attached and it is
// idle, so a thread that is not loaded (or no longer exists) is a gone session.
func TestDeliverCodexNotLoadedIsGone(t *testing.T) {
	f := &fakeCodex{respond: func(m string, _ map[string]any) (any, string) {
		if m == "thread/read" {
			return status("notLoaded"), ""
		}
		return map[string]any{}, ""
	}}
	sock := f.start(t)
	_, err := DeliverCodex(context.Background(), sock, "th", "cid", "hi")
	assert.ErrorIs(t, err, ErrSessionGone)
	assert.NotContains(t, f.methods(), "turn/start")
}

func TestDeliverCodexUnknownThreadIsGone(t *testing.T) {
	f := &fakeCodex{respond: func(m string, _ map[string]any) (any, string) {
		if m == "thread/read" {
			return nil, "thread not found: th"
		}
		return map[string]any{}, ""
	}}
	sock := f.start(t)
	_, err := DeliverCodex(context.Background(), sock, "th", "cid", "hi")
	assert.ErrorIs(t, err, ErrSessionGone)
}

func TestDeliverCodexUnavailable(t *testing.T) {
	_, err := DeliverCodex(context.Background(), shortSocketPath(t), "th", "cid", "hi")
	assert.ErrorIs(t, err, ErrCodexUnavailable)
	assert.NotErrorIs(t, err, ErrSessionGone, "an unreachable daemon is not proof the session is gone")
}

func TestCodexThreadAlive(t *testing.T) {
	for state, want := range map[string]bool{"idle": true, "active": true, "notLoaded": false} {
		f := &fakeCodex{respond: func(string, map[string]any) (any, string) { return status(state), "" }}
		alive, err := CodexThreadAlive(context.Background(), f.start(t), "th")
		require.NoError(t, err, state)
		assert.Equal(t, want, alive, state)
	}
	_, err := CodexThreadAlive(context.Background(), shortSocketPath(t), "th")
	assert.ErrorIs(t, err, ErrCodexUnavailable, "unknown, not dead")
}

func TestClientMessageIDDeterministic(t *testing.T) {
	a := clientMessageID("s", "C", "1.0")
	assert.Equal(t, a, clientMessageID("s", "C", "1.0"))
	assert.NotEqual(t, a, clientMessageID("s", "C", "2.0"))
	assert.Contains(t, a, "slack-agent-chat-")
}
