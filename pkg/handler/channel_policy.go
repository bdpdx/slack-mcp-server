package handler

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/korotovsky/slack-mcp-server/pkg/provider"
	"github.com/korotovsky/slack-mcp-server/pkg/toolconfig"
	"go.uber.org/zap"
)

// errPolicyUnresolved means a deny-list entry could not be resolved to a
// channel ID, so the policy cannot prove the target is not denied.
var errPolicyUnresolved = errors.New("channel policy entry could not be resolved")

// channelsSnapshot returns the current channels cache, or an empty one when
// no provider is configured (unit tests).
func (ch *ConversationsHandler) channelsSnapshot() *provider.ChannelsCache {
	if ch.channelsFn != nil {
		return ch.channelsFn()
	}
	if ch.apiProvider == nil {
		return &provider.ChannelsCache{Channels: map[string]provider.Channel{}, ChannelsInv: map[string]string{}}
	}
	return ch.apiProvider.ProvideChannelsMaps()
}

// lookupChannelName resolves a lower-cased #channel or @user name to its ID
// using the cache, falling back to a case-insensitive scan.
func lookupChannelName(cache *provider.ChannelsCache, name string) (string, bool) {
	if cache == nil {
		return "", false
	}
	if id, ok := cache.ChannelsInv[name]; ok {
		return id, true
	}
	for key, id := range cache.ChannelsInv {
		if strings.EqualFold(key, name) {
			return id, true
		}
	}
	return "", false
}

// findDMChannel returns the ID of the cached 1:1 DM with userID.
func findDMChannel(cache *provider.ChannelsCache, userID string) (string, bool) {
	if cache == nil {
		return "", false
	}
	for _, c := range cache.Channels {
		if c.IsIM && strings.EqualFold(c.User, userID) {
			return c.ID, true
		}
	}
	return "", false
}

// policyEntryMatches reports whether a canonical policy entry refers to the
// canonical channel ID. resolved is false when a name entry is unknown.
func policyEntryMatches(entry, channelID string, cache *provider.ChannelsCache) (matched, resolved bool) {
	switch {
	case strings.HasPrefix(entry, "#") || strings.HasPrefix(entry, "@"):
		id, ok := lookupChannelName(cache, entry)
		if !ok {
			return false, false
		}
		return strings.EqualFold(id, channelID), true
	case isSlackUserIDPrefix(entry):
		// A user ID in the policy stands for the DM with that user.
		if entry == channelID {
			return true, true
		}
		if cache != nil {
			if c, ok := cache.Channels[channelID]; ok && c.IsIM && strings.EqualFold(c.User, entry) {
				return true, true
			}
		}
		return false, true
	default:
		return entry == channelID, true
	}
}

// policyAllows reports whether the canonical channel ID passes the policy.
// A deny-list entry that cannot be resolved fails closed.
func policyAllows(policy toolconfig.ChannelPolicy, channelID string, cache *provider.ChannelsCache) (bool, error) {
	switch policy.Mode {
	case toolconfig.PolicyAll:
		return true, nil
	case toolconfig.PolicyAllow:
		for _, entry := range policy.Entries {
			if matched, _ := policyEntryMatches(entry, channelID, cache); matched {
				return true, nil
			}
		}
		return false, nil
	case toolconfig.PolicyDeny:
		var unresolved bool
		for _, entry := range policy.Entries {
			matched, resolved := policyEntryMatches(entry, channelID, cache)
			if matched {
				return false, nil
			}
			if !resolved {
				unresolved = true
			}
		}
		if unresolved {
			return false, errPolicyUnresolved
		}
		return true, nil
	}
	return false, nil
}

// canonicalWriteTarget resolves a channel argument for a write tool to an
// upper-cased channel ID. #name and @name are resolved through the cache.
// When the policy names specific channels, a user ID is mapped to the
// cached DM with that user, or refused, so a denied DM cannot be reached by
// addressing the user directly.
func (ch *ConversationsHandler) canonicalWriteTarget(ctx context.Context, raw string, policy toolconfig.ChannelPolicy) (string, error) {
	channel := strings.TrimSpace(raw)
	if channel == "" {
		return "", errors.New("channel_id is required")
	}
	if strings.HasPrefix(channel, "#") || strings.HasPrefix(channel, "@") {
		resolved, err := ch.resolveChannelID(ctx, channel)
		if err != nil {
			return "", err
		}
		channel = resolved
	}
	channel = strings.ToUpper(strings.TrimSpace(channel))
	if isSlackUserIDPrefix(channel) && policy.Restricted() {
		dm, ok := findDMChannel(ch.channelsSnapshot(), channel)
		if !ok {
			return "", fmt.Errorf("user ID %q cannot be checked against the channel policy; pass the DM channel ID (Dxxxxxxxxxx) or @username instead", channel)
		}
		channel = dm
	}
	return channel, nil
}

// checkWriteTarget enforces that tool is enabled and that the target channel
// passes its channel policy. It returns the canonical channel ID. The policy
// itself is logged but never included in the error returned to the model.
func (ch *ConversationsHandler) checkWriteTarget(ctx context.Context, tool, raw string) (string, error) {
	policy := ch.cfg.ChannelPolicy(tool)
	if policy.Mode == toolconfig.PolicyOff {
		return "", toolconfig.DisabledError(tool)
	}
	channel, err := ch.canonicalWriteTarget(ctx, raw, policy)
	if err != nil {
		return "", err
	}
	allowed, err := policyAllows(policy, channel, ch.channelsSnapshot())
	if err != nil || !allowed {
		ch.logger.Warn("Tool not allowed for channel by policy",
			zap.String("tool", tool),
			zap.String("channel", channel),
			zap.Strings("policy_entries", policy.Entries),
			zap.Error(err),
		)
		return "", fmt.Errorf("%s is not allowed for channel %q by the server's channel policy", tool, channel)
	}
	return channel, nil
}

// checkToolEnabled returns an error when tool is disabled.
func (ch *ConversationsHandler) checkToolEnabled(tool string) error {
	if !ch.cfg.ToolEnabled(tool) {
		return toolconfig.DisabledError(tool)
	}
	return nil
}

// checkAsUser refuses as_user=true unless SLACK_MCP_ALLOW_AS_USER is on.
func (ch *ConversationsHandler) checkAsUser(asUser bool) error {
	if asUser && (ch.cfg == nil || !ch.cfg.AllowAsUser) {
		return fmt.Errorf("as_user is disabled; set %s=true to allow acting as the user", toolconfig.EnvAllowAsUser)
	}
	return nil
}
