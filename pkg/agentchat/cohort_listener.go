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
	// recheckWindow is how long a possible GM signal is kept to be
	// classified again: well past the routine fetch interval (the gap between
	// a claim landing and this home seeing it) and the answer deadline.
	// Replay only takes messages sent after the claim (see replayRechecks).
	// maxRechecks bounds how many are kept, and claimSkew allows for clocks
	// when a kept message is compared with the claim time.
	recheckWindow = 30 * time.Minute
	maxRechecks   = 500
	claimSkew     = 30 * time.Second
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

// tsTime is the instant a Slack timestamp names. Deadlines count from the
// message itself, not from when this listener saw it, so a reconnect, a
// backlog or a slow machine never restarts the clock.
func tsTime(ts string) time.Time {
	sec, micro := splitTS(ts)
	return time.Unix(sec, micro*1000)
}

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
	self := ""
	if req.Op == "cohort-register" {
		// The socket is the enforcement point: an agent registers only as
		// this home's bot, and an unresolvable bot refuses the registration.
		if self = l.name(ctx, l.Self.UserID); self == "" || l.Self.UserID == "" {
			return ControlResponse{Error: "cannot resolve this home's agent; registration refused"}
		}
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
		next, err := l.validateRegistration(req.SessionID, req.Cohort, view, self)
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
func (l *Listener) validateRegistration(session string, in *CohortReg, p *CohortProject, self string) (*CohortReg, error) {
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
	if self != in.Agent {
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
	// A message that mentions, or is a BLOCKED notice from, a roster agent
	// the cached view does not name as GM may follow an authority change the
	// cache has not seen. Re-read the checkout first (fetching only if the
	// routine interval is due, as any view read does); if it still names
	// another GM, keep the message so each tick classifies it again.
	// Otherwise that single mention would be lost for good.
	if l.mayNameUncachedGM(ctx, m, p) {
		if fresh, err := l.projectView(ctx, reg.Root, reg.Project, true); err == nil {
			p = fresh
		}
		if l.mayNameUncachedGM(ctx, m, p) {
			l.rememberRecheck(m)
		}
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
				GM: gm, GMID: m.User, Term: p.GM.Term, Seen: tsTime(m.TS).Add(-gmAnswerDeadline), Blocked: true}
			changed = true
		}
	} else if len(mentions) > 0 {
		l.state.GMWatches[watchKey(m.Channel, m.TS)] = &GMWatch{Project: reg.Project, Channel: m.Channel, TS: m.TS, Thread: thread,
			Sender: sender, SenderID: m.User, GM: gm, GMID: mentions[0], Term: p.GM.Term, Seen: tsTime(m.TS)}
		changed = true
	}
	if changed {
		if err := l.state.Save(l.StateFile); err != nil {
			l.Log.Error("saving state failed", zap.Error(err))
		}
	}
}

// mayNameUncachedGM reports whether m @mentions a roster agent other than
// p's GM, or is a BLOCKED notice from one: either is a GM signal if the
// authority changed after p was read.
func (l *Listener) mayNameUncachedGM(ctx context.Context, m Message, p *CohortProject) bool {
	other := func(userID string) (string, bool) {
		name := l.name(ctx, userID)
		return name, name != "" && name != p.GM.GM && p.InRoster(name) && l.isAgent(ctx, userID)
	}
	if name, ok := other(m.User); ok && strings.HasPrefix(m.Text, blockedPrefix(name)) {
		return true
	}
	for _, sm := range idMention.FindAllStringSubmatch(m.Text, -1) {
		if sm[1] == m.User {
			continue
		}
		if _, ok := other(sm[1]); ok {
			return true
		}
	}
	return false
}

// recheck is a possible GM signal kept until it can be classified against a
// view that names its agent as GM, or until it expires.
type recheck struct {
	m     Message
	until time.Time
}

