package agentchat

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/slack-go/slack"
	"go.uber.org/zap"
)

// GMWatch is one pending sign that a project's GM may be unavailable: an
// @mention of the GM nobody has seen it answer, or a BLOCKED notice it
// posted. Every listener that hosts a registered agent of the project keeps
// its own copy and reaches the same conclusions from the same facts.
type GMWatch struct {
	Project  string    `json:"project"`
	Channel  string    `json:"channel"`
	TS       string    `json:"ts"`
	Sender   string    `json:"sender,omitempty"` // who mentioned the GM (name)
	SenderID string    `json:"sender_id,omitempty"`
	GM       string    `json:"gm"`
	GMID     string    `json:"gm_id"`
	Term     int       `json:"term"`
	Seen     time.Time `json:"seen"` // escalation counts from here
	Blocked  bool      `json:"blocked,omitempty"`
	// Notified records the sessions this listener has told, by
	// session|step, so each one is told once.
	Notified map[string]bool `json:"notified,omitempty"`
}

func watchKey(channel, ts string) string { return channel + "|" + ts }

// cohortControl answers the cohort-* control ops.
func (l *Listener) cohortControl(req ControlRequest) ControlResponse {
	l.mu.Lock()
	defer l.mu.Unlock()
	if req.Op == "cohort-status" {
		return ControlResponse{OK: true, Cohort: l.cohortRegs(func(r *CohortReg) bool {
			return req.Cohort == nil || req.Cohort.Project == "" || r.Project == req.Cohort.Project
		})}
	}
	if req.Cohort == nil || req.Cohort.Project == "" || req.SessionID == "" {
		return ControlResponse{Error: "a session and a project are required"}
	}
	key := cohortKey(req.SessionID, req.Cohort.Project)
	reg := l.state.Cohort[key]
	switch req.Op {
	case "cohort-register":
		next, err := l.validateRegistration(req.SessionID, req.Cohort)
		if err != nil {
			return ControlResponse{Error: err.Error()}
		}
		if reg != nil && reg.Agent == next.Agent { // re-registering keeps the checkpoint history
			next.RegisteredAt, next.LastCheckpoint, next.CheckpointNotified, next.OffDuty =
				reg.RegisteredAt, reg.LastCheckpoint, reg.CheckpointNotified, reg.OffDuty
		}
		l.state.Cohort[key] = next
		reg = next
	case "cohort-leave":
		delete(l.state.Cohort, key)
		reg = nil
	case "cohort-duty", "cohort-checkpoint":
		if reg == nil {
			return ControlResponse{Error: "this session is not registered in " + req.Cohort.Project}
		}
		if req.Op == "cohort-checkpoint" {
			reg.LastCheckpoint = l.Now()
		} else {
			switch req.Text {
			case "off":
				reg.OffDuty = true
			case "on":
				reg.OffDuty = false
			default:
				return ControlResponse{Error: `duty is "on" or "off"`}
			}
		}
	}
	if err := l.state.Save(l.StateFile); err != nil {
		l.Log.Error("saving state failed", zap.Error(err))
	}
	resp := ControlResponse{OK: true}
	if reg != nil {
		resp.Cohort = []CohortReg{*reg}
	}
	return resp
}

// validateRegistration checks a registration against the live subscription
// and the project files. Call with l.mu held.
func (l *Listener) validateRegistration(session string, in *CohortReg) (*CohortReg, error) {
	sub := l.state.Subscriptions[session]
	if sub == nil {
		return nil, errors.New("this session is not watching any channel; run watch start first")
	}
	if in.Channel == "" || !sub.Watches(in.Channel) {
		return nil, fmt.Errorf("this session does not watch the project channel %s", in.Channel)
	}
	if in.Root == "" {
		return nil, errors.New("the project-state root is required")
	}
	p, err := LoadCohortProject(in.Root, in.Project)
	if err != nil {
		return nil, err
	}
	if !p.InRoster(in.Agent) {
		return nil, fmt.Errorf("%s is not in %s's succession order", in.Agent, in.Project)
	}
	return &CohortReg{Project: in.Project, Agent: in.Agent, SessionID: session, Root: in.Root, Channel: in.Channel, RegisteredAt: l.Now()}, nil
}

