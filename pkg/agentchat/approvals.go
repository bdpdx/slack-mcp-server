package agentchat

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// A home's approval and denial prompts go to Slack unless the user turned
// that off (chat approvals off). The setting is a marker file in the home's
// state directory, so it holds for every session in the home until turned
// back on.

const approvalsOffFile = "approvals-off"

const approvalsUsage = "usage: approvals on|off|status"

func approvalsOffPath(h Home) string { return filepath.Join(h.StateDir, approvalsOffFile) }

// slackApprovals reports whether approval-hook and denied-hook ask in Slack.
// Anything but a marker that is certainly there leaves them on.
func slackApprovals(h Home) bool {
	_, err := os.Stat(approvalsOffPath(h))
	return err != nil
}

// approvals runs `chat approvals on|off|status`.
func (c *cli) approvals(args []string) error {
	if len(args) != 1 {
		return errors.New(approvalsUsage)
	}
	path := approvalsOffPath(c.home)
	switch args[0] {
	case "on":
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	case "off":
		if err := os.MkdirAll(c.home.StateDir, 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			return err
		}
	case "status":
	default:
		return errors.New(approvalsUsage)
	}
	on := slackApprovals(c.home)
	where := "the terminal only"
	if on {
		where = "Slack, then the terminal if unanswered"
	}
	c.printJSON(map[string]any{"home": c.home.Dir, "slack_approvals": on,
		"message": fmt.Sprintf("Approval and denial prompts for %s go to %s.", c.home.Dir, where)})
	return nil
}