// rememberRecheck keeps m to be classified again (bounded; the oldest is
// dropped first).
func (l *Listener) rememberRecheck(m Message) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.rechecks == nil {
		l.rechecks = map[string]recheck{}
	}
	if len(l.rechecks) >= maxRechecks {
		var oldest string
		for k, r := range l.rechecks {
			if oldest == "" || TSLess(r.m.TS, l.rechecks[oldest].m.TS) {
				oldest = k
			}
		}
		delete(l.rechecks, oldest)
	}
	l.rechecks[watchKey(m.Channel, m.TS)] = recheck{m: m, until: tsTime(m.TS).Add(recheckWindow)}
}

// replayRechecks classifies each kept message again, in timestamp order,
// once a freshly read view names one of its mentioned bots (or its BLOCKED
// sender) as GM, and only if the message was sent after that authority began
// (a mention of an agent before it became GM owed no GM answer). A watch it
// creates keeps the message's own timestamp and is dropped at once if Slack
// shows the GM already answered, since that answer arrived before the watch
// existed.
func (l *Listener) replayRechecks(ctx context.Context) {
	now := l.Now()
	l.mu.Lock()
	var due []Message
	for k, r := range l.rechecks {
		if now.After(r.until) {
			delete(l.rechecks, k)
			continue
		}
		due = append(due, r.m)
	}
	var regs []CohortReg
	for _, r := range l.state.Cohort {
		regs = append(regs, *r)
	}
	l.mu.Unlock()
	sort.Slice(due, func(i, j int) bool { return TSLess(due[i].TS, due[j].TS) })
	views := map[string]*CohortProject{}
	for _, m := range due {
		key := watchKey(m.Channel, m.TS)
		forget := func() {
			l.mu.Lock()
			delete(l.rechecks, key)
			l.mu.Unlock()
		}
		var reg *CohortReg
		for i := range regs {
			if regs[i].Channel == m.Channel {
				reg = &regs[i]
				break
			}
		}
		if reg == nil {
			forget()
			continue
		}
		vk := reg.Root + "|" + reg.Project
		p, ok := views[vk]
		if !ok {
			var err error
			if p, err = l.projectView(ctx, reg.Root, reg.Project, true); err != nil {
				continue // unreadable this tick; try again next tick
			}
			views[vk] = p
		}
		if !l.namesGM(ctx, m, p) {
			continue // not (yet) a signal for the current GM
		}
		forget()
		if since, err := time.Parse(time.RFC3339, p.GM.Since); err == nil && tsTime(m.TS).Before(since.Add(-claimSkew)) {
			continue // sent before this GM's authority began
		}
		l.mu.Lock()
		_, existed := l.state.GMWatches[key]
		l.mu.Unlock()
		if existed {
			continue // already classified live; keep its notification marks
		}
		l.trackCohort(ctx, m)
		l.mu.Lock()
		var w *GMWatch
		if cur := l.state.GMWatches[key]; cur != nil {
			c := *cur
			w = &c
		}
		l.mu.Unlock()
		if w == nil {
			continue
		}
		if answered, err := l.answeredInSlackErr(ctx, w); err == nil && answered {
			l.mu.Lock()
			delete(l.state.GMWatches, key)
			if err := l.state.Save(l.StateFile); err != nil {
				l.Log.Error("saving state failed", zap.Error(err))
			}
			l.mu.Unlock()
		}
	}
}

// namesGM reports whether m @mentions p's GM's bot, or is a BLOCKED notice
// from it: what the classifier treats as a GM signal.
func (l *Listener) namesGM(ctx context.Context, m Message, p *CohortProject) bool {
	gm := p.GM.GM
	if l.isAgentNamed(ctx, m.User, gm) && strings.HasPrefix(m.Text, blockedPrefix(gm)) {
		return true
	}
	for _, sm := range idMention.FindAllStringSubmatch(m.Text, -1) {
		if sm[1] != m.User && l.isAgentNamed(ctx, sm[1], gm) {
			return true
		}
	}
	return false
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
	answered, _ := l.answeredInSlackErr(ctx, w)
	return answered
}

