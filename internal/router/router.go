// Package router maps the short names an agent speaks ("boss", "#rp-main",
// "dm:333") onto Discord channel IDs -- a DM is a channel too, addressed by the
// peer's user id -- and enforces which of them a given principal may read or
// write. Those bits are per agent: a target declares defaults, an agent may
// tighten or widen them.
//
// It is deliberately pure: no Discord calls, no I/O, safe to hit from gateway
// handler goroutines and tool goroutines alike.
package router

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"mcp-discord-roleplay/internal/config"
)

// Principal identifies the MCP client that issued a request. With stdio there
// is exactly one (from --agent); the future HTTP transport will derive it from
// the bearer token per request.
type Principal struct {
	AgentID string `json:"agent"`
}

type ctxKey int

const principalCtxKey ctxKey = 1

// WithPrincipal attaches p to ctx for downstream handlers.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalCtxKey, p)
}

// FromContext returns the principal, or a zero Principal when none was set.
func FromContext(ctx context.Context) Principal {
	if ctx == nil {
		return Principal{}
	}
	if p, ok := ctx.Value(principalCtxKey).(Principal); ok {
		return p
	}
	return Principal{}
}

// Rule kinds. A DM is a channel: there are two kinds of real target, and the
// id behind "dm:" is the PEER USER's snowflake (a DM's channel id is not
// knowable before the first message, while the user id is what events carry).
// A rule may also be a wildcard of its kind (ID "*"), which claims everything
// the kind's exact rules did not match.
const (
	KindChannel = "channel"
	KindDM      = "dm"
	KindMe      = "me" // the bot's own note-to-self DM, resolved lazily
)

// Grant is one agent's effective permission on one target: the target's
// read/write defaults with that agent's own override applied. An override may
// loosen as well as tighten, because the shape people actually want is a
// read-only wildcard that exactly one agent is allowed to answer.
type Grant struct {
	Read  bool
	Write bool
}

// Rule is one routing entry (or an ad-hoc rule derived from the allow-lists).
type Rule struct {
	Alias    string `json:"alias"`
	Kind     string `json:"kind"`
	ID       string `json:"id,omitempty"`
	Target   string `json:"target"`
	Name     string `json:"name,omitempty"`
	Agent    string `json:"agent,omitempty"`
	Read     bool   `json:"read"`
	Write    bool   `json:"write"`
	Wildcard bool   `json:"wildcard,omitempty"` // ID == "*": claims what exact rules miss
	Source   string `json:"source"`             // routing | ad-hoc | self
}

// Router answers lookup and ACL questions.
type Router struct {
	ordered  []*Rule
	byAlias  map[string]*Rule // lowercased alias
	byTarget map[string]*Rule // canonical "channel:123"
	byChan   map[string]*Rule // channel id -> rule
	byDM     map[string]*Rule // peer user id -> rule
	byName   map[string]*Rule // lowercased channel/user name -> rule
	wild     map[string]*Rule // kind -> that kind's wildcard rule (at most one)

	agentRules  map[string]map[string]*Rule // agentID -> target -> most specific rule it holds
	agentGrants map[string]map[string]Grant // agentID -> target -> effective read/write
	anyRead     map[string]bool             // target -> some agent may read it (base read or an override)
	hasAgents   bool

	allowGuilds   map[string]bool
	allowChannels map[string]bool // canonical target forms AND aliases, raw ids included
	denyChannels  map[string]bool
	unknownChan   string // ignore | ring
	defaultAgent  string
}

