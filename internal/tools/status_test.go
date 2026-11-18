package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	mcp "mcptransport"

	"mcp-discord-roleplay/internal/discord"
	"mcp-discord-roleplay/internal/router"
)

// These tests pin the SHAPE of the status answer, because the tool's entire value
// is that it is cheap enough to call before an exchange. Static facts cost the
// same on every call and say the same thing every time, so they belong to the
// description or the panel; a field that survives here is a field someone depends
// on. The retired ones stay asserted as absent, since "we stopped paying for it"
// is exactly the kind of regression that creeps back in unnoticed.

func statusCall(t *testing.T, d *Deps, ctx context.Context) *mcp.ToolResult {
	t.Helper()
	res, err := d.handleStatus(ctx, &mcp.Call{Name: "status"})
	if err != nil {
		t.Fatalf("handleStatus must answer through the result, not the protocol: %v", err)
	}
	if res.IsError {
		t.Fatalf("status reports state, it does not fail: %s", res.Content[0].Text)
	}
	if len(res.Content) == 0 || res.Content[0].Type != "text" {
		t.Fatalf("answer has no text part: %+v", res)
	}
	return res
}

func statusOK(t *testing.T, d *Deps) map[string]any {
	t.Helper()
	return decode(t, statusCall(t, d, context.Background()))
}

// readyGateway makes the fixture's never-started client look like a gateway that
// is connected and has said READY, so a healthy answer can be tested as one. The
// client itself cannot be poked from here: those fields are written by the
// websocket handlers, and Deps.snapshot exists for exactly this.
func readyGateway(d *Deps) {
	d.up = func() bool { return true }
	d.snapshot = func() discord.Snapshot {
		return discord.Snapshot{
			Started: true, Connected: true, Ready: true,
			SelfID: "1571829640467583016", SelfName: "Whale", SelfTag: "Whale#0001",
		}
	}
}

// The contract is a closed set. Everything else this process knows — limits,
// routing, uptime, the bot's tag, the media policy — stays in the panel.
func TestStatusWireKeysAreExactlyThese(t *testing.T) {
	d := newDeps(t, routingCfg(t))
	readyGateway(d)

	text := statusCall(t, d, context.Background()).Content[0].Text
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("status answer is not JSON (%q): %v", text, err)
	}
	allowed := []string{"gateway", "queue", "dropped", "last_error", "targets", "read_only"}
	for k := range out {
		found := false
		for _, a := range allowed {
			if a == k {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("unexpected key %q on the wire: %s", k, text)
		}
	}
	for _, k := range []string{"gateway", "queue"} {
		if _, ok := out[k]; !ok {
			t.Errorf("%s must always be present: %s", k, text)
		}
	}
	// Retired: advice that stood in for a number (next_action, warnings, hints), a
	// duplicate of the tool result's own isError (ok), settings reported as if they
	// were news (dry_run, breaker_open, send_budget_left, uptime_s, file_roots), and
	// the booleans one word now covers (connected, ready).
	for _, dead := range []string{"ok", "next_action", "warnings", "hints", "connected",
		"ready", "dry_run", "breaker_open", "send_budget_left", "uptime_s", "file_roots"} {
		if _, ok := out[dead]; ok {
			t.Errorf("%q came back: status is a state table, not a report (%s)", dead, text)
		}
	}
}

// Healthy and idle is the answer this tool produces most often, so its SIZE is the
// number that matters. 200 bytes is a generous ceiling (the real answer is a
// little over a hundred); the old panel cost 2658.
func TestStatusHealthyAnswerIsSmall(t *testing.T) {
	d := newDeps(t, routingCfg(t))
	readyGateway(d)

	text := statusCall(t, d, context.Background()).Content[0].Text
	if len(text) > 200 {
		t.Fatalf("healthy status costs %d bytes, want <= 200: %s", len(text), text)
	}
}

