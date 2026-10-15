package agentchat

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/slack-go/slack"
)

// Channels derived from a project channel are named after it, then "__":
//
//	<project>__users           the people in the project, no bots
//	<project>__<user>_<agent>  one person and one of their agents
//	<project>__<agent>_<agent> a side channel; participants sorted
//
// so project names never contain "__", and user and agent names never
// contain "_" (and no bot is named "users").
const derivedSep = "__"

// maxProjectName leaves room in Slack's 80-character limit for the names of
// the channels derived from a project, such as side channels.
const maxProjectName = 32

// ValidateProjectName checks a normalized name for use as a project channel.
func ValidateProjectName(name string) error {
	if strings.Contains(name, derivedSep) {
		return fmt.Errorf("project names cannot contain %q; open side channels with `side`", derivedSep)
	}
	if len(name) > maxProjectName {
		return fmt.Errorf("project name %q is %d characters; the limit is %d, leaving room for derived channel names", name, len(name), maxProjectName)
	}
	return nil
}

// ProjectOf returns the project a channel belongs to: the name itself for a
// project channel, or the part before "__" for a derived one.
func ProjectOf(name string) (project string, derived bool) {
	project, _, derived = strings.Cut(name, derivedSep)
	return project, derived
}

// UsersChannelName names the project's people-only channel.
func UsersChannelName(project string) string { return project + derivedSep + "users" }

// DirectChannelName names the channel between a person and one agent.
func DirectChannelName(project, user, agent string) (string, error) {
	parts, err := nameParts([]string{user, agent})
	if err != nil {
		return "", err
	}
	return derivedName(project, parts)
}

// SideChannelName names the side channel for agents (the creator included),
// listing them sorted so every participant arrives at the same name.
func SideChannelName(project string, agents []string) (string, error) {
	parts, err := nameParts(agents)
	if err != nil {
		return "", err
	}
	slices.Sort(parts)
	return derivedName(project, slices.Compact(parts))
}

// nameParts normalizes user and agent names for use in a channel name.
func nameParts(names []string) ([]string, error) {
	parts := make([]string, len(names))
	for i, name := range names {
		n, err := NormalizeChannelName(name)
		if err != nil {
			return nil, err
		}
		if strings.Contains(n, "_") {
			return nil, fmt.Errorf("name %q contains _, which channel names use as a separator", name)
		}
		parts[i] = n
	}
	return parts, nil
}

func derivedName(project string, parts []string) (string, error) {
	name := project + derivedSep + strings.Join(parts, "_")
	if len(name) > 80 {
		return "", fmt.Errorf("#%s is longer than Slack's 80-character limit", name)
	}
	return name, nil
}

// shownName is the name Slack shows for a user or bot: the display name, else
// the real name (what bots without a display name show), else the username.
func shownName(u *slack.User) string {
	switch {
	case u.Profile.DisplayName != "":
		return u.Profile.DisplayName
	case u.RealName != "":
		return u.RealName
	}
	return u.Name
}

type channelLister interface {
	GetConversationsForUserContext(ctx context.Context, params *slack.GetConversationsForUserParameters) ([]slack.Channel, string, error)
}

type channelInviter interface {
	InviteUsersToConversationContext(ctx context.Context, channelID string, users ...string) (*slack.Channel, error)
}

// channelMaker can find, create and invite to channels as one Slack identity.
type channelMaker interface {
	channelLister
	channelInviter
	CreateConversationContext(ctx context.Context, params slack.CreateConversationParams) (*slack.Channel, error)
}

// findChannel returns the ID of the unarchived channel called name that the
// token's identity belongs to.
func findChannel(ctx context.Context, api channelLister, name string) (string, error) {
	cursor := ""
	for {
		chans, next, err := api.GetConversationsForUserContext(ctx, &slack.GetConversationsForUserParameters{
			Types: []string{"private_channel", "public_channel"}, ExcludeArchived: true, Limit: 200, Cursor: cursor,
		})
		if err != nil {
			return "", err
		}
		for _, ch := range chans {
			if ch.Name == name {
				return ch.ID, nil
			}
		}
		if next == "" {
			return "", fmt.Errorf("not a member of a channel named %q", name)
		}
		cursor = next
	}
}

// derivedChannels returns the IDs of the unarchived #project__… channels
// the token's identity belongs to.
func derivedChannels(ctx context.Context, api channelLister, project string) ([]string, error) {
	var ids []string
	cursor := ""
	for {
		chans, next, err := api.GetConversationsForUserContext(ctx, &slack.GetConversationsForUserParameters{
			Types: []string{"private_channel", "public_channel"}, ExcludeArchived: true, Limit: 200, Cursor: cursor,
		})
		if err != nil {
			return ids, err
		}
		for _, ch := range chans {
			if strings.HasPrefix(ch.Name, project+derivedSep) {
				ids = append(ids, ch.ID)
			}
		}
		if next == "" {
			return ids, nil
		}
		cursor = next
	}
}

// uniq drops repeated entries, keeping the first of each in order.
func uniq(s []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range s {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// inviteEach adds users to a channel one at a time, so one who is already a
// member does not stop the rest. self, the inviting identity, is skipped.
func inviteEach(ctx context.Context, api channelInviter, channelID, self string, users []string) error {
	for _, u := range users {
		if u == "" || u == self {
			continue
		}
		if _, err := api.InviteUsersToConversationContext(ctx, channelID, u); err != nil && !strings.Contains(err.Error(), "already_in_channel") {
			return fmt.Errorf("inviting %s: %w", u, err)
		}
	}
	return nil
}

// ensureChannel creates the private channel name as api's identity (self),
// or finds it if it already exists, and makes sure users are in it. created
// reports whether this call made it.
func ensureChannel(ctx context.Context, api channelMaker, name, self string, users []string) (id string, created bool, err error) {
	ch, err := api.CreateConversationContext(ctx, slack.CreateConversationParams{ChannelName: name, IsPrivate: true})
	switch {
	case err == nil:
		id, created = ch.ID, true
	case strings.Contains(err.Error(), "name_taken"):
		if id, err = findChannel(ctx, api, name); err != nil {
			return "", false, fmt.Errorf("#%s exists but this identity is not in it", name)
		}
	default:
		return "", false, fmt.Errorf("creating #%s: %w", name, err)
	}
	if err := inviteEach(ctx, api, id, self, users); err != nil {
		return "", false, fmt.Errorf("#%s: %w", name, err)
	}
	return id, created, nil
}