// New builds a Router from the resolved configuration (routing.json plus the
// command-line allow/deny lists).
func New(cfg *config.Config) (*Router, error) {
	rt := &Router{
		byAlias:       map[string]*Rule{},
		byTarget:      map[string]*Rule{},
		byChan:        map[string]*Rule{},
		byDM:          map[string]*Rule{},
		byName:        map[string]*Rule{},
		wild:          map[string]*Rule{},
		agentRules:    map[string]map[string]*Rule{},
		agentGrants:   map[string]map[string]Grant{},
		anyRead:       map[string]bool{},
		allowGuilds:   toSet(cfg.AllowGuilds),
		allowChannels: toGateSet(cfg.AllowChannels),
		denyChannels:  toGateSet(cfg.DenyChannels),
		unknownChan:   cfg.UnknownChannel,
		defaultAgent:  cfg.AgentID,
	}
	if rt.unknownChan == "" {
		rt.unknownChan = "ignore"
	}

	r := cfg.Routing
	if r != nil {
		for i := range r.Targets {
			t := &r.Targets[i]
			kind, id, err := config.NormalizeTargetID(t.ID)
			if err != nil {
				return nil, fmt.Errorf("target %s: %w", t.Alias, err)
			}
			ru := &Rule{
				Alias:    t.Alias,
				Kind:     kind,
				ID:       id,
				Target:   config.CanonicalTarget(kind, id),
				Name:     t.Name,
				Agent:    t.Agent,
				Read:     t.Allowed(t.Read),
				Write:    t.Allowed(t.Write),
				Wildcard: config.IsWildcard(id),
				Source:   "routing",
			}
			rt.add(ru)
		}
	}

	if r != nil && len(r.Agents) > 0 {
		rt.hasAgents = true
		for i := range r.Agents {
			a := &r.Agents[i]
			set := rt.agentRules[a.ID]
			if set == nil {
				set = map[string]*Rule{}
				rt.agentRules[a.ID] = set
			}
			grants := rt.agentGrants[a.ID]
			if grants == nil {
				grants = map[string]Grant{}
				rt.agentGrants[a.ID] = grants
			}
			for _, ref := range a.Targets {
				ru, ok := rt.byAlias[strings.ToLower(ref.Alias)]
				if !ok {
					continue // routing.validate already rejects this
				}
				g := Grant{Read: ru.Read, Write: ru.Write}
				if ref.Read != nil {
					g.Read = *ref.Read
				}
				if ref.Write != nil {
					g.Write = *ref.Write
				}
				set[ru.Target] = ru
				grants[ru.Target] = g
			}
		}
		// A target that names an agent implicitly belongs to that agent.
		for _, ru := range rt.ordered {
			if ru.Agent == "" {
				continue
			}
			set := rt.agentRules[ru.Agent]
			if set == nil {
				set = map[string]*Rule{}
				rt.agentRules[ru.Agent] = set
				rt.agentGrants[ru.Agent] = map[string]Grant{}
			}
			set[ru.Target] = ru
		}
	}
	// An agent that references a target with read:true has to be able to receive
	// it, whatever the target's own default said; this is the gate's only peek at
	// per-agent state.
	for _, ru := range rt.ordered {
		if ru.Read {
			rt.anyRead[ru.Target] = true
			continue
		}
		for _, grants := range rt.agentGrants {
			if g, ok := grants[ru.Target]; ok && g.Read {
				rt.anyRead[ru.Target] = true
				break
			}
		}
	}
	return rt, nil
}

// toGateSet indexes an allow/deny list for membership tests. Entries are aliases
// or target ids, and both forms have to answer: a channel id appears here as
// well as in its canonical "channel:<id>" form, and a wildcard as "dm:*".
func toGateSet(list []string) map[string]bool {
	set := map[string]bool{}
	for _, s := range list {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		set[s] = true
		if kind, id, err := config.NormalizeTargetID(s); err == nil {
			set[config.CanonicalTarget(kind, id)] = true
			if kind == KindChannel {
				set[id] = true // a bare snowflake is how inbound messages arrive
			}
		} else {
			set[strings.ToLower(s)] = true
			set[strings.TrimPrefix(strings.ToLower(s), "#")] = true
		}
	}
	return set
}

func (rt *Router) add(ru *Rule) {
	low := strings.ToLower(ru.Alias)
	if _, dup := rt.byAlias[low]; dup {
		return // config validation already rejects duplicates
	}
	rt.ordered = append(rt.ordered, ru)
	rt.byAlias[low] = ru
	rt.byTarget[ru.Target] = ru
	switch {
	case ru.Wildcard:
		// One wildcard per kind, which routing.validate already guarantees by
		// rejecting a duplicate canonical target.
		rt.wild[ru.Kind] = ru
	case ru.Kind == KindChannel:
		rt.byChan[ru.ID] = ru
	case ru.Kind == KindDM:
		rt.byDM[ru.ID] = ru
	}
	if ru.Name != "" {
		rt.byName[normalizeName(ru.Name)] = ru
	}
}

