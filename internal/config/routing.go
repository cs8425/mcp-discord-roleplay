package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// discordEpochMs is Discord's epoch: 2015-01-01T00:00:00Z.
const discordEpochMs = 1420070400000

// Target kinds. M2c collapsed the old two-entity model into one: a DM is just
// a channel, so the only kinds are "channel" (a guild channel) and "dm" (the
// 1:1 channel with a user, addressed by that USER's snowflake). The "user:"
// prefix is gone; "dm:" is the only spelling accepted.
const (
	TargetChannel = "channel"
	TargetDM      = "dm"
	WildcardID    = "*"
)

// Target is one routing rule in routing.json.
//
// ID uses "channel:<snowflake>", "dm:<user-snowflake>", "channel:*", "dm:*",
// or a bare snowflake (== channel). A "*" id is a wildcard: it claims every
// channel (or every DM) that no exact rule matched.
//
// Read/Write are the DEFAULTS for every agent that references this target; an
// agent may override either one in its own targets[] entry (see Ref). They
// default to true when nil so a minimal routing file works without ceremony.
type Target struct {
	Alias string `json:"alias"`
	ID    string `json:"id"`
	Agent string `json:"agent,omitempty"`
	Read  *bool  `json:"read,omitempty"`
	Write *bool  `json:"write,omitempty"`
	Name  string `json:"name,omitempty"`
}

// Allowed reports whether the pointer flag is true (unset = allowed).
func (t *Target) Allowed(p *bool) bool { return p == nil || *p }

// Ref is one entry of agents[].targets. It is written either as a bare alias
// string (inherit the target's read/write defaults) or as an object naming the
// alias plus the permissions this agent gets:
//
//	"targets": ["boss", { "alias": "dms", "write": true }]
//
// Overrides may LOOSEN as well as tighten: the usual shape is a wildcard target
// that is read-only for everyone with one agent allowed to answer, so a target
// default of write:false must not be a hard ceiling.
type Ref struct {
	Alias string
	Read  *bool
	Write *bool
}

// UnmarshalJSON accepts both spellings.
func (r *Ref) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		r.Alias = strings.TrimSpace(s)
		return nil
	}
	var obj struct {
		Alias string `json:"alias"`
		Read  *bool  `json:"read,omitempty"`
		Write *bool  `json:"write,omitempty"`
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&obj); err != nil {
		return fmt.Errorf("target reference %s must be an alias string or {alias,read,write}: %w", string(b), err)
	}
	r.Alias = strings.TrimSpace(obj.Alias)
	r.Read, r.Write = obj.Read, obj.Write
	return nil
}

// MarshalJSON keeps round-tripping cheap for tests and dumps: a plain alias
// stays a plain string.
func (r Ref) MarshalJSON() ([]byte, error) {
	if r.Read == nil && r.Write == nil {
		return json.Marshal(r.Alias)
	}
	return json.Marshal(struct {
		Alias string `json:"alias"`
		Read  *bool  `json:"read,omitempty"`
		Write *bool  `json:"write,omitempty"`
	}{r.Alias, r.Read, r.Write})
}

// Agent is one MCP client identity in routing.json. Once the file declares any
// agent, targets become scoped: an agent only sees the aliases it lists (plus
// every target whose own agent field matches).
type Agent struct {
	ID      string `json:"id"`
	Targets []Ref  `json:"targets,omitempty"`
}

// Routing is the parsed --config file.
//
// AllowChannels/DenyChannels are GLOBAL gates (they do not depend on which
// agent asks) and their entries are aliases or target ids; deny always wins,
// and an entry listed in both is reported as a config warning.
type Routing struct {
	Agents         []Agent  `json:"agents,omitempty"`
	Targets        []Target `json:"targets,omitempty"`
	AllowChannels  []string `json:"allow_channels,omitempty"`
	DenyChannels   []string `json:"deny_channels,omitempty"`
	UnknownChannel string   `json:"unknown_channel,omitempty"`
}

// Empty reports whether the routing file carries any information.
func (r *Routing) Empty() bool {
	return r == nil || (len(r.Agents) == 0 && len(r.Targets) == 0 &&
		len(r.AllowChannels) == 0 && len(r.DenyChannels) == 0 && r.UnknownChannel == "")
}

// renamedKeys is the M2c vocabulary table: a file still using an old key gets
// told what it became instead of a bare "unknown field".
var renamedKeys = map[string]string{
	"unknown_dm": `"unknown_channel" (M2c: DMs are channels now, so there is one policy for all of them)`,
}

