package agentchat

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
	"go.uber.org/zap"
)

const (
	idleExit      = 60 * time.Second
	sweepInterval = 60 * time.Second
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
	owner, err := slack.New(userToken).AuthTestContext(ctx)
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

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go ServeControl(ctx, ln, l.Control)

	sm := socketmode.New(botAPI)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case evt := <-sm.Events:
				if evt.Type != socketmode.EventTypeEventsAPI || evt.Request == nil {
					continue
				}
				sm.Ack(*evt.Request)
				if m, ok := ParseEventsAPIMessage(evt.Request.Payload); ok {
					l.HandleMessage(ctx, m)
				}
			}
		}
	}()

	go func() {
		idleSince, lastSweep := time.Now(), time.Now()
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-tick.C:
				if now.Sub(lastSweep) >= sweepInterval {
					l.Sweep(ctx)
					lastSweep = now
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
	if ctx.Err() != nil {
		return nil
	}
	return err
}