// gateway is one word standing in for started / connected / ready / stopping. Only
// "ready" means a send is worth trying; a client that was never started and a
// socket that died have to read the same way to a caller that cannot do anything
// about the difference.
func TestStatusGatewayStateMachine(t *testing.T) {
	cases := []struct {
		name     string
		snap     discord.Snapshot
		stopping bool
		want     string
	}{
		{"selftest, never started", discord.Snapshot{}, false, "down"},
		{"socket died", discord.Snapshot{Started: true}, false, "down"},
		{"connected, READY late", discord.Snapshot{Started: true, Connected: true}, false, "starting"},
		{"healthy", discord.Snapshot{Started: true, Connected: true, Ready: true}, false, "ready"},
		{"stopping wins over ready", discord.Snapshot{Started: true, Connected: true, Ready: true}, true, "stopping"},
		{"stopping while down", discord.Snapshot{Started: true}, true, "stopping"},
	}
	for _, c := range cases {
		if got := gatewayState(c.snap, c.stopping); got != c.want {
			t.Errorf("%s: gatewayState = %q, want %q", c.name, got, c.want)
		}
	}
}

// queue is a ratio rather than a count, so "almost full" is visible without
// knowing --bus-capacity, and no sentence has to tell the caller to poll.
func TestStatusQueueCellTracksUnread(t *testing.T) {
	d := newDeps(t, routingCfg(t))
	readyGateway(d)
	push(t, d, "default", evMsg("m1", "a"), evMsg("m2", "b"))

	out := statusOK(t, d)
	q, _ := out["queue"].(string)
	if !strings.HasPrefix(q, "2/") {
		t.Fatalf("queue = %q, want the unread count over the capacity", q)
	}
	if strings.HasSuffix(q, "/0") {
		t.Fatalf("queue = %q: a zero denominator makes the ratio meaningless", q)
	}
	if _, ok := out["dropped"]; ok {
		t.Errorf("dropped must stay off the wire while it is zero: %v", out)
	}
}

// dropped is a counter, not a complaint: it appears only once the ring has
// actually overwritten something.
func TestStatusDroppedAppearsOnlyWhenNonZero(t *testing.T) {
	d := newDeps(t, routingCfg(t, "-bus-capacity=10"))
	readyGateway(d)
	for i := 0; i < 12; i++ {
		push(t, d, "default", evMsg(fmt.Sprintf("m%d", i), "x"))
	}

	out := statusOK(t, d)
	if got, _ := out["dropped"].(float64); got < 1 {
		t.Fatalf("dropped = %v after overflowing a 10-slot queue: %v", out["dropped"], out)
	}
	if q, _ := out["queue"].(string); !strings.HasPrefix(q, "10/") {
		t.Errorf("queue = %q, want it pinned at the cap", q)
	}
}

// targets and read_only are the two lists the caller can actually type into
// to:"...". The alias of a wildcard rule ("dms" for dm:*) is a POLICY, not an
// address — send refuses it — so it must not be offered as somewhere to speak.
func TestStatusTargetsAndReadOnly(t *testing.T) {
	d := newDeps(t, routingCfg(t))
	readyGateway(d)

	out := statusOK(t, d)
	if got, _ := out["targets"].([]any); len(got) != 2 || got[0] != "boss" || got[1] != "ami" {
		t.Errorf("targets = %v, want boss and ami (the two this agent may write to)", out["targets"])
	}
	if got, _ := out["read_only"].([]any); len(got) != 1 || got[0] != "log" {
		t.Errorf("read_only = %v, want log", out["read_only"])
	}
}

func TestStatusWildcardAliasIsNotAnAddress(t *testing.T) {
	d := newDeps(t, pollRoutingCfg(t, m2cRouting))
	readyGateway(d)

	out := statusOK(t, d)
	if got, _ := out["targets"].([]any); len(got) != 1 || got[0] != "boss" {
		t.Fatalf("targets = %v, want only boss", out["targets"])
	}
	for _, key := range []string{"targets", "read_only"} {
		if s, present := out[key]; present && strings.Contains(fmt.Sprint(s), "dms") {
			t.Errorf("%s = %v: dm:* is a policy, not somewhere to send", key, s)
		}
	}
}

// last_error is the only prose this tool ships, and only when there is something
// to say: it is what turns gateway:"down" into an action.
func TestStatusLastErrorOnlyWhenThereIsOne(t *testing.T) {
	d := newDeps(t, routingCfg(t))
	readyGateway(d)
	if _, ok := statusOK(t, d)["last_error"]; ok {
		t.Fatal("a healthy gateway has nothing to explain")
	}

	d.snapshot = func() discord.Snapshot {
		return discord.Snapshot{Started: true, LastError: "websocket: close 1006"}
	}
	out := statusOK(t, d)
	if got, _ := out["last_error"].(string); got != "websocket: close 1006" {
		t.Fatalf("last_error = %q", got)
	}
	if got, _ := out["gateway"].(string); got != "down" {
		t.Fatalf("an unconnected socket must not read as ready: %q", got)
	}
}

