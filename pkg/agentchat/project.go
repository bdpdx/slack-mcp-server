package agentchat

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
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

const projectUsage = "usage: project create|join|archive NAME"

// project runs `chat project create|join|archive`.
func (c *cli) project(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New(projectUsage)
	}
	switch args[0] {
	case "create":
		return c.projectCreate(ctx, args[1:])
	case "join":
		return c.projectJoin(ctx, args[1:])
	case "archive":
		return c.projectArchive(ctx, args[1:])
	}
	return errors.New(projectUsage)
}

// projectCreate runs `chat project create NAME [--invite a,b] [--invite-user u,v]`:
// the project channel, #NAME__users for the people, then project join.
func (c *cli) projectCreate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("project create", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	invite := fs.String("invite", "", "comma-separated agent names to invite")
	inviteUser := fs.String("invite-user", "", "comma-separated people to invite")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: project create NAME [--invite a,b] [--invite-user u,v]")
	}
	name, err := NormalizeChannelName(pos[0])
	if err != nil {
		return err
	}
	if err := ValidateProjectName(name); err != nil {
		return err
	}
	created, err := c.createChannel(ctx, name, *invite, *inviteUser)
	if err != nil {
		return err
	}
	// Created as the owner, so no bot is ever in it. Another person may
	// own it already; the project does not depend on it.
	usersID, _, err := ensureChannel(ctx, c.user, UsersChannelName(name), created.me.ownerID, userIDs(created.people))
	if err != nil {
		fmt.Fprintf(c.stderr, "slack-agent-chat: %v\n", err)
	}
	c.printJSON(map[string]any{"channel_id": created.id, "name": name, "users_channel_id": usersID})
	return c.subscribe(ctx, c.projectWatch(ctx, created.me, created.id, name), 0)
}

// projectJoin runs `chat project join NAME [--backlog N]`.
func (c *cli) projectJoin(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("project join", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	backlog := fs.Int("backlog", 0, "when the join starts, deliver this many recent messages of the project channel")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: project join NAME [--backlog N]")
	}
	id, name, err := c.resolveChannel(ctx, pos[0])
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
	// The backlog is the project channel's: the direct and side channels
	// start with nothing.
	return c.subscribe(ctx, c.projectWatch(ctx, me, id, name), *backlog, id)
}

// projectWatch returns the channels a session watches for project (whose
// channel is id): the project channel, this agent's direct channel with the
// owner (set up if missing; both get the watcher bot), and the side channels
// this agent was added to earlier, which a restarted session would otherwise
// stop watching.
func (c *cli) projectWatch(ctx context.Context, me identity, id, project string) []string {
	return projectWatchChannels(ctx, c.bot, c.stderr, me, id, project)
}

// projectAPI is what projectWatchChannels needs of the agent's bot token.
type projectAPI interface {
	channelMaker
	usersLister
}

func projectWatchChannels(ctx context.Context, api projectAPI, w io.Writer, me identity, id, project string) []string {
	ids := []string{id}
	// The project watch must not depend on the direct channel.
	if direct, err := ensureDirect(ctx, api, me, project); err != nil {
		fmt.Fprintf(w, "slack-agent-chat: no direct channel for #%s: %v\n", project, err)
	} else {
		ids = append(ids, direct)
	}
	// The watcher bot belongs in the project and direct channels; this
	// agent's bot is in both, so every create and join (re)invites it.
	addWatcher(ctx, api, api, w, ids...)
	derived, err := derivedChannels(ctx, api, project)
	if err != nil {
		fmt.Fprintf(w, "slack-agent-chat: listing #%s's side channels: %v\n", project, err)
	}
	return uniq(append(ids, derived...))
}

// projectArchive runs `chat project archive NAME [--dry-run]`.
func (c *cli) projectArchive(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("project archive", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	dryRun := fs.Bool("dry-run", false, "list the channels without archiving them")
	pos, err := parseArgs(fs, args)
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
