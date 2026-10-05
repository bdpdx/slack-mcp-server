package agentchat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// ErrCodexUnavailable means the app-server daemon could not be reached. It
// says nothing about whether the session is still there.
var ErrCodexUnavailable = errors.New("codex app-server is unavailable")

// codexRetryDelay is the pause before re-checking a thread after a delivery
// attempt was refused or found no in-progress turn.
var codexRetryDelay = 300 * time.Millisecond

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("app-server error %d: %s", e.Code, e.Message) }

type codexConn struct {
	ws     *websocket.Conn
	nextID int64
}

func dialCodex(ctx context.Context, socket string) (*codexConn, error) {
	d := websocket.Dialer{
		NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var nd net.Dialer
			return nd.DialContext(ctx, "unix", socket)
		},
		HandshakeTimeout: 10 * time.Second,
	}
	ws, _, err := d.DialContext(ctx, "ws://localhost/", nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCodexUnavailable, err)
	}
	c := &codexConn{ws: ws}
	init := map[string]any{"clientInfo": map[string]string{"name": "slack_agent_chat", "title": "Slack Agent Chat", "version": "1.0.0"}}
	if err := c.call(ctx, "initialize", init, nil); err != nil {
		ws.Close()
		return nil, fmt.Errorf("%w: initialize: %v", ErrCodexUnavailable, err)
	}
	if err := ws.WriteJSON(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		ws.Close()
		return nil, fmt.Errorf("%w: initialized: %v", ErrCodexUnavailable, err)
	}
	return c, nil
}

// call sends one request and waits for its response, skipping notifications
// and server-initiated requests.
func (c *codexConn) call(ctx context.Context, method string, params, result any) error {
	c.nextID++
	id := c.nextID
	if dl, ok := ctx.Deadline(); ok {
		_ = c.ws.SetReadDeadline(dl)
		_ = c.ws.SetWriteDeadline(dl)
	}
	if err := c.ws.WriteJSON(map[string]any{"method": method, "id": id, "params": params}); err != nil {
		return err
	}
	for {
		var msg struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  *rpcError       `json:"error"`
		}
		if err := c.ws.ReadJSON(&msg); err != nil {
			return err
		}
		if msg.Method != "" || msg.ID == nil || *msg.ID != id {
			continue
		}
		if msg.Error != nil {
			return msg.Error
		}
		if result != nil {
			return json.Unmarshal(msg.Result, result)
		}
		return nil
	}
}

// threadStatus reads a thread's status type. A thread the daemon no longer
// knows is reported as ErrSessionGone.
func (c *codexConn) threadStatus(ctx context.Context, threadID string) (string, error) {
	var read struct {
		Thread struct {
			Status struct {
				Type string `json:"type"`
			} `json:"status"`
		} `json:"thread"`
	}
	err := c.call(ctx, "thread/read", map[string]any{"threadId": threadID, "includeTurns": false}, &read)
	var rerr *rpcError
	if errors.As(err, &rerr) && strings.Contains(strings.ToLower(rerr.Message), "not found") {
		return "", fmt.Errorf("%w: %v", ErrSessionGone, err)
	}
	return read.Thread.Status.Type, err
}

// DeliverCodex pushes text into a Codex thread and returns the method used:
// turn/steer while a turn is running, turn/start when idle. A JSON-RPC
// rejection (the turn ended between the status check and the request)
// re-checks the status and retries. The daemon unloads a thread once no
// terminal is attached and it is idle, so a thread that is not loaded is
// reported as ErrSessionGone rather than started headless.
func DeliverCodex(ctx context.Context, socket, threadID, clientMsgID, text string) (string, error) {
	c, err := dialCodex(ctx, socket)
	if err != nil {
		return "", err
	}
	defer c.ws.Close()
	input := []map[string]string{{"type": "text", "text": text}}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(codexRetryDelay):
			}
		}
		status, err := c.threadStatus(ctx, threadID)
		if err != nil {
			return "", err
		}
		var method string
		switch status {
		case "notLoaded":
			return "", fmt.Errorf("%w: codex thread %s is not loaded", ErrSessionGone, threadID)
		case "idle":
			method = "turn/start"
			lastErr = c.call(ctx, method, map[string]any{
				"threadId": threadID, "input": input, "clientUserMessageId": clientMsgID,
			}, nil)
		case "active":
			var turns struct {
				Data []struct {
					ID     string `json:"id"`
					Status string `json:"status"`
				} `json:"data"`
			}
			if err := c.call(ctx, "thread/turns/list", map[string]any{
				"threadId": threadID, "sortDirection": "desc", "itemsView": "notLoaded", "limit": 1,
			}, &turns); err != nil {
				return "", err
			}
			if len(turns.Data) == 0 || turns.Data[0].Status != "inProgress" {
				lastErr = errors.New("active thread has no in-progress turn")
				continue
			}
			method = "turn/steer"
			lastErr = c.call(ctx, method, map[string]any{
				"threadId": threadID, "input": input,
				"expectedTurnId": turns.Data[0].ID, "clientUserMessageId": clientMsgID,
			}, nil)
		default:
			return "", fmt.Errorf("codex thread status %q", status)
		}
		if lastErr == nil {
			return method, nil
		}
		var rerr *rpcError
		if !errors.As(lastErr, &rerr) {
			return "", lastErr
		}
	}
	return "", lastErr
}

// CodexThreadAlive reports whether a Codex thread is still loaded on the
// daemon. An unreachable daemon is an error, not a dead session.
func CodexThreadAlive(ctx context.Context, socket, threadID string) (bool, error) {
	c, err := dialCodex(ctx, socket)
	if err != nil {
		return false, err
	}
	defer c.ws.Close()
	status, err := c.threadStatus(ctx, threadID)
	if errors.Is(err, ErrSessionGone) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return status != "notLoaded", nil
}
