package agentchat

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/slack-go/slack"
	"go.uber.org/zap"
)

const (
	// cohortViewTTL bounds how stale a project view used for live messages
	// may be; each tick reads a fresh one.
	cohortViewTTL = time.Minute
	// cohortFetchInterval is how often the listener fetches a project-state
	// checkout so it sees GM claims and holds published from other machines.
	cohortFetchInterval = 2 * time.Minute
	// cohortFetchTimeout bounds a project-state fetch so a hung network can
	// never stall the listener.
	cohortFetchTimeout = 20 * time.Second
	// watchMaxAge drops a GM watch nobody resolved, so state cannot grow
	// without bound.
	watchMaxAge = 24 * time.Hour
)

// GMWatch is one pending sign that a project's GM may be unavailable: an
// @mention of the GM nobody has seen it answer, or a BLOCKED notice it
// posted. Every listener that hosts a registered agent of the project keeps
// its own copy and reaches the same conclusions from the same facts.
type GMWatch struct {
	Project  string    `json:"project"`
	Channel  string    `json:"channel"`
	TS       string    `json:"ts"`               // the mention or BLOCKED message
	Thread   string    `json:"thread"`           // its thread root (TS for a top-level message)
	Sender   string    `json:"sender,omitempty"` // who mentioned the GM (name)
	SenderID string    `json:"sender_id,omitempty"`
	GM       string    `json:"gm"`
	GMID     string    `json:"gm_id"`
	Term     int       `json:"term"`
	Seen     time.Time `json:"seen"` // escalation counts from here
	Blocked  bool      `json:"blocked,omitempty"`
	// Notified records whom this listener has told, by session|agent (a
	// successor is told once, whatever step it falls at) or session|final.
	Notified map[string]bool `json:"notified,omitempty"`
}

func watchKey(channel, ts string) string { return channel + "|" + ts }

// cohortViews caches project views for the live message path.
type cohortViews struct {
	mu      sync.Mutex
	views   map[string]cohortView
	fetched map[string]time.Time // root → last fetch
}

type cohortView struct {
	p  *CohortProject
	at time.Time
}

// projectView reads project as the listener should trust it: from the
// project-state checkout's origin/main, fetched at most every
// cohortFetchInterval, so claims and holds published elsewhere count before
// anyone pulls; or from the working tree when root is not such a checkout.
// fresh skips the cache.
func (l *Listener) projectView(ctx context.Context, root, project string, fresh bool) (*CohortProject, error) {
	l.views.mu.Lock()
	if l.views.views == nil {
		l.views.views, l.views.fetched = map[string]cohortView{}, map[string]time.Time{}
	}
	key := root + "|" + project
	if v, ok := l.views.views[key]; ok && !fresh && l.Now().Sub(v.at) < cohortViewTTL {
		l.views.mu.Unlock()
		return v.p, nil
	}
	needFetch := l.Now().Sub(l.views.fetched[root]) >= cohortFetchInterval
	if needFetch {
		l.views.fetched[root] = l.Now() // claimed before fetching, so concurrent callers don't fetch too
	}
	l.views.mu.Unlock()

	var p *CohortProject
	var err error
	if _, gerr := gitIn(ctx, root, nil, nil, "rev-parse", "--verify", "-q", "refs/remotes/origin/main"); gerr == nil {
		if needFetch {
			fctx, cancel := context.WithTimeout(ctx, cohortFetchTimeout)
			if _, ferr := gitIn(fctx, root, nil, nil, "fetch", "-q", "origin", "+refs/heads/main:refs/remotes/origin/main"); ferr != nil {
				l.Log.Warn("fetching project-state failed; using the last fetch", zap.String("root", root), zap.Error(ferr))
			}
			cancel()
		}
		p, err = LoadCohortProjectAt(ctx, root, project, "refs/remotes/origin/main")
	} else {
		p, err = LoadCohortProject(root, project)
	}
	if err != nil {
		return nil, err
	}
	l.views.mu.Lock()
	l.views.views[key] = cohortView{p, l.Now()}
	l.views.mu.Unlock()
	return p, nil
}

