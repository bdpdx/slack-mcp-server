package setup

import (
	"context"
	"fmt"
	"strings"

	"github.com/slack-go/slack"
)

// NormalizeToken removes whitespace and wrapping quotes from a pasted token.
func NormalizeToken(s string) string {
	return strings.TrimSpace(strings.Trim(strings.TrimSpace(s), `"'`))
}

var tokenPrefixes = map[string]string{"bot": "xoxb-", "user": "xoxp-", "app": "xapp-"}

// CheckTokenPrefix returns why token is not a kind token, or "".
func CheckTokenPrefix(kind, token string) string {
	want, ok := tokenPrefixes[kind]
	if !ok {
		return fmt.Sprintf("unknown token kind %q", kind)
	}
	if strings.HasPrefix(token, want) || strings.HasPrefix(token, "xoxe."+want) {
		return ""
	}
	return fmt.Sprintf("This doesn't look like a %s token; it should start with %s.", kind, want)
}

// Identity is what auth.test reports for a token.
type Identity struct {
	TeamID string
	UserID string
	User   string // username
	IsBot  bool
}

// Validator checks tokens against Slack.
type Validator interface {
	AuthTest(ctx context.Context, token string) (Identity, error)
	CheckAppToken(ctx context.Context, token string) error
	BotNameTaken(ctx context.Context, botToken, name, selfID string) (bool, error)
}

// SlackValidator talks to the Slack API.
type SlackValidator struct{}

func (SlackValidator) AuthTest(ctx context.Context, token string) (Identity, error) {
	r, err := slack.New(token).AuthTestContext(ctx)
	if err != nil {
		return Identity{}, err
	}
	return Identity{TeamID: r.TeamID, UserID: r.UserID, User: r.User, IsBot: r.BotID != ""}, nil
}

// CheckAppToken opens (and drops) a Socket Mode connection, which proves the
// token has connections:write and the app has Socket Mode on.
func (SlackValidator) CheckAppToken(ctx context.Context, token string) error {
	_, _, err := slack.New("", slack.OptionAppLevelToken(token)).StartSocketModeContext(ctx)
	return err
}

func (SlackValidator) BotNameTaken(ctx context.Context, botToken, name, selfID string) (bool, error) {
	users, err := slack.New(botToken).GetUsersContext(ctx)
	if err != nil {
		return false, err
	}
	for _, u := range users {
		if u.IsBot && !u.Deleted && u.ID != selfID &&
			(strings.EqualFold(u.Name, name) || strings.EqualFold(u.RealName, name) || strings.EqualFold(u.Profile.DisplayName, name)) {
			return true, nil
		}
	}
	return false, nil
}