// A caller named in routing.json sees ONLY its own grants: what one persona may
// say is not what another may say, and the whole point of listing aliases is that
// the model does not have to guess and eat a write_denied.
func TestStatusTargetsFollowTheCaller(t *testing.T) {
	d := newDeps(t, routingCfg(t))
	readyGateway(d)

	ctx := router.WithPrincipal(context.Background(), router.Principal{AgentID: "solo"})
	res := statusCall(t, d, ctx)
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Content[0].Text), &out); err != nil {
		t.Fatal(err)
	}
	if got, _ := out["targets"].([]any); len(got) != 1 || got[0] != "boss" {
		t.Errorf("solo's targets = %v, want only boss", out["targets"])
	}
	if _, present := out["read_only"]; present {
		t.Errorf("solo holds no read-only target: %v", out["read_only"])
	}
}

// The panel did not disappear, it moved: one debug line, whole, and only when the
// operator raised the log level. That guard is what lets the cheap answer stay
// cheap without throwing anything away.
func TestStatusDebugLogCarriesThePanel(t *testing.T) {
	d := newDeps(t, routingCfg(t))
	readyGateway(d)
	buf := withDebugLog(t, d)

	statusCall(t, d, context.Background())

	s := buf.String()
	for _, want := range []string{"panel=", "connection", "routing", "bus_capacity_per_agent", "registered_tools"} {
		if !strings.Contains(s, want) {
			t.Errorf("debug panel missing %s in:\n%s", want, s)
		}
	}
	// The panel keeps the host paths and the retired advice-free detail: it is the
	// operator's view, not the model's.
	if !strings.Contains(s, "uptime_s") {
		t.Errorf("the panel should still report uptime: %s", s)
	}
}

// registeredDesc registers the build and reads back one tool's description, which
// is where the facts about THIS process now live.
func registeredDesc(t *testing.T, d *Deps, bare string) string {
	t.Helper()
	reg := mcp.NewRegistry(d.prefix())
	if err := RegisterAll(reg, d); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}
	for _, tool := range reg.List() {
		if strings.HasSuffix(tool.Name, bare) {
			return tool.Description
		}
	}
	t.Fatalf("no tool named %q in %v", bare, reg.List())
	return ""
}

// Deployment facts ride on the tool that has to live with them: a model reads a
// description once per session, but pays for an answer field on every call.
func TestDeploymentNotesFollowConfig(t *testing.T) {
	d := newDeps(t, routingCfg(t))
	if got := registeredDesc(t, d, "poll"); strings.Contains(got, "Message Content intent") {
		t.Errorf("the intent note must only appear when the intent is off: %q", got)
	}
	// toolCfg passes -dry-run, so send has to say so out loud.
	if got := registeredDesc(t, d, "send"); !strings.Contains(got, "dry-run") {
		t.Errorf("a dry-run process must say so on send: %q", got)
	}

	quiet := newDeps(t, routingCfg(t, "-no-message-content"))
	if got := registeredDesc(t, quiet, "poll"); !strings.Contains(got, "Message Content intent is off") {
		t.Errorf("with the intent off, poll has to warn that text arrives empty: %q", got)
	}
}

// StatusPretty is the --selftest view: the wire the model will get, and the panel
// behind it. It never reaches a context window, so it may be as long as it likes.
func TestStatusPrettyShowsBothViews(t *testing.T) {
	d := newDeps(t, routingCfg(t))
	readyGateway(d)

	got := StatusPretty(d)
	for _, want := range []string{"wire:", "panel:", `"gateway"`, `"queue"`, `"connection"`, "\n  \""} {
		if !strings.Contains(got, want) {
			t.Errorf("StatusPretty missing %q in:\n%s", want, got)
		}
	}
}

// The panel is operator-facing, but it must not keep a vocabulary the wire gave
// up: `ok` was retired everywhere because the tool result's own isError already
// carries that bit, and a stale key in the log is how a second truth survives.
func TestFullPanelRetiredOkToo(t *testing.T) {
	d := newDeps(t, routingCfg(t))
	if _, dead := d.fullPanel(router.Principal{AgentID: d.Cfg.AgentID})["ok"]; dead {
		t.Error("fullPanel still reports ok; the wire stopped repeating isError, so must the log")
	}
}