// answeredInSlackErr is answeredInSlack that reports a failed lookup, for
// callers that must fail closed.
func (l *Listener) answeredInSlackErr(ctx context.Context, w *GMWatch) (bool, error) {
	later := func(m slack.Message) bool { return m.User == w.GMID && TSLess(w.TS, m.Timestamp) }
	resp, err := l.API.GetConversationHistoryContext(ctx, &slack.GetConversationHistoryParameters{ChannelID: w.Channel, Oldest: w.TS, Limit: 200})
	if err != nil {
		return false, err
	}
	for _, m := range resp.Messages {
		if later(m) && (w.Blocked || mentionsUser(m.Text, w.SenderID)) {
			return true, nil
		}
	}
	if w.Blocked {
		return false, nil
	}
	thread := w.Thread
	if thread == "" {
		thread = w.TS
	}
	msgs, _, _, err := l.API.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{ChannelID: w.Channel, Timestamp: thread})
	if err != nil {
		return false, err
	}
	for _, m := range msgs {
		if m.Timestamp == w.TS && hasCheckFrom(m, w.GMID) {
			return true, nil
		}
		if later(m) {
			return true, nil
		}
	}
	return false, nil
}

// CohortTick runs the cohort deadlines: GM watches that have come due notify
// the right successor (or, with none left, every registered agent here), and
// 2-hourly checkpoints notify each registered agent. The daemon calls it
// periodically.
func (l *Listener) CohortTick(ctx context.Context) {
	// First classify kept possible GM signals again, against freshly read
	// views (fetched on the routine interval), so a watch they create is
	// handled by this same tick.
	l.replayRechecks(ctx)
	l.cohortTick(ctx)
}

