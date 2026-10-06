package agentchat

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/slack-go/slack"
)

// A turn the user started by typing at the terminal is marked when its
// prompt is submitted (relay-hook, UserPromptSubmit); when the turn ends
// (stop-hook, Stop), a marked turn's final response is sent to the owner as
// a DM from this agent's bot. Turns started by Slack notices, background
// work or subagents are never marked, so they send nothing.

// noticeMarker opens every notice the listener pushes into a session.
const noticeMarker = "[slack-agent-chat]"

func turnMarkerPath(home Home, session string) (string, error) {
	if session == "" || session != filepath.Base(session) || strings.HasPrefix(session, ".") {
		return "", fmt.Errorf("unusable session ID %q", session)
	}
	return filepath.Join(home.StateDir, "terminal-turns", session), nil
}

// turnKey identifies a turn across its UserPromptSubmit and Stop hooks:
// Claude Code's prompt_id or Codex's turn_id ("" if the host sends neither).
func turnKey(promptID, turnID string) string {
	if promptID != "" {
		return promptID
	}
	return turnID
}

// MarkTurn records whether the turn prompt starts was typed at the terminal,
// storing the turn's key so a mark left by a turn that never reached Stop
// (blocked or interrupted) cannot match a later one.
func MarkTurn(home Home, session, key, prompt string) error {
	path, err := turnMarkerPath(home, session)
	if err != nil {
		return err
	}
	if strings.Contains(prompt, noticeMarker) {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(key), 0o600)
}

// ClearTurn drops session's mark.
func ClearTurn(home Home, session string) {
	if path, err := turnMarkerPath(home, session); err == nil {
		_ = os.Remove(path)
	}
}

// TakeTurnMark reports whether session's ending turn (key) was typed at the
// terminal, clearing the mark. When both the mark and key carry a turn key
// they must match.
func TakeTurnMark(home Home, session, key string) bool {
	path, err := turnMarkerPath(home, session)
	if err != nil {
		return false
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	_ = os.Remove(path)
	return len(stored) == 0 || key == "" || string(stored) == key
}

// maxDMChunk keeps each message under Slack's 12,000-character limit for
// markdown blocks.
const maxDMChunk = 10000

// SplitMarkdown splits text into chunks of at most max runes at line breaks,
// closing a code fence at the end of a chunk and reopening it in the next.
// A line too long for any chunk is split mid-line.
func SplitMarkdown(text string, max int) []string {
	const closing = len("\n```")
	var chunks []string
	var cur strings.Builder
	curLen, fence := 0, ""
	reopen := func() int {
		if fence == "" {
			return 0
		}
		return len([]rune(fence)) + 1
	}
	flush := func() {
		s := cur.String()
		if fence != "" {
			if !strings.HasSuffix(s, "\n") {
				s += "\n"
			}
			s += "```"
		}
		chunks = append(chunks, s)
		cur.Reset()
		curLen = reopen()
		if fence != "" {
			cur.WriteString(fence + "\n")
		}
	}
	for _, line := range strings.SplitAfter(text, "\n") {
		r := []rune(line)
		for {
			room := max - closing - curLen
			if len(r) <= room {
				break
			}
			if curLen > reopen() && len(r) <= max-closing-reopen() {
				flush() // the whole line fits in the next chunk
				continue
			}
			if room <= 0 {
				if curLen <= reopen() {
					break // max is too small to make progress
				}
				flush()
				continue
			}
			cur.WriteString(string(r[:room]))
			curLen += room
			r = r[room:]
			flush()
		}
		cur.WriteString(string(r))
		curLen += len(r)
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "```") {
			if fence == "" {
				fence = t
			} else {
				fence = ""
			}
		}
	}
	if curLen > reopen() {
		chunks = append(chunks, cur.String())
	}
	return chunks
}

// stopHook (Stop) DMs the owner the final response of a turn they started
// at the terminal. Stop fires only for the main agent, never for subagents.
func (c *cli) stopHook(ctx context.Context, ev hookEvent) int {
	if !TakeTurnMark(c.home, c.hookSession(ev), turnKey(ev.PromptID, ev.TurnID)) || strings.TrimSpace(ev.LastAssistantMessage) == "" {
		return 0
	}
	me, err := c.identity(ctx)
	if err != nil {
		fmt.Fprintf(c.stderr, "slack-agent-chat: %v\n", err)
		return 0
	}
	where := ""
	if ev.Cwd != "" {
		where = " in `" + filepath.Base(ev.Cwd) + "`"
	}
	header := fmt.Sprintf("**%s** finished%s:\n\n", me.agentName, where)
	for i, chunk := range SplitMarkdown(ev.LastAssistantMessage, maxDMChunk-len(header)) {
		text := chunk
		if i == 0 {
			text = header + chunk
		}
		fallback := fmt.Sprintf("%s finished%s", me.agentName, strings.ReplaceAll(where, "`", ""))
		if _, _, err := c.bot.PostMessageContext(ctx, me.ownerID,
			slack.MsgOptionText(fallback, false),
			slack.MsgOptionBlocks(slack.NewMarkdownBlock("", text))); err != nil {
			fmt.Fprintf(c.stderr, "slack-agent-chat: sending turn DM: %v\n", err)
			return 0
		}
	}
	return 0
}
