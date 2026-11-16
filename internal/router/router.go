// Package router maps the short names an agent speaks ("boss", "#rp-main",
// "user:333") onto Discord channel/user IDs, and enforces which of them a
// given principal may read or write.
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

// Rule kinds.
const (
	KindChannel = "channel"
	KindUser    = "user"
	KindMe      = "me" // the bot's own note-to-self DM, resolved lazily
)

// Rule is one routing entry (or an ad-hoc rule derived from the allow-lists).
type Rule struct {
	Alias  string `json:"alias"`
	Kind   string `json:"kind"`
	ID     string `json:"id,omitempty"`
	Target string `json:"target"`
	Name   string `json:"name,omitempty"`
	Agent  string `json:"agent,omitempty"`
	Read   bool   `json:"read"`
	Write  bool   `json:"write"`
	Source string `json:"source"` // routing | ad-hoc | self
}

// Router answers lookup and ACL questions.
type Router struct {
	ordered  []*Rule
	byAlias  map[string]*Rule // lowercased alias
	byTarget map[string]*Rule // canonical "channel:123"
	byChan   map[string]*Rule // channel id -> rule
	byUser   map[string]*Rule // recipient user id -> rule
	byName   map[string]*Rule // lowercased channel/user name -> rule

	agentRules map[string]map[string]*Rule // agentID -> target -> rule
	hasAgents  bool

	allowGuilds   map[string]bool
	allowChannels map[string]bool
	denyChannels  map[string]bool
	allowDM       bool
	unknownDM     string
	defaultAgent  string
}

// New builds a Router from the resolved configuration (routing.json plus the
// command-line allow/deny lists).
func New(cfg *config.Config) (*Router, error) {
	rt := &Router{
		byAlias:       map[string]*Rule{},
		byTarget:      map[string]*Rule{},
		byChan:        map[string]*Rule{},
		byUser:        map[string]*Rule{},
		byName:        map[string]*Rule{},
		agentRules:    map[string]map[string]*Rule{},
		allowGuilds:   toSet(cfg.AllowGuilds),
		allowChannels: toSet(cfg.AllowChannels),
		denyChannels:  toSet(cfg.DenyChannels),
		allowDM:       cfg.AllowDM,
		unknownDM:     cfg.UnknownDM,
		defaultAgent:  cfg.AgentID,
	}
	if rt.unknownDM == "" {
		rt.unknownDM = "ignore"
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
				Alias:  t.Alias,
				Kind:   kind,
				ID:     id,
				Target: kind + ":" + id,
				Name:   t.Name,
				Agent:  t.Agent,
				Read:   t.Allowed(t.Read),
				Write:  t.Allowed(t.Write),
				Source: "routing",
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
			for _, ref := range a.Targets {
				if ru, ok := rt.byAlias[strings.ToLower(ref)]; ok {
					set[ru.Target] = ru
				}
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
			}
			set[ru.Target] = ru
		}
	}
	return rt, nil
}

