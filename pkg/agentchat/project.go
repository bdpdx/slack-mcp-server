package agentchat

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/slack-go/slack"
)

// Slack's API cannot delete channels outside Enterprise Grid admin, so
// "deleting" a project archives its channels: they leave the sidebar, become
// read-only, and can be restored (or deleted for good in the Slack UI by a
// workspace owner).

// projectArchiver is the owner's Slack client (user token) as archiving needs it.
type projectArchiver interface {
	channelLister
	GetConversationInfoContext(ctx context.Context, input *slack.GetConversationInfoInput) (*slack.Channel, error)
	ArchiveConversationContext(ctx context.Context, channelID string) error
}

// projectChannel is one channel of a project.
type projectChannel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// errNotProjectOwner refuses archiving a project someone else created.
var errNotProjectOwner = errors.New("not the project's owner")

// projectChannels returns #project and every #project__… channel the owner is
// in, project channel last, after checking that the owner may archive them:
// Slack must record the owner (ownerID) as the project channel's creator, or,
// for projects created before project channels were created with the user
// token, this home's bot (botID).
func projectChannels(ctx context.Context, api projectArchiver, project, ownerID, botID string) ([]projectChannel, error) {
	var root *projectChannel
	var derived []projectChannel
	cursor := ""
	for {
		chans, next, err := api.GetConversationsForUserContext(ctx, &slack.GetConversationsForUserParameters{
			Types: []string{"private_channel", "public_channel"}, ExcludeArchived: true, Limit: 200, Cursor: cursor,
		})
		if err != nil {
			return nil, err
		}
		for _, ch := range chans {
			switch {
			case ch.Name == project:
				root = &projectChannel{ch.ID, ch.Name}
			case strings.HasPrefix(ch.Name, project+derivedSep):
				derived = append(derived, projectChannel{ch.ID, ch.Name})
			}
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if root == nil {
		return nil, fmt.Errorf("you are not in an unarchived channel named #%s", project)
	}
	info, err := api.GetConversationInfoContext(ctx, &slack.GetConversationInfoInput{ChannelID: root.ID})
	if err != nil {
		return nil, fmt.Errorf("reading #%s: %w", project, err)
	}
	if info.Creator == "" || (info.Creator != ownerID && info.Creator != botID) {
		return nil, fmt.Errorf("%w: #%s was created by <@%s>, not by you or this agent", errNotProjectOwner, project, info.Creator)
	}
	return append(derived, *root), nil
}

// archiveChannels archives each channel, carrying on past failures, and
// returns those archived and an error describing any that failed.
func archiveChannels(ctx context.Context, api projectArchiver, chans []projectChannel) ([]projectChannel, error) {
	var done []projectChannel
	var failed []string
	for _, ch := range chans {
		err := api.ArchiveConversationContext(ctx, ch.ID)
		if err != nil && !strings.Contains(err.Error(), "already_archived") {
			failed = append(failed, fmt.Sprintf("#%s: %v", ch.Name, err))
			continue
		}
		done = append(done, ch)
	}
	if len(failed) > 0 {
		return done, fmt.Errorf("could not archive %s", strings.Join(failed, "; "))
	}
	return done, nil
}

// project runs `chat project archive NAME [--dry-run]`.
func (c *cli) project(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "archive" {
		return errors.New("usage: project archive NAME [--dry-run]")
	}
	fs := flag.NewFlagSet("project", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	dryRun := fs.Bool("dry-run", false, "list the channels without archiving them")
	pos, err := parseArgs(fs, args[1:])
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: project archive NAME [--dry-run]")
	}
	name, err := NormalizeChannelName(pos[0])
	if err != nil {
		return err
	}
	if _, derived := ProjectOf(name); derived {
		return fmt.Errorf("#%s is not a project channel; name the project", name)
	}
	me, err := c.identity(ctx)
	if err != nil {
		return err
	}
	chans, err := projectChannels(ctx, c.user, name, me.ownerID, me.agentID)
	if err != nil {
		return err
	}
	if *dryRun {
		c.printJSON(map[string]any{"project": name, "would_archive": chans})
		return nil
	}
	done, archiveErr := archiveChannels(ctx, c.user, chans)
	c.unwatch(ctx, done)
	c.printJSON(map[string]any{"project": name, "archived": done})
	return archiveErr
}

// unwatch stops this home's sessions watching archived channels. Other
// homes' listeners drop them when they see the archive event.
func (c *cli) unwatch(ctx context.Context, chans []projectChannel) {
	resp, err := SendControl(ctx, c.home.ControlSocket, ControlRequest{Op: "status"})
	if err != nil {
		return
	}
	for _, s := range resp.Sessions {
		for _, ch := range chans {
			if (&Subscription{Channels: s.Channels}).Watches(ch.ID) {
				_, _ = SendControl(ctx, c.home.ControlSocket, ControlRequest{Op: "unsubscribe", SessionID: s.SessionID, Channel: ch.ID})
			}
		}
	}
}