// cohortRegs copies the registrations keep picks, sorted. Call with l.mu held.
func (l *Listener) cohortRegs(keep func(*CohortReg) bool) []CohortReg {
	out := []CohortReg{}
	for _, r := range l.state.Cohort {
		if keep(r) {
			out = append(out, *r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return cohortKey(out[i].SessionID, out[i].Project) < cohortKey(out[j].SessionID, out[j].Project)
	})
	return out
}

// trackCohort updates the GM watches for a message in a registered
// project's channel: an @mention of the GM starts a watch, and the GM's own
// posts answer (a reply in the mention's thread, or one that @mentions the
// sender) or report BLOCKED. Any other GM post does not answer a mention.
func (l *Listener) trackCohort(ctx context.Context, m Message) {
	l.mu.Lock()
	var reg *CohortReg
	for _, r := range l.state.Cohort {
		if r.Channel == m.Channel {
			c := *r
			reg = &c
			break
		}
	}
	l.mu.Unlock()
	if reg == nil || m.User == "" {
		return
	}
	p, err := LoadCohortProject(reg.Root, reg.Project)
	if err != nil {
		return
	}
	gm := p.GM.GM
	sender := l.name(ctx, m.User)
	var mentions []string // user IDs the message @mentions that are the GM
	for _, sm := range idMention.FindAllStringSubmatch(m.Text, -1) {
		if sm[1] != m.User && l.name(ctx, sm[1]) == gm {
			mentions = append(mentions, sm[1])
		}
	}
	blocked := strings.HasPrefix(m.Text, blockedPrefix(gm))

	l.mu.Lock()
	defer l.mu.Unlock()
	changed := false
	if sender == gm {
		for k, w := range l.state.GMWatches {
			if w.Channel != m.Channel || w.GM != gm {
				continue
			}
			answered := w.Blocked && !blocked || !w.Blocked && (m.ThreadTS == w.TS || mentionsUser(m.Text, w.SenderID))
			if answered {
				delete(l.state.GMWatches, k)
				changed = true
			}
		}
		if blocked {
			l.state.GMWatches[watchKey(m.Channel, m.TS)] = &GMWatch{Project: reg.Project, Channel: m.Channel, TS: m.TS,
				GM: gm, GMID: m.User, Term: p.GM.Term, Seen: l.Now().Add(-gmAnswerDeadline), Blocked: true}
			changed = true
		}
	} else if len(mentions) > 0 {
		l.state.GMWatches[watchKey(m.Channel, m.TS)] = &GMWatch{Project: reg.Project, Channel: m.Channel, TS: m.TS,
			Sender: sender, SenderID: m.User, GM: gm, GMID: mentions[0], Term: p.GM.Term, Seen: l.Now()}
		changed = true
	}
	if changed {
		if err := l.state.Save(l.StateFile); err != nil {
			l.Log.Error("saving state failed", zap.Error(err))
		}
	}
}

func blockedPrefix(agent string) string { return "BLOCKED: " + slackEscaper.Replace(agent) + " " }

func mentionsUser(text, userID string) bool {
	if userID == "" {
		return false
	}
	for _, sm := range idMention.FindAllStringSubmatch(text, -1) {
		if sm[1] == userID {
			return true
		}
	}
	return false
}

// answeredInSlack checks Slack itself, which every home sees, for an answer
// the live stream may have missed: the GM's reply in the mention's thread or
// its ✅ (chat ack) on the mention; for a BLOCKED watch, any later GM post.
// A failed lookup counts as no answer.
func (l *Listener) answeredInSlack(ctx context.Context, w *GMWatch) bool {
	if w.Blocked {
		resp, err := l.API.GetConversationHistoryContext(ctx, &slack.GetConversationHistoryParameters{ChannelID: w.Channel, Oldest: w.TS, Limit: 100})
		if err != nil {
			return false
		}
		for _, m := range resp.Messages {
			if m.User == w.GMID && m.Timestamp != w.TS {
				return true
			}
		}
		return false
	}
	msgs, _, _, err := l.API.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{ChannelID: w.Channel, Timestamp: w.TS})
	if err != nil {
		return false
	}
	for i, m := range msgs {
		if i > 0 && m.User == w.GMID {
			return true
		}
		if m.Timestamp == w.TS {
			for _, r := range m.Reactions {
				if r.Name == "white_check_mark" || r.Name == "heavy_check_mark" {
					for _, u := range r.Users {
						if u == w.GMID {
							return true
						}
					}
				}
			}
		}
	}
	return false
}

