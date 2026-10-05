package agentchat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"time"

	"github.com/gorilla/websocket"
)

var (
	// ErrThreadNotLoaded means the thread exists but is not loaded on the daemon.
	ErrThreadNotLoaded = errors.New("codex thread is not loaded on the app-server daemon")
	// ErrCodexUnavailable means the app-server daemon could not be reached.
	ErrCodexUnavailable = errors.New("codex app-server is unavailable")
)

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

// DeliverCodex pushes text into a Codex thread: turn/steer while a turn is
// running, turn/start when idle. A JSON-RPC rejection (the turn ended between
// the status check and the request) re-checks the status and retries.
func DeliverCodex(ctx context.Context, socket, threadID, clientMsgID, text string) error {
	c, err := dialCodex(ctx, socket)
	if err != nil {
		return err
	}
	defer c.ws.Close()
	input := []map[string]string{{"type": "text", "text": text}}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		var read struct {
			Thread struct {
				Status struct {
					Type string `json:"type"`
				} `json:"status"`
			} `json:"thread"`
		}
		if err := c.call(ctx, "thread/read", map[string]any{"threadId": threadID, "includeTurns": false}, &read); err != nil {
			return err
		}
		switch read.Thread.Status.Type {
		case "notLoaded":
			return ErrThreadNotLoaded
		case "idle":
			lastErr = c.call(ctx, "turn/start", map[string]any{
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
				return err
			}
			if len(turns.Data) == 0 || turns.Data[0].Status != "inProgress" {
				lastErr = errors.New("active thread has no in-progress turn")
				continue
			}
			lastErr = c.call(ctx, "turn/steer", map[string]any{
				"threadId": threadID, "input": input,
				"expectedTurnId": turns.Data[0].ID, "clientUserMessageId": clientMsgID,
			}, nil)
		default:
			return fmt.Errorf("codex thread status %q", read.Thread.Status.Type)
		}
		if lastErr == nil {
			return nil
		}
		var rerr *rpcError
		if !errors.As(lastErr, &rerr) {
			return lastErr
		}
	}
	return lastErr
}

// QueueCodex hands text to `codex queue`, which delivers it once the thread is idle.
func QueueCodex(ctx context.Context, codexHome, threadID, text string) error {
	cmd := exec.CommandContext(ctx, "codex", "queue", "--thread", threadID, "--message", text)
	cmd.Env = append(os.Environ(), "CODEX_HOME="+codexHome)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("codex queue: %v: %s", err, bytes.TrimSpace(out))
	}
	return nil
}