func (l *Listener) cohortTick(ctx context.Context) {
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

	l.checkApprovalWaits(ctx, regs, subs, now)

	delivered := map[int]bool{}
	for i, s := range sends {
		// A watch retired since the snapshot (an UNBLOCKED notice this
		// tick) no longer escalates.
		if s.watch != "" {
			l.mu.Lock()
			_, live := l.state.GMWatches[s.watch]
			l.mu.Unlock()
			if !live {
				continue
			}
		}
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

// errApprovalUnobservable means a session's runtime does not expose whether
// it is waiting on an approval.
var errApprovalUnobservable = errors.New("approval waits are not observable for this session")

// approvalProber is implemented by deliverers that can tell whether a
// session is stuck on an approval prompt (Codex, through thread/read).
type approvalProber interface {
	WaitingOnApproval(ctx context.Context, sub *Subscription) (bool, error)
}

// approvalProbeTimeout bounds one thread/read, so a stuck daemon cannot
// stall the cohort tick.
const approvalProbeTimeout = 10 * time.Second

// warnUnobservable logs, once per session, that its Codex daemon does not
// report approval waits, so no BLOCKED notice can be posted for it.
func (l *Listener) warnUnobservable(session string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.unobservable == nil {
		l.unobservable = map[string]bool{}
	}
	if l.unobservable[session] {
		return
	}
	l.unobservable[session] = true
	l.Log.Warn("this Codex daemon does not report approval waits; no BLOCKED notice can be posted for the session", zap.String("session", session))
}

type approvalWait struct {
	since  time.Time
	posted bool
}

// checkApprovalWaits posts BLOCKED for a registered session that has been
// waiting on an approval for the approval-hook's wait (10 minutes): the
// Codex counterpart of the Claude hook's notice. Each wait posts once; a
// cleared flag or a failed observation resets it, so nothing is ever posted
// on a guess. The count starts when this listener first sees the wait.
func (l *Listener) checkApprovalWaits(ctx context.Context, regs []CohortReg, subs map[string]*Subscription, now time.Time) {
	prober, ok := l.Deliverer.(approvalProber)
	if !ok {
		return
	}
	// One probe per session per tick, however many projects it is in; each
	// registration keeps its own wait, so every project channel hears.
	type probe struct {
		waiting bool
		err     error
	}
	probes := map[string]probe{}
	seen := map[string]bool{}
	for _, r := range regs {
		sub := subs[r.SessionID]
		if sub == nil || sub.Kind != KindCodex {
			continue
		}
		key := cohortKey(r.SessionID, r.Project)
		l.mu.Lock()
		if l.approvalWaits == nil {
			l.approvalWaits = map[string]*approvalWait{}
		}
		w := l.approvalWaits[key]
		// The wait is memory-only; after a restart, durable evidence
		// rebuilds it. Our own bot's saved BLOCKED watch proves delivery:
		// the session is not announced again. The announcement record alone
		// only means a notice may be in Slack: recovery stays owed, but a
		// session still waiting starts a fresh observed count and is
		// announced again after it (a duplicate beats a missing notice).
		// Neither is read as proof of a continuous wait.
		if w == nil && l.ownBlockedWatchLocked(r) {
			w = &approvalWait{since: now, posted: true}
			l.approvalWaits[key] = w
		} else if w == nil && l.state.Announced[key] {
			w = &approvalWait{} // unstarted: the count begins at the next known waiting read
			l.approvalWaits[key] = w
		}
		// owed: a BLOCKED notice may be in Slack (posted, or attempted with
		// an uncertain result), so a known clear must announce recovery.
		owed := (w != nil && w.posted) || l.state.Announced[key]
		l.mu.Unlock()
		// Only an agent the cohort could act on is announced: on duty, on
		// the roster, not held, in a readable project. Anything else drops
		// a pending count (a notice already posted keeps its recovery).
		p, err := l.projectView(ctx, r.Root, r.Project, false)
		if err != nil || r.OffDuty || !p.InRoster(r.Agent) || p.Held(r.Agent) {
			if !owed {
				l.mu.Lock()
				delete(l.approvalWaits, key)
				l.mu.Unlock()
				continue
			}
		} else {
			seen[key] = true
		}
		pr, done := probes[r.SessionID]
		if !done {
			pctx, cancel := context.WithTimeout(ctx, approvalProbeTimeout)
			pr.waiting, pr.err = prober.WaitingOnApproval(pctx, sub)
			cancel()
			probes[r.SessionID] = pr
			if errors.Is(pr.err, errApprovalUnobservable) {
				l.warnUnobservable(r.SessionID)
			}
		}
		switch {
		case pr.err != nil:
			// Unknown: never post on a guess, and never assert recovery.
			// A pending count restarts; a posted notice waits for a known
			// answer.
			seen[key] = owed
			if owed && w != nil && !w.posted {
				// Recovery stays owed; the observed count is unstarted until
				// the next known waiting read.
				l.mu.Lock()
				w.since = time.Time{}
				l.mu.Unlock()
			}
			if !seen[key] {
				l.mu.Lock()
				delete(l.approvalWaits, key)
				l.mu.Unlock()
			}
		case !pr.waiting:
			if owed {
				seen[key] = !l.postUnblocked(ctx, r, key)
				if seen[key] {
					continue // retry the recovery notice next tick
				}
			}
			l.mu.Lock()
			delete(l.approvalWaits, key)
			l.mu.Unlock()
		case seen[key]:
			if w == nil {
				w = &approvalWait{since: now}
				l.mu.Lock()
				l.approvalWaits[key] = w
				l.mu.Unlock()
			}
			if !w.posted && w.since.IsZero() {
				// The first known, eligible waiting read starts the count;
				// unknown or ineligible time before it is never counted.
				l.mu.Lock()
				w.since = now
				l.mu.Unlock()
			}
			if w.posted || now.Sub(w.since) < defaultApprovalWait {
				continue
			}
			// Our own notice already reached Slack (its event created our
			// watch): that is delivery, so do not post a duplicate.
			l.mu.Lock()
			delivered := l.ownBlockedWatchLocked(r)
			if delivered {
				w.posted = true
			}
			l.mu.Unlock()
			if delivered {
				continue
			}
			// Record the announcement before making it: a crash after
			// Slack accepts the post must still leave a recovery owed.
			l.mu.Lock()
			l.state.Announced[key] = true
			err := l.state.Save(l.StateFile)
			l.mu.Unlock()
			if err != nil {
				l.Log.Error("saving state failed; BLOCKED not posted", zap.Error(err))
				continue
			}
			if _, _, err := l.API.PostMessageContext(ctx, r.Channel, slack.MsgOptionText(blockedNotice(r.Agent, "", defaultApprovalWait), false)); err != nil {
				l.Log.Warn("posting a Codex BLOCKED notice failed", zap.String("session", r.SessionID), zap.Error(err))
				continue
			}
			l.mu.Lock()
			w.posted = true
			l.mu.Unlock()
		default:
			// Still waiting but now ineligible: keep the owed recovery; an
			// unposted retry's count is unstarted until it is eligible again.
			seen[key] = true
			if !w.posted {
				l.mu.Lock()
				w.since = time.Time{}
				l.mu.Unlock()
			}
		}
	}
	// A registration that is gone takes its wait with it, so a later
	// registration starts fresh.
	l.mu.Lock()
	for key := range l.approvalWaits {
		if !seen[key] {
			delete(l.approvalWaits, key)
		}
	}
	// A durable announcement outlives its in-memory wait only while its
	// registration does.
	pruned := false
	for key := range l.state.Announced {
		if _, ok := l.state.Cohort[key]; !ok {
			delete(l.state.Announced, key)
			pruned = true
		}
	}
	if pruned {
		if err := l.state.Save(l.StateFile); err != nil {
			l.Log.Error("saving state failed", zap.Error(err))
		}
	}
	for session := range l.unobservable {
		if _, ok := probes[session]; !ok {
			delete(l.unobservable, session)
		}
	}
	l.mu.Unlock()
}

// ownBlockedWatchLocked reports whether a durable BLOCKED watch in r's
// channel comes from this listener's own bot as r's agent. l.mu is held.
func (l *Listener) ownBlockedWatchLocked(r CohortReg) bool {
	for _, w := range l.state.GMWatches {
		if w.Blocked && w.Channel == r.Channel && w.GM == r.Agent && w.GMID == l.Self.UserID {
			return true
		}
	}
	return false
}

// unblockedNotice is posted when a Codex session that was announced BLOCKED
// is known to have its approval answered. It is a later, non-BLOCKED post
// from the agent's own bot, which every listener's trackCohort already takes
// as the end of a BLOCKED GM watch.
func unblockedNotice(agent string) string {
	return "UNBLOCKED: " + slackEscaper.Replace(agent) + " is no longer waiting for approval."
}

// postUnblocked announces r's recovery and retires this listener's own
// BLOCKED watches for it at once (the others retire theirs from the Slack
// event). It reports whether the notice was posted.
func (l *Listener) postUnblocked(ctx context.Context, r CohortReg, key string) bool {
	if _, _, err := l.API.PostMessageContext(ctx, r.Channel, slack.MsgOptionText(unblockedNotice(r.Agent), false)); err != nil {
		l.Log.Warn("posting a Codex UNBLOCKED notice failed", zap.String("session", r.SessionID), zap.Error(err))
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	changed := l.state.Announced[key]
	delete(l.state.Announced, key)
	for k, w := range l.state.GMWatches {
		if w.Blocked && w.Channel == r.Channel && w.GM == r.Agent {
			delete(l.state.GMWatches, k)
			changed = true
		}
	}
	if changed {
		if err := l.state.Save(l.StateFile); err != nil {
			l.Log.Error("saving state failed", zap.Error(err))
		}
	}
	return true
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

// claimCheck is the listener-side eligibility gate for `chat gm claim`: the
// session is registered as that agent and on duty, the agent is not held,
// the authority is still the expected GM and term, and a GM deadline for
// them has come due with this agent's successor slot reached.
func (l *Listener) claimCheck(ctx context.Context, req ControlRequest) ControlResponse {
	if req.Cohort == nil || req.Expect == nil || req.SessionID == "" {
		return ControlResponse{Error: "a session, project, agent and expected authority are required"}
	}
	l.mu.Lock()
	reg := l.state.Cohort[cohortKey(req.SessionID, req.Cohort.Project)]
	var r CohortReg
	if reg != nil {
		r = *reg
	}
	sub := l.state.Subscriptions[req.SessionID]
	var watches []GMWatch
	for _, w := range l.state.GMWatches {
		if w.Project == req.Cohort.Project {
			watches = append(watches, *w)
		}
	}
	l.mu.Unlock()
	switch {
	case reg == nil || sub == nil:
		return ControlResponse{Error: "this session is not registered in " + req.Cohort.Project}
	case r.Agent != req.Cohort.Agent:
		return ControlResponse{Error: fmt.Sprintf("this session is registered as %s, not %s", r.Agent, req.Cohort.Agent)}
	case r.OffDuty:
		return ControlResponse{Error: "this session is off duty"}
	}
	// Fresh native liveness, not the saved subscription; unknown is not alive.
	alive, err := l.Deliverer.Alive(ctx, sub)
	if err != nil {
		return ControlResponse{Error: "this session's liveness cannot be confirmed: " + err.Error(), Unavailable: true}
	}
	if !alive {
		return ControlResponse{Error: "this session is no longer running"}
	}
	p, err := l.projectView(ctx, r.Root, r.Project, true)
	if err != nil {
		return ControlResponse{Error: "the project is unreadable: " + err.Error(), Unavailable: true}
	}
	switch {
	case p.Held(r.Agent):
		return ControlResponse{Error: r.Agent + " is on hold"}
	case p.GM.Term != req.Expect.Term || p.GM.GM != req.Expect.GM:
		return ControlResponse{Error: fmt.Sprintf("the authority is term %d (%s), not the expected term %d (%s)", p.GM.Term, p.GM.GM, req.Expect.Term, req.Expect.GM)}
	}
	if req.UserDirected {
		return ControlResponse{OK: true, Text: "user-directed: deadline check skipped"}
	}
	succ := p.Successors()
	now := l.Now()
	for i := range watches {
		w := &watches[i]
		if w.GM != p.GM.GM || w.Term != p.GM.Term {
			continue
		}
		step := EscalationStep(w.Seen, now)
		if step < 0 {
			continue
		}
		slot := false
		for j, a := range succ {
			if a == r.Agent && j <= step {
				slot = true
			}
		}
		if !slot {
			continue
		}
		// The deadline must still be unanswered now, on evidence: a failed
		// lookup refuses rather than assumes.
		answered, err := l.answeredInSlackErr(ctx, w)
		if err != nil {
			return ControlResponse{Error: "cannot confirm the deadline is still unanswered: " + err.Error(), Unavailable: true}
		}
		if answered {
			l.mu.Lock()
			delete(l.state.GMWatches, watchKey(w.Channel, w.TS))
			if err := l.state.Save(l.StateFile); err != nil {
				l.Log.Error("saving state failed", zap.Error(err))
			}
			l.mu.Unlock()
			continue
		}
		return ControlResponse{OK: true, Text: fmt.Sprintf("deadline %s, step %d", w.TS, step+1)}
	}
	return ControlResponse{Error: fmt.Sprintf("no GM deadline for %s (term %d) has reached %s's slot", p.GM.GM, p.GM.Term, r.Agent)}
}