// CohortTick runs the cohort deadlines: GM watches that have come due notify
// the right successor (or, with none left, every registered agent here), and
// 2-hourly checkpoints notify each registered agent. The daemon calls it
// periodically.
func (l *Listener) CohortTick(ctx context.Context) {
	now := l.Now()
	l.mu.Lock()
	regs := l.cohortRegs(func(*CohortReg) bool { return true })
	watches := make([]GMWatch, 0, len(l.state.GMWatches))
	for _, w := range l.state.GMWatches {
		watches = append(watches, *w)
	}
	subs := map[string]*Subscription{}
	for _, r := range regs {
		if s := l.state.Subscriptions[r.SessionID]; s != nil {
			subs[r.SessionID] = s
		}
	}
	l.mu.Unlock()
	sort.Slice(watches, func(i, j int) bool { return watches[i].TS < watches[j].TS })

	projects := map[string]*CohortProject{}
	load := func(root, name string) *CohortProject {
		k := root + "|" + name
		if p, ok := projects[k]; ok {
			return p
		}
		p, err := LoadCohortProject(root, name)
		if err != nil {
			l.Log.Warn("cohort project unreadable", zap.String("project", name), zap.Error(err))
			p = nil
		}
		projects[k] = p
		return p
	}
	var dropRegs []string
	for _, r := range regs { // registrations must stay valid: live session, agent still on the roster
		p := load(r.Root, r.Project)
		if subs[r.SessionID] == nil || p == nil || !p.InRoster(r.Agent) {
			dropRegs = append(dropRegs, cohortKey(r.SessionID, r.Project))
		}
	}

	type send struct {
		reg        CohortReg
		mark, text string
		watch      string
	}
	var sends []send
	var dropWatches []string
	for i := range watches {
		w := &watches[i]
		var p *CohortProject
		for _, r := range regs {
			if r.Project == w.Project {
				p = load(r.Root, r.Project)
				break
			}
		}
		if p == nil {
			dropWatches = append(dropWatches, watchKey(w.Channel, w.TS)) // no registered agent here any more
			continue
		}
		if p.GM.Term != w.Term || p.GM.GM != w.GM {
			dropWatches = append(dropWatches, watchKey(w.Channel, w.TS)) // a stale notice never evicts the new GM
			continue
		}
		step := EscalationStep(w.Seen, now)
		if step < 0 || p.Held(w.GM) {
			continue
		}
		var targets []CohortReg
		var text, mark string
		succ := p.Successors()
		if step < len(succ) {
			mark = fmt.Sprintf("step%d", step)
			for _, r := range regs {
				if r.Project == w.Project && r.Agent == succ[step] && !r.OffDuty {
					targets = append(targets, r)
				}
			}
			text = successorNotice(w, p, step)
		} else {
			mark = "final"
			for _, r := range regs {
				if r.Project == w.Project && !r.OffDuty && r.Agent != w.GM {
					targets = append(targets, r)
				}
			}
			text = exhaustedNotice(w, p)
		}
		var due []CohortReg
		for _, r := range targets {
			if !w.Notified[r.SessionID+"|"+mark] {
				due = append(due, r)
			}
		}
		if len(due) == 0 {
			continue
		}
		if l.answeredInSlack(ctx, w) {
			dropWatches = append(dropWatches, watchKey(w.Channel, w.TS))
			continue
		}
		for _, r := range due {
			sends = append(sends, send{r, r.SessionID + "|" + mark, text, watchKey(w.Channel, w.TS)})
		}
	}
	for _, r := range regs {
		if !r.OffDuty && r.CheckpointDue(now) {
			sends = append(sends, send{reg: r, text: checkpointNotice(r)})
		}
	}

	delivered := map[int]bool{}
	for i, s := range sends {
		sub := subs[s.reg.SessionID]
		if sub == nil || l.deliverCohort(ctx, sub, s.watch+"|"+s.mark+"|"+NowTS(now), s.text) != nil {
			continue
		}
		delivered[i] = true
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range dropRegs {
		delete(l.state.Cohort, k)
	}
	for _, k := range dropWatches {
		delete(l.state.GMWatches, k)
	}
	for i, s := range sends {
		if !delivered[i] {
			continue
		}
		if s.watch == "" {
			if r := l.state.Cohort[cohortKey(s.reg.SessionID, s.reg.Project)]; r != nil {
				r.CheckpointNotified = now
			}
			continue
		}
		if w := l.state.GMWatches[s.watch]; w != nil {
			if w.Notified == nil {
				w.Notified = map[string]bool{}
			}
			w.Notified[s.mark] = true
		}
	}
	if len(dropRegs)+len(dropWatches)+len(delivered) > 0 {
		if err := l.state.Save(l.StateFile); err != nil {
			l.Log.Error("saving state failed", zap.Error(err))
		}
	}
}

// deliverCohort pushes a listener-generated notice (not a Slack message) into
// a session.
func (l *Listener) deliverCohort(ctx context.Context, sub *Subscription, key, text string) error {
	sl := l.sessionLock(sub.SessionID)
	sl.Lock()
	defer sl.Unlock()
	if _, err := l.Deliverer.Deliver(ctx, sub, clientMessageID(sub.SessionID, "cohort", key), text); err != nil {
		l.Log.Warn("cohort notice delivery failed", zap.String("session", sub.SessionID), zap.Error(err))
		return err
	}
	l.Log.Info("delivered cohort notice", zap.String("session", sub.SessionID))
	return nil
}

const cohortMarker = noticeMarker + " [cohort]"

func watchReason(w *GMWatch) string {
	if w.Blocked {
		return fmt.Sprintf("it posted BLOCKED (ts %s)", w.TS)
	}
	return fmt.Sprintf("%s @mentioned it at ts %s and it has not answered for %s", w.Sender, w.TS, gmAnswerDeadline)
}

func successorNotice(w *GMWatch, p *CohortProject, step int) string {
	return fmt.Sprintf("%s #%s: GM %s is unavailable: %s. You are next in the succession order (term %d, step %d).\n"+
		"If you can act now, claim with `slack-mcp-server chat gm claim --project %s --expect-term %d --expect-gm %s` and follow projects/README.md. "+
		"If you cannot, do nothing: the next successor is told in %s.",
		cohortMarker, p.Name, w.GM, watchReason(w), w.Term, step+1, p.Name, w.Term, w.GM, successorAckWindow)
}

func exhaustedNotice(w *GMWatch, p *CohortProject) string {
	return fmt.Sprintf("%s #%s: GM %s is unavailable (%s) and no successor has claimed. Tell the user in your direct channel.",
		cohortMarker, p.Name, w.GM, watchReason(w))
}

func checkpointNotice(r CohortReg) string {
	return fmt.Sprintf("%s #%s: 2-hour checkpoint due. Run your context self-check (silent if fine) and post your drift line with "+
		"`slack-mcp-server chat cohort checkpoint --project %s --drift \"<line>\"`.", cohortMarker, r.Project, r.Project)
}
