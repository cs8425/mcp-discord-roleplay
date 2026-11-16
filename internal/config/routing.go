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

// Target is one routing rule in routing.json. ID uses the canonical
// "channel:<snowflake>" / "user:<snowflake>" form. Read/Write default to true
// when nil so that a minimal routing file works without ceremony.
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

// Agent is one MCP client identity in routing.json. Once the file declares any
// agent, targets become scoped: an agent only sees the aliases it lists (plus
// every target whose own agent field matches).
type Agent struct {
	ID      string   `json:"id"`
	Targets []string `json:"targets,omitempty"`
}

// Routing is the parsed --config file.
type Routing struct {
	Agents    []Agent  `json:"agents,omitempty"`
	Targets   []Target `json:"targets,omitempty"`
	UnknownDM string   `json:"unknown_dm,omitempty"`
}

// Empty reports whether the routing file carries any information.
func (r *Routing) Empty() bool {
	return r == nil || (len(r.Agents) == 0 && len(r.Targets) == 0 && r.UnknownDM == "")
}

// LoadRouting reads and validates a routing.json file. Unknown keys are an
// error on purpose: a typo in "write": flase would otherwise silently grant
// access.
func LoadRouting(path string) (*Routing, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
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
	if r.UnknownDM != "" {
		switch strings.ToLower(r.UnknownDM) {
		case "ignore", "ring":
		default:
			return fmt.Errorf("unknown_dm must be ignore|ring, got %q", r.UnknownDM)
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
		canonical := kind + ":" + id
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
	}
	// Every agent target must resolve to a declared alias.
	for _, a := range r.Agents {
		for _, ref := range a.Targets {
			if !seenAlias[ref] {
				return fmt.Errorf("agent %q references unknown target alias %q", a.ID, ref)
			}
		}
	}
	return nil
}

// NormalizeTargetID turns a user-supplied target id into a canonical
// (kind, id) pair where kind is "channel" or "user". Accepted spellings:
// channel:123, user:123, dm:123 (== user:123), or a bare snowflake (== channel).
func NormalizeTargetID(s string) (kind, id string, err error) {
	v := strings.TrimSpace(s)
	if v == "" {
		return "", "", fmt.Errorf("target id is empty")
	}
	lower := strings.ToLower(v)
	switch {
	case strings.HasPrefix(lower, "channel:"):
		kind, v = "channel", v[len("channel:"):]
	case strings.HasPrefix(lower, "user:"):
		kind, v = "user", v[len("user:"):]
	case strings.HasPrefix(lower, "dm:"):
		kind, v = "user", v[len("dm:"):]
	case allDigits(v):
		kind = "channel"
	default:
		return "", "", fmt.Errorf("target id %q must be channel:<id>, user:<id> or a bare snowflake", s)
	}
	v = strings.TrimSpace(v)
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
func SnowflakeTime(id string) (t time.Time, ok bool) {
	n, err := strconv.ParseUint(strings.TrimSpace(id), 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	ms := int64(n>>22) + discordEpochMs
	return time.Unix(ms/1000, (ms%1000)*int64(time.Millisecond)).UTC(), true
}
