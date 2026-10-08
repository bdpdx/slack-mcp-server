package agentchat

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// ErrListenerRunning means another listener already owns the control socket.
var ErrListenerRunning = errors.New("a listener is already running for this home")

// RefusedError is a reply the listener sent with ok=false: the listener is
// running and answered, but declined the request. Its text is the
// listener's reason, unchanged.
type RefusedError struct{ Reason string }

func (e *RefusedError) Error() string { return e.Reason }

// listenerDown reports whether err from SendControl means no listener
// answered (no socket, a dropped connection, a timeout), as opposed to a
// listener that answered with a refusal.
func listenerDown(err error) bool {
	var refused *RefusedError
	return err != nil && !errors.As(err, &refused)
}

// ControlRequest is one command sent to the listener.
type ControlRequest struct {
	Op           string        `json:"op"`
	Subscription *Subscription `json:"subscription,omitempty"`
	Backlog      int           `json:"backlog,omitempty"`
	SessionID    string        `json:"session_id,omitempty"`
	Channel      string        `json:"channel,omitempty"`
	TS           string        `json:"ts,omitempty"`
	Text         string        `json:"text,omitempty"`
	Approval     string        `json:"approval,omitempty"`
	Cohort       *CohortReg    `json:"cohort,omitempty"`        // cohort-* ops
	Expect       *GMState      `json:"expect,omitempty"`        // cohort-claim-check: the GM and term the claim expects
	UserDirected bool          `json:"user_directed,omitempty"` // cohort-claim-check: the user ordered it; skip only the deadline
}

// ControlResponse is the listener's reply.
type ControlResponse struct {
	OK       bool            `json:"ok"`
	Error    string          `json:"error,omitempty"`
	Sessions []SessionStatus `json:"sessions,omitempty"`
	Decision string          `json:"decision,omitempty"` // for op "approval"
	Text     string          `json:"text,omitempty"`     // for op "approval": the reason
	Cohort   []CohortReg     `json:"cohort,omitempty"`   // for cohort-* ops
	Watches  []GMWatch       `json:"watches,omitempty"`  // for cohort-status: pending deadlines
	// Unavailable marks a cohort-claim-check refusal for missing evidence
	// (an unreadable lookup), as opposed to a definite no.
	Unavailable bool `json:"unavailable,omitempty"`
	// Unknown marks an "approval" poll for a request this listener has no
	// record of (it restarted): the hook registers its message again.
	Unknown bool `json:"unknown,omitempty"`
	// Version is the running listener's build (op "status"). A listener
	// older than this field leaves it empty: its version is unknown.
	Version string `json:"version,omitempty"`
}

// SessionStatus describes one subscribed session.
type SessionStatus struct {
	SessionID string   `json:"session_id"`
	Kind      string   `json:"kind"`
	Channels  []string `json:"channels"`
	// Names maps channel IDs to names the listener knows (control op status).
	Names map[string]string `json:"names,omitempty"`
}

// ControlHandler answers one request.
type ControlHandler func(ctx context.Context, req ControlRequest) ControlResponse

// AcquireListenerLock takes the home's exclusive listener lock, held for the
// listener's whole life; it fails with ErrListenerRunning while another
// process holds it. Call release on exit.
func AcquireListenerLock(path string) (release func(), err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrListenerRunning
		}
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// ListenControl binds the control socket, replacing a stale one. Callers hold
// the listener lock, so a socket nobody answers on is safe to remove.
func ListenControl(socket string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		return nil, err
	}
	if conn, err := net.DialTimeout("unix", socket, time.Second); err == nil {
		conn.Close()
		return nil, ErrListenerRunning
	}
	if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// ServeControl answers one JSON line per connection until ctx ends.
func ServeControl(ctx context.Context, ln net.Listener, h ControlHandler) {
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func(conn net.Conn) {
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(2 * time.Minute))
			r := bufio.NewReader(conn)
			line, err := r.ReadBytes('\n')
			if err != nil {
				return
			}
			var req ControlRequest
			resp := ControlResponse{Error: "invalid request"}
			if json.Unmarshal(line, &req) == nil {
				resp = h(ctx, req)
			}
			data, _ := json.Marshal(resp)
			_, _ = conn.Write(append(data, '\n'))
		}(conn)
	}
}

// SendControl sends req to the listener and returns its reply; a reply with
// ok=false is returned as an error.
func SendControl(ctx context.Context, socket string, req ControlRequest) (ControlResponse, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return ControlResponse{}, err
	}
	defer conn.Close()
	deadline := time.Now().Add(2 * time.Minute)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetDeadline(deadline)
	data, err := json.Marshal(req)
	if err != nil {
		return ControlResponse{}, err
	}
	if _, err := conn.Write(append(data, '\n')); err != nil {
		return ControlResponse{}, err
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return ControlResponse{}, err
	}
	var resp ControlResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return ControlResponse{}, err
	}
	if !resp.OK {
		return resp, &RefusedError{Reason: resp.Error}
	}
	return resp, nil
}