func (rt *Router) add(ru *Rule) {
	low := strings.ToLower(ru.Alias)
	if _, dup := rt.byAlias[low]; dup {
		return // config validation already rejects duplicates
	}
	rt.ordered = append(rt.ordered, ru)
	rt.byAlias[low] = ru
	rt.byTarget[ru.Target] = ru
	switch ru.Kind {
	case KindChannel:
		rt.byChan[ru.ID] = ru
	case KindUser:
		rt.byUser[ru.ID] = ru
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
// Accepted forms: alias, channel:<id>, user:<id>, dm:<id>, #name, @name,
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
		target := kind + ":" + id
		if ru, ok := rt.byTarget[target]; ok {
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

// adhoc builds an unlisted rule governed only by the allow/deny lists.
func (rt *Router) adhoc(kind, id string) (*Rule, bool) {
	target := kind + ":" + id
	if kind == KindUser && !rt.allowDM {
		return nil, false
	}
	if rt.denyChannels[target] || rt.denyChannels[id] {
		return nil, false
	}
	if kind == KindChannel && len(rt.allowChannels) > 0 &&
		!rt.allowChannels[target] && !rt.allowChannels[id] {
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
// tries the channel id, then any recipient id (the other human).
func (rt *Router) RuleForMessage(channelID string, isDM bool, recipients []string) *Rule {
	if ru := rt.byChan[channelID]; ru != nil {
		return ru
	}
	if isDM {
		for _, u := range recipients {
			if ru := rt.byUser[u]; ru != nil {
				return ru
			}
		}
	}
	return nil
}

// Admission reasons returned by AdmitInbound. "" means "store this event".
const (
	AdmitOK          = ""
	AdmitDMDisabled  = "dm_disabled"
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
	if isDM && !rt.allowDM {
		return nil, AdmitDMDisabled
	}
	if rt.denyChannels[channelID] {
		return nil, AdmitDenied
	}
	ru := rt.RuleForMessage(channelID, isDM, recipients)
	if ru != nil {
		if !ru.Read {
			return nil, AdmitReadDenied
		}
		return ru, AdmitOK
	}
	if isDM {
		if rt.unknownDM == "ring" {
			return nil, AdmitOK // stored under its raw channel id
		}
		return nil, AdmitNoRuleIgnor
	}
	// guild channel with no rule: fall back to the allow-lists
	if len(rt.allowChannels) > 0 && !rt.allowChannels[channelID] {
		return nil, AdmitNotListed
	}
	if len(rt.allowGuilds) > 0 && !rt.allowGuilds[guildID] {
		return nil, AdmitNotListed
	}
	return nil, AdmitOK
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
// It walks ALL rules rather than the byChan/byUser single-value indexes, and
// that is the whole point: those indexes keep one rule per id (last declaration
// wins), so a channel listed twice under two aliases for two agents would only
// ever reach one of them.
//
// Events that no rule owns but policy admits (a guild channel passing the
// allow-lists, a stranger DM with unknown_dm="ring") go to the DEFAULT agent
// only. They are never broadcast: an unknown DM is nobody's business until an
// agent claims that user.
//
// An empty result means "nobody may read this", and the event is dropped.
func (rt *Router) Audience(guildID, channelID string, isDM bool, recipients []string) []AudienceEntry {
	if isDM && !rt.allowDM {
		return nil
	}
	if rt.denyChannels[channelID] {
		return nil
	}

	var matched []*Rule
	for _, ru := range rt.ordered {
		switch ru.Kind {
		case KindChannel:
			if ru.ID == channelID {
				matched = append(matched, ru)
			}
		case KindUser:
			if !isDM {
				continue
			}
			for _, u := range recipients {
				if u != "" && ru.ID == u {
					matched = append(matched, ru)
					break
				}
			}
		}
	}

	seen := map[string]AudienceEntry{}
	for _, ru := range matched {
		if !ru.Read {
			continue
		}
		for _, a := range rt.agentsSeeing(ru) {
			seen[a] = AudienceEntry{Agent: a, Alias: ru.Alias, Name: ru.Name, Target: ru.Target}
		}
	}
	if len(matched) > 0 {
		// A rule owns this channel/user: read_denied and foreign_agent are final,
		// they must not fall through to the stray handler below.
		return sortAudience(seen)
	}

	if !rt.admitStray(guildID, channelID, isDM) {
		return nil
	}
	def := rt.defaultID()
	seen[def] = AudienceEntry{Agent: def, Target: "channel:" + channelID}
	return sortAudience(seen)
}

// admitStray applies the no-rule branch of the inbound policy.
func (rt *Router) admitStray(guildID, channelID string, isDM bool) bool {
	if isDM {
		return rt.unknownDM == "ring"
	}
	if len(rt.allowChannels) > 0 && !rt.allowChannels[channelID] {
		return false
	}
	if len(rt.allowGuilds) > 0 && !rt.allowGuilds[guildID] {
		return false
	}
	return true
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
// ("channel:123" / "user:456") by, or "" when it has no rule for it. Used to
// label polled messages with the name the caller would send to.
func (rt *Router) AliasForTarget(p Principal, targets ...string) string {
	for _, t := range targets {
		if t == "" {
			continue
		}
		if ru, ok := rt.byTarget[t]; ok && ru.Read && rt.visible(p, ru) {
			return ru.Alias
		}
	}
	return ""
}

// SendTarget resolves a target reference for a send, applying ACL. It returns
// (rule, "") on success or (nil, reason) with a machine-readable reason.
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
	if !ru.Write {
		return nil, "write_denied"
	}
	return ru, ""
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

// Targets lists everything this principal may see, in declaration order.
func (rt *Router) Targets(p Principal) []*Rule {
	out := make([]*Rule, 0, len(rt.ordered))
	for _, ru := range rt.ordered {
		if rt.visible(p, ru) {
			out = append(out, ru)
		}
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
		"routing_file":  rt.HasRouting(),
		"scoped":        rt.hasAgents,
		"agent":         p.AgentID,
		"known_agents":  agents,
		"targets":       list,
		"unknown_dm":    rt.unknownDM,
		"allow_dm":      rt.allowDM,
		"default_agent": rt.defaultAgent,
	}
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
