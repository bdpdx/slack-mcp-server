package agentchat

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	KindCodex  = "codex"
	KindClaude = "claude"

	deliveredRetention = 7 * 24 * time.Hour
)

// Subscription is one session watching one or more channels.
type Subscription struct {
	SessionID string   `json:"session_id"`
	Kind      string   `json:"kind"`
	ThreadID  string   `json:"thread_id,omitempty"`
	Socket    string   `json:"socket,omitempty"`
	Token     string   `json:"token,omitempty"`
	Channels  []string `json:"channels"`
}

// Watches reports whether the subscription includes channel.
func (s *Subscription) Watches(channel string) bool { return slices.Contains(s.Channels, channel) }

// State is the listener's persisted state.
type State struct {
	JoinTS        map[string]string        `json:"join_ts"`
	Subscriptions map[string]*Subscription `json:"subscriptions"`
	Delivered     map[string]int64         `json:"delivered"`
}

// LoadState reads path; a missing file yields empty state.
func LoadState(path string) (*State, error) {
	s := &State{}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		if err := json.Unmarshal(data, s); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", path, err)
		}
	}
	if s.JoinTS == nil {
		s.JoinTS = map[string]string{}
	}
	if s.Subscriptions == nil {
		s.Subscriptions = map[string]*Subscription{}
	}
	if s.Delivered == nil {
		s.Delivered = map[string]int64{}
	}
	return s, nil
}

// Save writes the state atomically with owner-only permissions.
func (s *State) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func deliveredKey(session, channel, ts string) string { return session + "|" + channel + "|" + ts }

// WasDelivered reports whether ts in channel was already pushed to session.
func (s *State) WasDelivered(session, channel, ts string) bool {
	_, ok := s.Delivered[deliveredKey(session, channel, ts)]
	return ok
}

// MarkDelivered records that ts in channel was pushed to session.
func (s *State) MarkDelivered(session, channel, ts string, now time.Time) {
	s.Delivered[deliveredKey(session, channel, ts)] = now.Unix()
}

// Prune forgets deliveries older than the retention period.
func (s *State) Prune(now time.Time) {
	cutoff := now.Add(-deliveredRetention).Unix()
	for k, t := range s.Delivered {
		if t < cutoff {
			delete(s.Delivered, k)
		}
	}
}

// Watchers returns the subscriptions that include channel, ordered by session ID.
func (s *State) Watchers(channel string) []*Subscription {
	var out []*Subscription
	for _, sub := range s.Subscriptions {
		if sub.Watches(channel) {
			out = append(out, sub)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SessionID < out[j].SessionID })
	return out
}

// NowTS formats t as a Slack timestamp.
func NowTS(t time.Time) string { return fmt.Sprintf("%d.%06d", t.Unix(), t.Nanosecond()/1000) }

func splitTS(ts string) (int64, int64) {
	sec, frac, _ := strings.Cut(ts, ".")
	s, _ := strconv.ParseInt(sec, 10, 64)
	f, _ := strconv.ParseInt((frac + "000000")[:6], 10, 64)
	return s, f
}

// TSLess reports whether Slack timestamp a is earlier than b.
func TSLess(a, b string) bool {
	as, af := splitTS(a)
	bs, bf := splitTS(b)
	return as < bs || (as == bs && af < bf)
}