// LoadRouting reads and validates a routing.json file. Unknown keys are an
// error on purpose: a typo in "write": flase would otherwise silently grant
// access.
func LoadRouting(path string) (*Routing, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	for old, now := range renamedKeys {
		if _, ok := raw[old]; ok {
			return nil, fmt.Errorf("parse %s: %q is gone; use %s", path, old, now)
		}
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	var r Routing
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := r.validate(); err != nil {
		return nil, fmt.Errorf("validate %s: %w", path, err)
	}
	return &r, nil
}

func (r *Routing) validate() error {
	if r.UnknownChannel != "" {
		switch strings.ToLower(r.UnknownChannel) {
		case "ignore", "ring":
		default:
			return fmt.Errorf("unknown_channel must be ignore|ring, got %q", r.UnknownChannel)
		}
	}
	seenAlias := map[string]bool{}
	seenTarget := map[string]bool{}
	for i := range r.Targets {
		t := &r.Targets[i]
		if strings.TrimSpace(t.Alias) == "" {
			return fmt.Errorf("targets[%d]: alias is required", i)
		}
		if seenAlias[t.Alias] {
			return fmt.Errorf("duplicate alias %q", t.Alias)
		}
		seenAlias[t.Alias] = true
		kind, id, err := NormalizeTargetID(t.ID)
		if err != nil {
			return fmt.Errorf("targets[%d] (%s): %w", i, t.Alias, err)
		}
		canonical := CanonicalTarget(kind, id)
		if seenTarget[canonical] {
			return fmt.Errorf("duplicate target id %s (alias %s)", canonical, t.Alias)
		}
		seenTarget[canonical] = true
	}
	seenAgent := map[string]bool{}
	for i := range r.Agents {
		a := &r.Agents[i]
		if strings.TrimSpace(a.ID) == "" {
			return fmt.Errorf("agents[%d]: id is required", i)
		}
		if seenAgent[a.ID] {
			return fmt.Errorf("duplicate agent id %q", a.ID)
		}
		seenAgent[a.ID] = true
		for j, ref := range a.Targets {
			if ref.Alias == "" {
				return fmt.Errorf("agents[%d] (%s): targets[%d] is empty", i, a.ID, j)
			}
			// Every agent target must resolve to a declared alias.
			if !seenAlias[ref.Alias] {
				return fmt.Errorf("agent %q references unknown target alias %q", a.ID, ref.Alias)
			}
		}
	}
	// Global lists: alias or target id, never free text -- a typo here would
	// silently leave a channel open (allow) or shut (deny).
	for _, which := range []struct {
		name string
		list []string
	}{
		{"allow_channels", r.AllowChannels},
		{"deny_channels", r.DenyChannels},
	} {
		for i, s := range which.list {
			s = strings.TrimSpace(s)
			if s == "" {
				return fmt.Errorf("%s[%d]: empty entry", which.name, i)
			}
			if !seenAlias[s] {
				if _, _, err := NormalizeTargetID(s); err != nil {
					return fmt.Errorf("%s[%d] %q is neither a declared alias nor a target id", which.name, i, s)
				}
			}
		}
	}
	return nil
}

// CanonicalTarget joins a kind and an id into the form used everywhere else
// ("channel:123", "dm:456", "dm:*").
func CanonicalTarget(kind, id string) string { return kind + ":" + id }

// IsWildcard reports whether a normalized id is the "*" catch-all.
func IsWildcard(id string) bool { return id == WildcardID }

// NormalizeTargetID turns a user-supplied target id into a canonical
// (kind, id) pair where kind is "channel" or "dm". Accepted spellings:
// channel:123, dm:123, channel:*, dm:*, or a bare snowflake (== channel).
//
// The id after "dm:" is the OTHER user's snowflake, not the DM channel's: a
// user's DM channel id is not knowable before the first message, while the
// user id is what inbound events already carry.
func NormalizeTargetID(s string) (kind, id string, err error) {
	v := strings.TrimSpace(s)
	if v == "" {
		return "", "", fmt.Errorf("target id is empty")
	}
	lower := strings.ToLower(v)
	switch {
	case strings.HasPrefix(lower, "channel:"):
		kind, v = TargetChannel, v[len("channel:"):]
	case strings.HasPrefix(lower, "dm:"):
		kind, v = TargetDM, v[len("dm:"):]
	case allDigits(v):
		kind = TargetChannel
	default:
		return "", "", fmt.Errorf("target id %q must be channel:<id>, dm:<user_id>, channel:*, dm:* or a bare snowflake", s)
	}
	v = strings.TrimSpace(v)
	if v == WildcardID {
		return kind, WildcardID, nil
	}
	if !allDigits(v) || len(v) < 5 || len(v) > 25 {
		return "", "", fmt.Errorf("%s id %q is not a Discord snowflake", kind, v)
	}
	return kind, v, nil
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// SnowflakeTime converts a Discord snowflake to its creation timestamp
// (Discord epoch 2015-01-01T00:00:00Z). ok is false when id is not numeric.
func SnowflakeTime(id string) (time.Time, bool) {
	n, err := strconv.ParseUint(strings.TrimSpace(id), 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	ms := int64(n>>22) + discordEpochMs
	return time.Unix(ms/1000, (ms%1000)*int64(time.Millisecond)).UTC(), true
}