// HasRouting reports whether routing.json declared any target. When it did,
// ad-hoc targets (bare IDs not listed in the file) are rejected.
func (rt *Router) HasRouting() bool { return len(rt.ordered) > 0 }

// DeclaresAgent reports whether the principal may see anything at all. Once
// routing.json declares agents, an agent nobody declared resolves to an empty
// world (every send fails with foreign_agent) — a silent trap worth warning
// about at startup instead.
func (rt *Router) DeclaresAgent(id string) bool {
	if !rt.hasAgents {
		return true
	}
	_, ok := rt.agentRules[id]
	return ok
}

// AgentIDs lists every agent id that needs its own queue: the agents declared
// in routing.json (both the agents[] entries and any target that names one)
// plus the process default, so a poll without _meta.agentId still reads from
// somewhere. Sorted, always contains at least one entry.
func (rt *Router) AgentIDs() []string {
	set := map[string]bool{rt.defaultID(): true}
	for a := range rt.agentRules {
		if a != "" {
			set[a] = true
		}
	}
	for _, ru := range rt.ordered {
		if ru.Agent != "" {
			set[ru.Agent] = true
		}
	}
	out := make([]string, 0, len(set))
	for a := range set {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// Resolve turns a caller-supplied target reference into a Rule.
// Accepted forms: alias, channel:<id>, dm:<id>, channel:*, dm:*, #name, @name,
// "me", or a bare snowflake (treated as a channel id).
func (rt *Router) Resolve(ref string) (*Rule, bool) {
	s := strings.TrimSpace(ref)
	if s == "" {
		return nil, false
	}
	if strings.EqualFold(s, "me") || strings.EqualFold(s, "self") {
		return &Rule{Alias: "me", Kind: KindMe, Target: "me", Read: true, Write: true, Source: "self"}, true
	}
	if ru, ok := rt.byAlias[strings.ToLower(s)]; ok {
		return ru, true
	}
	if kind, id, err := config.NormalizeTargetID(s); err == nil {
		if ru, ok := rt.byTarget[config.CanonicalTarget(kind, id)]; ok {
			return ru, true
		}
		// An unlisted channel still resolves when its kind's wildcard claims it.
		// Skipping this would let "dm:<stranger>" fall through to the ad-hoc rule
		// and write with rights the "dm:*" rule never granted.
		if ru, ok := rt.wild[kind]; ok {
			return ru, true
		}
		if rt.HasRouting() {
			return nil, false
		}
		return rt.adhoc(kind, id)
	}
	if ru, ok := rt.byName[normalizeName(s)]; ok {
		return ru, true
	}
	return nil, false
}

// adhoc builds an unlisted rule governed only by the allow/deny lists. It runs
// only when routing.json has no rules at all.
func (rt *Router) adhoc(kind, id string) (*Rule, bool) {
	target := config.CanonicalTarget(kind, id)
	wildcard := config.CanonicalTarget(kind, config.WildcardID)
	if rt.denyChannels[target] || rt.denyChannels[id] || rt.denyChannels[wildcard] {
		return nil, false
	}
	if len(rt.allowChannels) > 0 &&
		!rt.allowChannels[target] && !rt.allowChannels[id] && !rt.allowChannels[wildcard] {
		return nil, false
	}
	return &Rule{
		Alias: target, Kind: kind, ID: id, Target: target,
		Read: true, Write: true, Source: "ad-hoc",
	}, true
}

// RuleForChannel returns the declared rule for a channel id, if any.
func (rt *Router) RuleForChannel(channelID string) *Rule { return rt.byChan[channelID] }

// RuleForMessage finds the rule that owns an inbound event. For DMs it first
// tries the channel id, then any recipient id (the other human). When nothing
// exact answers, the kind's wildcard owns it.
func (rt *Router) RuleForMessage(channelID string, isDM bool, recipients []string) *Rule {
	if ru := rt.byChan[channelID]; ru != nil {
		return ru
	}
	if isDM {
		for _, u := range recipients {
			if ru := rt.byDM[u]; ru != nil {
				return ru
			}
		}
		return rt.wild[KindDM]
	}
	return rt.wild[KindChannel]
}

// Admission reasons returned by AdmitInbound. "" means "store this event".
//
// M2c retired "dm_disabled": a DM is a channel, so the fence is the same one
// every channel uses -- put "dm:*" in --deny-channels to close DMs.
const (
	AdmitOK          = ""
	AdmitNoRuleIgnor = "no_rule_ignored"
	AdmitDenied      = "denied_channel"
	AdmitNotListed   = "not_allowlisted"
	AdmitReadDenied  = "read_denied"
)

// AdmitInbound decides whether an observed event may enter a queue at all, and
// which rule owns it. Audience is what the bus actually calls: it answers the
// same policy per agent. AdmitInbound is kept as the single readable statement
// of the policy order and of its reason codes (the vocabulary DESIGN.md uses).
// They duplicate the same rules, so if you change one, change the other; M3
// folds them into one pass.
func (rt *Router) AdmitInbound(guildID, channelID string, isDM bool, recipients []string) (*Rule, string) {
	ru := rt.RuleForMessage(channelID, isDM, recipients)
	if rt.denied(isDM, channelID, recipients, ru) {
		return nil, AdmitDenied
	}
	if ru != nil {
		// A target nobody may read is shut; one that only loosened an agent's own
		// read bit is not (that is what per-agent grants are for).
		if !ru.Read && !rt.anyRead[ru.Target] {
			return nil, AdmitReadDenied
		}
		return ru, AdmitOK
	}
	// Nothing routed it: the allow-list gets first say, then unknown_channel
	// decides the stranger -- a DM exactly like a guild channel.
	if rt.admitted(isDM, channelID, recipients) {
		return nil, AdmitOK
	}
	if len(rt.allowChannels) > 0 {
		return nil, AdmitNotListed
	}
	if len(rt.allowGuilds) > 0 {
		if isDM || !rt.allowGuilds[guildID] {
			return nil, AdmitNotListed
		}
		return nil, AdmitOK // an allow-listed guild is an explicit yes
	}
	if rt.unknownChan == "ring" {
		return nil, AdmitOK // stored under its raw channel id
	}
	return nil, AdmitNoRuleIgnor
}

// AliasFor returns the name an agent should see for a channel id.
func (rt *Router) AliasFor(channelID string, isDM bool, recipients []string) string {
	if ru := rt.RuleForMessage(channelID, isDM, recipients); ru != nil {
		return ru.Alias
	}
	return "channel:" + channelID
}

// ---------------------------------------------------------------------------
// Inbound audience (M2): who is allowed to read an event
// ---------------------------------------------------------------------------

// AudienceEntry is one agent that may read an observed inbound event.
type AudienceEntry struct {
	Agent  string `json:"agent"`
	Alias  string `json:"alias,omitempty"` // "" = admitted by policy, no rule owns it
	Name   string `json:"name,omitempty"`
	Target string `json:"target"`
}

// Audience returns every agent allowed to read an inbound event, sorted by
// agent id. It is the fan-out key for the per-agent queues: an event is pushed
// only into these queues.
//
// It walks ALL rules rather than the byChan/byDM single-value indexes, and
// that is the whole point: those indexes keep one rule per id (last declaration
// wins), so a channel listed twice under two aliases for two agents would only
// ever reach one of them.
//
// matched is ordered specific-first (exact rules, then the kind's wildcard), and
// each agent is answered by the MOST SPECIFIC rule it holds -- the rule's read
// bit included. That is what keeps an agent listed under both "ami" and "dm:*"
// from receiving the same message twice, without letting another agent's exact
// rule cost it the wildcard copy.
//
// Events that no rule owns but policy admits (a guild channel passing the
// allow-lists, a stranger with unknown_channel="ring") go to the DEFAULT agent
// only. They are never broadcast: an unknown channel is nobody's business until
// an agent claims it.
//
// An empty result means "nobody may read this", and the event is dropped.
func (rt *Router) Audience(guildID, channelID string, isDM bool, recipients []string) []AudienceEntry {
	var matched, wild []*Rule
	for _, ru := range rt.ordered {
		switch ru.Kind {
		case KindChannel:
			if isDM {
				continue
			}
			if ru.ID == channelID {
				matched = append(matched, ru)
			} else if ru.Wildcard {
				wild = append(wild, ru)
			}
		case KindDM:
			if !isDM {
				continue
			}
			found := false
			for _, u := range recipients {
				if u != "" && ru.ID == u {
					found = true
					break
				}
			}
			if found {
				matched = append(matched, ru)
			} else if ru.Wildcard {
				wild = append(wild, ru)
			}
		}
	}
	matched = append(matched, wild...)
	// Deny after matching, so an entry written as an ALIAS is honoured too
	// (gateKeys only knows ids and wildcards).
	if rt.denied(isDM, channelID, recipients, matched...) {
		return nil
	}

	claimed := map[string]bool{}
	seen := map[string]AudienceEntry{}
	for _, ru := range matched {
		for _, a := range rt.agentsSeeing(ru) {
			if claimed[a] {
				continue // already answered by a more specific rule this agent holds
			}
			claimed[a] = true
			if !rt.grantFor(a, ru).Read || !rt.visible(Principal{AgentID: a}, ru) {
				continue
			}
			seen[a] = AudienceEntry{Agent: a, Alias: ru.Alias, Name: ru.Name, Target: ru.Target}
		}
	}
	if len(matched) > 0 {
		// A rule owns this channel: read_denied and foreign_agent are final, they
		// must not fall through to the stray handler below.
		return sortAudience(seen)
	}

	if !rt.admitStray(guildID, channelID, isDM, recipients) {
		return nil
	}
	def := rt.defaultID()
	seen[def] = AudienceEntry{Agent: def, Target: "channel:" + channelID}
	return sortAudience(seen)
}

// admitStray applies the no-rule branch of the inbound policy: the allow-lists
// get first say, then unknown_channel. M2c gave guild channels and DMs the same
// path -- there is no allow_dm left to ask about.
func (rt *Router) admitStray(guildID, channelID string, isDM bool, recipients []string) bool {
	if rt.admitted(isDM, channelID, recipients) {
		return true
	}
	if len(rt.allowChannels) > 0 {
		return false
	}
	if len(rt.allowGuilds) > 0 {
		// An allow-listed guild is an explicit yes; unknown_channel only speaks
		// when nothing in the lists mentioned the message at all.
		return !isDM && rt.allowGuilds[guildID]
	}
	return rt.unknownChan == "ring"
}

// agentsSeeing lists the principals for which visible() accepts ru.
//
// An explicit owner wins even when the file has no agents[] block: otherwise a
// target marked "agent":"a9" would be read by "default" here while SendTarget
// still refused it as foreign_agent, and no config author expects that split.
func (rt *Router) agentsSeeing(ru *Rule) []string {
	if ru.Agent != "" {
		return []string{ru.Agent}
	}
	if !rt.hasAgents {
		return []string{rt.defaultID()}
	}
	out := []string{}
	for a := range rt.agentRules {
		if rt.visible(Principal{AgentID: a}, ru) {
			out = append(out, a)
		}
	}
	if def := rt.defaultID(); !rt.hasAgent(def) && rt.visible(Principal{AgentID: def}, ru) {
		out = append(out, def)
	}
	return out
}

func (rt *Router) hasAgent(id string) bool {
	_, ok := rt.agentRules[id]
	return ok
}

func (rt *Router) defaultID() string {
	if rt.defaultAgent != "" {
		return rt.defaultAgent
	}
	return "default"
}

func sortAudience(seen map[string]AudienceEntry) []AudienceEntry {
	if len(seen) == 0 {
		return nil
	}
	out := make([]AudienceEntry, 0, len(seen))
	for _, e := range seen {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Agent < out[j].Agent })
	return out
}

// AliasForTarget returns the alias this principal knows a canonical target
// ("channel:123" / "dm:456") by, or "" when it has no rule for it. Used to
// label polled messages with the name the caller would send to.
func (rt *Router) AliasForTarget(p Principal, targets ...string) string {
	for _, t := range targets {
		if t == "" {
			continue
		}
		ru, ok := rt.byTarget[t]
		if !ok {
			kind, _, err := config.NormalizeTargetID(t)
			if err != nil {
				continue
			}
			ru, ok = rt.wild[kind]
		}
		if ok && !ru.Wildcard && rt.grantFor(p.AgentID, ru).Read && rt.visible(p, ru) {
			return ru.Alias
		}
	}
	return ""
}

// SendTarget resolves a target reference for a send, applying ACL. It returns
// (rule, "") on success or (nil, reason) with a machine-readable reason.
//
// The returned rule is a COPY carrying this agent's effective read/write, and
// the concrete channel id when the reference named one: sending through the
// "dm:*" rule must not aim at a channel called "*".
func (rt *Router) SendTarget(p Principal, ref string) (*Rule, string) {
	if strings.TrimSpace(ref) == "" {
		return nil, "empty_target"
	}
	ru, ok := rt.Resolve(ref)
	if !ok {
		if rt.HasRouting() {
			return nil, "unknown_target"
		}
		return nil, "not_allowlisted"
	}
	if !rt.visible(p, ru) {
		return nil, "foreign_agent"
	}
	if ru.Kind == KindMe {
		return ru, ""
	}
	if !rt.grantFor(p.AgentID, ru).Write {
		return nil, "write_denied"
	}
	kind, id, err := config.NormalizeTargetID(ref)
	if ru.Wildcard && (err != nil || kind != ru.Kind || id == config.WildcardID) {
		// "to":"dms" on a wildcard rule is a rule name, not an address.
		return nil, "wildcard_target"
	}
	if rt.denied(ru.Kind == KindDM, ru.ID, nil, ru) {
		// The deny list is absolute: it excludes a target from writing too, not
		// only from reading.
		return nil, "denied_channel"
	}
	return rt.concrete(p, ru, ref), ""
}

// visible enforces agent scoping once routing.json declares agents.
func (rt *Router) visible(p Principal, ru *Rule) bool {
	if ru.Kind == KindMe {
		return true
	}
	if ru.Agent != "" && p.AgentID != "" && ru.Agent != p.AgentID {
		return false
	}
	if !rt.hasAgents {
		return true
	}
	set := rt.agentRules[p.AgentID]
	if set == nil {
		// Unknown agent: allow only when nothing is scoped to anyone else.
		return false
	}
	_, ok := set[ru.Target]
	return ok
}

// Targets lists everything this principal may see, in declaration order, with
// THIS AGENT's effective read/write (the target's defaults plus its override),
// so what status and --selftest show is what the agent can actually do.
func (rt *Router) Targets(p Principal) []*Rule {
	out := make([]*Rule, 0, len(rt.ordered))
	for _, ru := range rt.ordered {
		if !rt.visible(p, ru) {
			continue
		}
		g := rt.grantFor(p.AgentID, ru)
		if g.Read == ru.Read && g.Write == ru.Write {
			out = append(out, ru)
			continue
		}
		c := *ru
		c.Read, c.Write = g.Read, g.Write
		out = append(out, &c)
	}
	return out
}

// Summary is the token-free routing view for discord_status.
func (rt *Router) Summary(p Principal) map[string]any {
	targets := rt.Targets(p)
	list := make([]map[string]any, 0, len(targets))
	for _, ru := range targets {
		list = append(list, map[string]any{
			"alias": ru.Alias, "target": ru.Target,
			"read": ru.Read, "write": ru.Write,
			"name": ru.Name, "agent": ru.Agent, "source": ru.Source,
		})
	}
	agents := make([]string, 0, len(rt.agentRules))
	for a := range rt.agentRules {
		agents = append(agents, a)
	}
	sort.Strings(agents)
	return map[string]any{
		"routing_file":    rt.HasRouting(),
		"scoped":          rt.hasAgents,
		"agent":           p.AgentID,
		"known_agents":    agents,
		"targets":         list,
		"unknown_channel": rt.unknownChan,
		"default_agent":   rt.defaultAgent,
	}
}

// kindOf names the target kind an inbound message belongs to.
func kindOf(isDM bool) string {
	if isDM {
		return KindDM
	}
	return KindChannel
}

// grantFor answers what this agent may actually do on a rule: the override the
// routing file gave it, otherwise the rule's own defaults. An agent that holds
// no grant still gets the defaults here -- whether it sees the target at all is
// visible()'s question, asked separately.
func (rt *Router) grantFor(agentID string, ru *Rule) Grant {
	if g, ok := rt.agentGrants[agentID][ru.Target]; ok {
		return g
	}
	return Grant{Read: ru.Read, Write: ru.Write}
}

// MayRead is the public form of the effective read bit for one agent: the
// target's default with that agent's own override applied.
func (rt *Router) MayRead(p Principal, ru *Rule) bool { return rt.grantFor(p.AgentID, ru).Read }

// MayWrite is MayRead's other half, and what discord_send consults.
func (rt *Router) MayWrite(p Principal, ru *Rule) bool { return rt.grantFor(p.AgentID, ru).Write }

// concrete returns a copy of ru with this agent's effective read/write and, when
// the caller's reference named a concrete channel of the same kind, that id.
func (rt *Router) concrete(p Principal, ru *Rule, ref string) *Rule {
	out := *ru
	g := rt.grantFor(p.AgentID, ru)
	out.Read, out.Write = g.Read, g.Write
	if kind, id, err := config.NormalizeTargetID(ref); err == nil && kind == ru.Kind && id != ru.ID {
		out.ID = id
		out.Target = config.CanonicalTarget(kind, id)
	}
	return &out
}

// gateKeys lists every form an allow/deny entry could use to name this message:
// the raw channel id, its canonical target, the kind's wildcard, and for a DM
// also the target of everyone on the other side (that is the id a caller would
// write). The router does not know the bot's own id, so all recipients are
// tried rather than guessing which one is the peer.
func (rt *Router) gateKeys(isDM bool, channelID string, recipients []string) []string {
	kind := kindOf(isDM)
	keys := []string{
		channelID,
		config.CanonicalTarget(kind, channelID),
		config.CanonicalTarget(kind, config.WildcardID),
	}
	if isDM {
		for _, r := range recipients {
			if r != "" {
				keys = append(keys, config.CanonicalTarget(KindDM, r))
			}
		}
	}
	return keys
}

// denied is the absolute gate: a listed channel is not even read. Aliases and
// target ids both answer, which is why the matched rules are passed in.
func (rt *Router) denied(isDM bool, channelID string, recipients []string, rules ...*Rule) bool {
	if len(rt.denyChannels) == 0 {
		return false
	}
	for _, k := range rt.gateKeys(isDM, channelID, recipients) {
		if rt.denyChannels[k] {
			return true
		}
	}
	for _, ru := range rules {
		if ru == nil {
			continue
		}
		if rt.denyChannels[strings.ToLower(ru.Alias)] || rt.denyChannels[ru.Target] {
			return true
		}
	}
	return false
}

// admitted is the other half of the gate: does an allow-list open this channel?
// Only meaningful when no rule claimed the message.
func (rt *Router) admitted(isDM bool, channelID string, recipients []string) bool {
	for _, k := range rt.gateKeys(isDM, channelID, recipients) {
		if rt.allowChannels[k] {
			return true
		}
	}
	return false
}

func normalizeName(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	s = strings.TrimPrefix(s, "#")
	s = strings.TrimPrefix(s, "@")
	return s
}

func toSet(list []string) map[string]bool {
	m := make(map[string]bool, len(list))
	for _, s := range list {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		m[strings.ToLower(s)] = true
		m[s] = true
	}
	return m
}
