package agentchat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"syscall"
	"time"
)

// ErrSessionGone means the target session no longer exists.
var ErrSessionGone = errors.New("session is gone")

// DeliverClaude posts text into a Claude Code session through its inbox
// socket: an auth line with the session token, then one user message line.
func DeliverClaude(ctx context.Context, socket, token, text string) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
			return fmt.Errorf("%w: %v", ErrSessionGone, err)
		}
		return err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	auth, err := json.Marshal(map[string]string{"type": "auth", "token": token})
	if err != nil {
		return err
	}
	msg, err := json.Marshal(map[string]any{
		"type":    "user",
		"message": map[string]string{"role": "user", "content": text},
	})
	if err != nil {
		return err
	}
	payload := append(append(append(auth, '\n'), msg...), '\n')
	_, err = conn.Write(payload)
	return err
}