// cohortControl answers the cohort-* control ops.
func (l *Listener) cohortControl(ctx context.Context, req ControlRequest) ControlResponse {
	var view *CohortProject
	if req.Op == "cohort-register" && l.Self.UserID != "" {
		l.selfName = l.name(ctx, l.Self.UserID)
	}
	if req.Op == "cohort-register" && req.Cohort != nil && req.Cohort.Root != "" {
		// Read the project as the tick will, before taking l.mu: it may run git.
		p, err := l.projectView(ctx, req.Cohort.Root, req.Cohort.Project, true)
		if err != nil {
			return ControlResponse{Error: err.Error()}
		}
		view = p
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if req.Op == "cohort-status" {
		want := func(project string) bool {
			return req.Cohort == nil || req.Cohort.Project == "" || project == req.Cohort.Project
		}
		var watches []GMWatch
		for _, w := range l.state.GMWatches {
			if want(w.Project) {
				watches = append(watches, *w)
			}
		}
		sort.Slice(watches, func(i, j int) bool { return TSLess(watches[i].TS, watches[j].TS) })
		return ControlResponse{OK: true, Watches: watches, Cohort: l.cohortRegs(func(r *CohortReg) bool { return want(r.Project) })}
	}
	if req.Cohort == nil || req.Cohort.Project == "" || req.SessionID == "" {
		return ControlResponse{Error: "a session and a project are required"}
	}
	key := cohortKey(req.SessionID, req.Cohort.Project)
	reg := l.state.Cohort[key]
	switch req.Op {
	case "cohort-register":
		next, err := l.validateRegistration(req.SessionID, req.Cohort, view)
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
func (l *Listener) validateRegistration(session string, in *CohortReg, p *CohortProject) (*CohortReg, error) {
	sub := l.state.Subscriptions[session]
	if sub == nil {
		return nil, errors.New("this session is not watching any channel; run watch start first")
	}
	if in.Channel == "" || !sub.Watches(in.Channel) {
		return nil, fmt.Errorf("this session does not watch the project channel %s", in.Channel)
	}
	if in.Root == "" || p == nil {
		return nil, errors.New("the project-state root is required")
	}
	if !p.InRoster(in.Agent) {
		return nil, fmt.Errorf("%s is not in %s's succession order", in.Agent, in.Project)
	}
	if self := l.selfName; self != "" && self != in.Agent {
		return nil, fmt.Errorf("this home's agent is %s, not %s", self, in.Agent)
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

// isAgentNamed reports whether id is a bot user shown as name. GM identity
// is the GM agent's bot, never a person who happens to share its name.
func (l *Listener) isAgentNamed(ctx context.Context, id, name string) bool {
	return id != "" && l.isAgent(ctx, id) && l.name(ctx, id) == name
}

// trackCohort updates the GM watches for a message in a registered
// project's channel: an @mention of the GM starts a watch, and the GM's own
// posts answer (a reply in the mention's thread after it, or a post that
// @mentions the sender) or report BLOCKED. Any other GM post does not answer
// a mention.
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
	p, err := l.projectView(ctx, reg.Root, reg.Project, false)
	if err != nil {
		return
	}
	gm := p.GM.GM
	fromGM := l.isAgentNamed(ctx, m.User, gm)
	var mentions []string // user IDs the message @mentions that are the GM's bot
	if !fromGM {
		for _, sm := range idMention.FindAllStringSubmatch(m.Text, -1) {
			if sm[1] != m.User && l.isAgentNamed(ctx, sm[1], gm) {
				mentions = append(mentions, sm[1])
			}
		}
	}
	sender := l.name(ctx, m.User)
	blocked := fromGM && strings.HasPrefix(m.Text, blockedPrefix(gm))
	thread := m.ThreadTS
	if thread == "" {
		thread = m.TS
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	changed := false
	if fromGM {
		for k, w := range l.state.GMWatches {
			if w.Channel != m.Channel || w.GM != gm || !TSLess(w.TS, m.TS) {
				continue
			}
			var answered bool
			if w.Blocked {
				answered = !blocked // any later GM post means it got unstuck
			} else {
				thread := w.Thread
				if thread == "" { // a watch saved before threads were tracked
					thread = w.TS
				}
				answered = m.ThreadTS == thread || mentionsUser(m.Text, w.SenderID)
			}
			if answered {
				delete(l.state.GMWatches, k)
				changed = true
			}
		}
		if blocked {
			l.state.GMWatches[watchKey(m.Channel, m.TS)] = &GMWatch{Project: reg.Project, Channel: m.Channel, TS: m.TS, Thread: thread,
				GM: gm, GMID: m.User, Term: p.GM.Term, Seen: l.Now().Add(-gmAnswerDeadline), Blocked: true}
			changed = true
		}
	} else if len(mentions) > 0 {
		l.state.GMWatches[watchKey(m.Channel, m.TS)] = &GMWatch{Project: reg.Project, Channel: m.Channel, TS: m.TS, Thread: thread,
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

func hasCheckFrom(m slack.Message, user string) bool {
	for _, r := range m.Reactions {
		if r.Name == "white_check_mark" || r.Name == "heavy_check_mark" {
			for _, u := range r.Users {
				if u == user {
					return true
				}
			}
		}
	}
	return false
}

// answeredInSlack checks Slack itself, which every home sees, for an answer
// the live stream may have missed. For a mention: the GM's reply in its
// thread after it, its ✅ (chat ack) on the mention, or a later top-level GM
// post @mentioning the sender. For a BLOCKED watch: any later GM post. A
// failed lookup counts as no answer.
func (l *Listener) answeredInSlack(ctx context.Context, w *GMWatch) bool {
	later := func(m slack.Message) bool { return m.User == w.GMID && TSLess(w.TS, m.Timestamp) }
	resp, err := l.API.GetConversationHistoryContext(ctx, &slack.GetConversationHistoryParameters{ChannelID: w.Channel, Oldest: w.TS, Limit: 200})
	if err == nil {
		for _, m := range resp.Messages {
			if later(m) && (w.Blocked || mentionsUser(m.Text, w.SenderID)) {
				return true
			}
		}
	}
	if w.Blocked {
		return false
	}
	thread := w.Thread
	if thread == "" {
		thread = w.TS
	}
	msgs, _, _, err := l.API.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{ChannelID: w.Channel, Timestamp: thread})
	if err != nil {
		return false
	}
	for _, m := range msgs {
		if m.Timestamp == w.TS && hasCheckFrom(m, w.GMID) {
			return true
		}
		if later(m) {
			return true
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
	sort.Slice(watches, func(i, j int) bool { return TSLess(watches[i].TS, watches[j].TS) })

	type loaded struct {
		p   *CohortProject
		err error
	}
	projects := map[string]loaded{}
	load := func(root, name string) (*CohortProject, error) {
		k := root + "|" + name
		if v, ok := projects[k]; ok {
			return v.p, v.err
		}
		p, err := l.projectView(ctx, root, name, true)
		if err != nil {
			l.Log.Warn("cohort project unreadable; skipping it this tick", zap.String("project", name), zap.Error(err))
		}
		projects[k] = loaded{p, err}
		return p, err
	}
	// A registration is dropped only for a definite reason (its session is
	// gone, or its agent left the roster), never for a read error.
	var dropRegs []string
	live := map[string]bool{} // projects with at least one registration
	for _, r := range regs {
		if subs[r.SessionID] == nil {
			dropRegs = append(dropRegs, cohortKey(r.SessionID, r.Project))
			continue
		}
		live[r.Project] = true
		if p, err := load(r.Root, r.Project); err == nil && !p.InRoster(r.Agent) {
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
		key := watchKey(w.Channel, w.TS)
		if !live[w.Project] || now.Sub(w.Seen) > watchMaxAge {
			dropWatches = append(dropWatches, key)
			continue
		}
		var p *CohortProject
		for _, r := range regs {
			if r.Project == w.Project {
				p, _ = load(r.Root, r.Project)
				break
			}
		}
		if p == nil {
			continue // unreadable this tick
		}
		if p.GM.Term != w.Term || p.GM.GM != w.GM {
			dropWatches = append(dropWatches, key) // a stale notice never evicts the new GM
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
			mark = "agent:" + succ[step]
			for _, r := range regs {
				if r.Project == w.Project && r.Agent == succ[step] && !r.OffDuty {
					targets = append(targets, r)
				}
			}
			text = successorNotice(w, p, step, succ[step])
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
			dropWatches = append(dropWatches, key)
			continue
		}
		for _, r := range due {
			sends = append(sends, send{r, r.SessionID + "|" + mark, text, key})
		}
	}
	for _, r := range regs {
		if r.OffDuty || subs[r.SessionID] == nil || !r.CheckpointDue(now) {
			continue
		}
		// A held agent gets no checkpoint wakeups; with the project
		// unreadable, nobody does this tick (fail closed).
		if p, err := load(r.Root, r.Project); err != nil || p.Held(r.Agent) {
			continue
		}
		sends = append(sends, send{reg: r, text: checkpointNotice(r)})
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

func successorNotice(w *GMWatch, p *CohortProject, step int, agent string) string {
	earlier := ""
	if step > 0 {
		earlier = " Earlier successors had their turn; one may not have been told if its listener was down, so check the channel for an ACTING GM post first."
	}
	return fmt.Sprintf("%s #%s: GM %s is unavailable: %s. You are next in the succession order (term %d, step %d).%s\n"+
		"If you can act now, claim with `slack-mcp-server chat gm claim --project %s --agent %s --expect-term %d --expect-gm %s` and follow projects/README.md. "+
		"If you cannot, do nothing: the next successor is told in %s.",
		cohortMarker, p.Name, w.GM, watchReason(w), w.Term, step+1, earlier, p.Name, agent, w.Term, w.GM, successorAckWindow)
}

func exhaustedNotice(w *GMWatch, p *CohortProject) string {
	return fmt.Sprintf("%s #%s: GM %s is unavailable (%s) and no successor has claimed. Tell the user in your direct channel.",
		cohortMarker, p.Name, w.GM, watchReason(w))
}

func checkpointNotice(r CohortReg) string {
	return fmt.Sprintf("%s #%s: 2-hour checkpoint due. Run your context self-check (silent if fine) and post your drift line with "+
		"`slack-mcp-server chat cohort checkpoint --project %s --drift \"<line>\"`.", cohortMarker, r.Project, r.Project)
}
