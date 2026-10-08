package agentchat

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
	"go.uber.org/zap"
)

const (
	idleExit      = 60 * time.Second
	sweepInterval = 60 * time.Second
	// cohortInterval is how often cohort deadlines are checked; well under
	// the 10-minute successor window.
	cohortInterval = 30 * time.Second
)

// ParseEventsAPIMessage extracts a message event from a Socket Mode
// events_api payload; ok is false for any other event.
func ParseEventsAPIMessage(payload []byte) (Message, bool) {
	var env struct {
		Event struct {
			Type     string `json:"type"`
			SubType  string `json:"subtype"`
			Channel  string `json:"channel"`
			User     string `json:"user"`
			BotID    string `json:"bot_id"`
			Text     string `json:"text"`
			TS       string `json:"ts"`
			ThreadTS string `json:"thread_ts"`
			Files    []struct {
				Name string `json:"name"`
			} `json:"files"`
		} `json:"event"`
	}
	if err := json.Unmarshal(payload, &env); err != nil || env.Event.Type != "message" {
		return Message{}, false
	}
	e := env.Event
	var files []string
	for _, f := range e.Files {
		files = append(files, f.Name)
	}
	return Message{Channel: e.Channel, TS: e.TS, ThreadTS: e.ThreadTS, User: e.User, BotID: e.BotID, Text: e.Text, SubType: e.SubType, Files: files}, true
}

// ParseEventsAPIMemberJoined extracts a member_joined_channel event from a
// Socket Mode events_api payload; ok is false for any other event.
func ParseEventsAPIMemberJoined(payload []byte) (channel, user string, ok bool) {
	var env struct {
		Event struct {
			Type    string `json:"type"`
			Channel string `json:"channel"`
			User    string `json:"user"`
		} `json:"event"`
	}
	if err := json.Unmarshal(payload, &env); err != nil || env.Event.Type != "member_joined_channel" {
		return "", "", false
	}
	return env.Event.Channel, env.Event.User, true
}

func requireEnv(home Home, key string) (string, error) {
	v := os.Getenv(key)
	if v == "" {
		return "", fmt.Errorf("%s is not set in %s", key, home.EnvFile)
	}
	return v, nil
}

// RunListener runs home's listener until ctx ends or no session has been
// subscribed for idleExit. The env file must already be loaded.
func RunListener(ctx context.Context, home Home, log *zap.Logger) error {
	appToken, err := requireEnv(home, "SLACK_MCP_XAPP_TOKEN")
	if err != nil {
		return err
	}
	botToken, err := requireEnv(home, "SLACK_MCP_XOXB_TOKEN")
	if err != nil {
		return err
	}
	userToken, err := requireEnv(home, "SLACK_MCP_XOXP_TOKEN")
	if err != nil {
		return err
	}

	// The lock makes this the home's only listener; it is held until exit, so
	// the socket removal below can never hit another listener's socket.
	release, err := AcquireListenerLock(filepath.Join(home.StateDir, "listener.lock"))
	if err != nil {
		return err
	}
	defer release()
	ln, err := ListenControl(home.ControlSocket)
	if err != nil {
		return err
	}
	defer os.Remove(home.ControlSocket)

	botAPI := slack.New(botToken, slack.OptionAppLevelToken(appToken))
	self, err := botAPI.AuthTestContext(ctx)
	if err != nil {
		ln.Close()
		return fmt.Errorf("bot auth.test: %w", err)
	}
	userAPI := slack.New(userToken)
	owner, err := userAPI.AuthTestContext(ctx)
	if err != nil {
		ln.Close()
		return fmt.Errorf("user auth.test: %w", err)
	}

	l, err := NewListener(botAPI, &HostDeliverer{CodexSocket: home.CodexSocket},
		Identity{UserID: self.UserID, BotID: self.BotID}, owner.UserID, home.StateFile, log)
	if err != nil {
		ln.Close()
		return err
	}
	l.Async = true // deliveries must never stall the Socket Mode event loop
	l.Users = userAPI

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	l.Stop = cancel
	go ServeControl(ctx, ln, l.Control)
	go func() {
		// Drop sessions that ended while the listener was down, then catch the
		// rest up on what they missed.
		l.Sweep(ctx)
		l.RecoverAll(ctx)
	}()

	sm := socketmode.New(botAPI)
	events := make(chan struct{}) // closed when the event loop has stopped
	go func() {
		defer close(events)
		for {
			select {
			case <-ctx.Done():
				return
			case evt := <-sm.Events:
				if evt.Request == nil {
					continue
				}
				if evt.Type == socketmode.EventTypeInteractive {
					sm.Ack(*evt.Request)
					l.HandleInteraction(evt.Request.Payload)
					continue
				}
				if evt.Type != socketmode.EventTypeEventsAPI {
					continue
				}
				sm.Ack(*evt.Request)
				if m, ok := ParseEventsAPIMessage(evt.Request.Payload); ok {
					l.HandleMessage(ctx, m)
				} else if channel, user, ok := ParseEventsAPIMemberJoined(evt.Request.Payload); ok {
					// Off the event loop: it may recover a backlog or call Slack.
					go l.HandleMemberJoined(ctx, channel, user)
				}
			}
		}
	}()

	var retrying atomic.Bool // a RetryBacklogs pass is running
	go func() {
		idleSince, lastSweep, lastCohort := time.Now(), time.Now(), time.Now()
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-tick.C:
				if now.Sub(lastSweep) >= sweepInterval {
					l.Sweep(ctx)
					// Off the ticker: a slow retry must not hold up the
					// approval sweep or cohort deadlines.
					if retrying.CompareAndSwap(false, true) {
						go func() {
							defer retrying.Store(false)
							l.RetryBacklogs(ctx)
						}()
					}
					lastSweep = now
				}
				l.SweepApprovals(ctx)
				if now.Sub(lastCohort) >= cohortInterval {
					l.CohortTick(ctx)
					lastCohort = now
				}
				if l.HasSubscriptions() {
					idleSince = now
				} else if now.Sub(idleSince) >= idleExit {
					log.Info("no subscriptions; exiting")
					cancel()
					return
				}
			}
		}
	}()

	log.Info("listener started", zap.String("home", home.Dir), zap.String("bot_user", self.UserID), zap.String("owner", owner.UserID))
	err = sm.RunContext(ctx)
	cancel() // stops the event loop too if Socket Mode ended on its own
	// Let the event loop finish the event in hand, then the notes and
	// redraws it started, so none is cut off by exit.
	select {
	case <-events:
	case <-time.After(5 * time.Second):
	}
	l.WaitNotes(5 * time.Second)
	if ctx.Err() != nil {
		return nil
	}
	return err
}
