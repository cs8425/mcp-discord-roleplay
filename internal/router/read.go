package router

import (
	"strings"

	"mcp-discord-roleplay/internal/config"
)

// ReadTarget is the read-path twin of SendTarget: identical resolution and
// visibility, but the ACL question is "may this agent READ here", and the word
// it gives back when the answer is no is its own.
//
// It is a separate method rather than a boolean on SendTarget because the two
// questions genuinely diverge in this project: a read_only target (write:false
// in routing.json, which is most of what a roleplay agent can see) is perfectly
// readable, and M2c made read and write independent grants per agent. One
// method answering "the right one" depending on a flag would also have to
// invent a single reason string for two different failures.
//
// Returns (rule, "") on success or (nil, reason) with a machine-readable
// reason. The reasons are the ones SendTarget already produces, except
// read_denied; the deny list stays absolute -- a channel we refuse to admit is
// not something we list a transcript of either.
//
// As in SendTarget, the returned rule is a COPY carrying this agent's effective
// read/write and the concrete channel id when the reference named one.
func (rt *Router) ReadTarget(p Principal, ref string) (*Rule, string) {
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
	if !rt.grantFor(p.AgentID, ru).Read {
		return nil, "read_denied"
	}
	kind, id, err := config.NormalizeTargetID(ref)
	if ru.Wildcard && (err != nil || kind != ru.Kind || id == config.WildcardID) {
		// "dms" on a wildcard rule is a rule name, not an address: there is no
		// channel to read.
		return nil, "wildcard_target"
	}
	if rt.denied(ru.Kind == KindDM, ru.ID, nil, ru) {
		return nil, "denied_channel"
	}
	return rt.concrete(p, ru, ref), ""
}
