// discord_send: the only way this server puts words on Discord.
//
// Two rules shape everything below.
//
//  1. The model never sends bytes. A file is named by a path inside --file-root
//     and read here, so a 3 MB image costs a hundred tokens instead of four
//     million, and a truncated base64 blob can never reach Discord.
//  2. A link is pasted, never fetched. A files entry with "url" is appended to
//     the message text and Discord downloads and renders it. We therefore hold
//     no outbound socket for content, build no embed, and have no SSRF surface
//     to defend — the only thing we vet is that the string is one clean http(s)
//     URL, because a newline in it would smuggle extra lines into the message.
//
// Budget, per-channel gap and circuit breaker live one layer down in
// internal/discord, so no call site can skip them by accident. This layer owns
// target resolution, splitting, and turning every failure into an answer the
// model can act on: a code, a retry_after_ms, and how much already landed.
package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"

	mcp "mcptransport"

	"mcp-discord-roleplay/internal/discord"
	"mcp-discord-roleplay/internal/files"
	"mcp-discord-roleplay/internal/router"
	"mcp-discord-roleplay/internal/tz"
)

// maxSendParts caps a single call. Twenty parts is 40k characters, far past any
// plausible line of dialogue; past that the caller is dumping a document and
// should hand over a file instead of flooding the channel.
const maxSendParts = 20

func sendTool() *mcp.Tool {
	return &mcp.Tool{
		Name:  "send",
		Title: "Send a message to Discord",
		Description: "Send text and/or attachments to exactly one Discord target. " +
			"to: an alias listed by discord_status, 'channel:<id>', 'dm:<id>', '#name', '@name', a bare channel id, " +
			"'me' (a DM to the bot itself), or 'last' (the target this agent spoke to most recently). " +
			"A body longer than 2000 characters is split into several messages sent in order. " +
			"files[].path is read on the server from the --file-root directories and uploaded as a real attachment: " +
			"give it relative to that root and never base64. files[].url is NOT downloaded - the link is pasted " +
			"into the message and Discord renders it. @everyone, role and user pings are stripped by the server " +
			"and cannot be enabled. A failure returns a code; rate_limited, budget_exhausted and circuit_open " +
			"come with retry_after_ms.",
		InputSchema: mcp.Schema{
			Type:        mcp.TypeObject,
			Description: "to is required; text, files or both.",
			Properties: map[string]mcp.Schema{
				"to": {
					Type:        mcp.TypeString,
					Description: "alias from discord_status | channel:<id> | dm:<id> | #name | @name | <bare channel id> | me | last",
				},
				"text": {
					Type:        mcp.TypeString,
					Description: "message body. May be empty when files carry the message. Newlines are kept.",
				},
				"files": {
					Type:        mcp.TypeArray,
					Description: "attachments for the last part of the message; at most --max-files-per-msg",
					Items: &mcp.Schema{
						Type:        mcp.TypeObject,
						Description: "one attachment: {path} to upload a local file, or {url} to paste a link",
						Properties: map[string]mcp.Schema{
							"name": {Type: mcp.TypeString, Description: "filename Discord shows; defaults to the basename of path"},
							"path": {Type: mcp.TypeString, Description: "file inside a --file-root directory, named relative to that root: \"out/pose.png\". A leading '/' is fine and means the root itself. The name is cleaned, so \"a/../b\" is just \"b\"; a '..' that would leave the root is refused, as is '~' (we never expand it)."},
							"url":  {Type: mcp.TypeString, Description: "http(s) link to paste into the message text instead of uploading anything"},
							"mime": {Type: mcp.TypeString, Description: "content type override; detected from the name and the first bytes otherwise"},
						},
					},
				},
			},
			Required: []string{"to"},
		},
	}
}

type sendArgs struct {
	To    string      `json:"to"`
	Text  string      `json:"text"`
	Files []files.Ref `json:"files"`
}

func (d *Deps) handleSend(ctx context.Context, call *mcp.Call) (*mcp.ToolResult, error) {
	var a sendArgs
	if err := decodeArguments(call, &a); err != nil {
		return errResult("bad_arguments", err.Error(), nil), nil
	}
	p := d.principal(ctx)

	if d.DC == nil {
		return errResult(discord.CodeNotConnected,
			"this build has no discord client; the server cannot send", nil), nil
	}
	ru, reason := d.routeSend(p, a.To)
	if reason != "" {
		return errResult(reason, d.targetHint(reason), d.targetList(reason, p)), nil
	}

	// --- attachments: resolve everything before anything is sent. A bad path
	// must fail the call, not silently deliver half a scene.
	var (
		opened []*files.Opened
		urls   []string
		warn   []string
	)
	defer func() {
		for _, o := range opened {
			_ = o.Close()
		}
	}()

	if len(a.Files) > d.Cfg.MaxFilesPerMsg {
		return errResult(files.CodeTooMany,
			fmt.Sprintf("%d files in one call, but --max-files-per-msg is %d: send them in separate messages",
				len(a.Files), d.Cfg.MaxFilesPerMsg),
			map[string]any{"max_files_per_msg": d.Cfg.MaxFilesPerMsg}), nil
	}
	for i, f := range a.Files {
		idx := map[string]any{"file_index": i}
		switch {
		case f.IsURL():
			u, err := files.ValidateURL(f.URL)
			if err != nil {
				return errResult(files.CodeOf(err), err.Error(), idx), nil
			}
			urls = append(urls, u)
		case strings.TrimSpace(f.Path) != "" && strings.TrimSpace(f.URL) != "":
			// path wins, and we say so instead of quietly dropping the link.
			warn = append(warn, fmt.Sprintf(
				"files[%d] gave both path and url: the file was uploaded, the url was ignored", i))
			o, err := d.Files.OpenRef(f)
			if err != nil {
				return errResult(files.CodeOf(err), err.Error(), idx), nil
			}
			opened = append(opened, o)
		case strings.TrimSpace(f.Path) != "":
			o, err := d.Files.OpenRef(f)
			if err != nil {
				ex := idx
				if files.CodeOf(err) == files.CodeNotFound {
					ex["file_roots"] = d.Cfg.FileRoots
				}
				return errResult(files.CodeOf(err), err.Error(), ex), nil
			}
			opened = append(opened, o)
		default:
			return errResult(files.CodeEmpty,
				"files[] entries need either path (inside a --file-root directory) or url", idx), nil
		}
	}

	if strings.TrimSpace(a.Text) == "" && len(opened) == 0 && len(urls) == 0 {
		return errResult("empty_message",
			"nothing to send: give text, a file path or a url", nil), nil
	}

	// --- body: text first, then one link per line. Joined before splitting so a
	// long text plus links still respects the 2000 limit part by part.
	body := a.Text
	if len(urls) > 0 {
		body = strings.TrimRight(body, " \t\r\n")
		if body != "" {
			body += "\n\n"
		}
		body += strings.Join(urls, "\n")
	}
	parts, hardCut := splitMessage(body, discord.MaxContentRunes)
	if len(parts) > maxSendParts {
		return errResult("text_too_long",
			fmt.Sprintf("the body needs %d messages of %d characters to deliver %d characters; "+
				"shorten it, or write it to a file and send that as an attachment",
				len(parts), discord.MaxContentRunes, utf8.RuneCountInString(body)),
			map[string]any{"runes": utf8.RuneCountInString(body), "max_parts": maxSendParts}), nil
	}
	if hardCut {
		warn = append(warn, "a part had no line, sentence end or space to break at, so a word may be cut in half")
	}
	if len(parts) == 0 {
		// Blank text with attachments is a legal message: the files are the point.
		parts = []string{""}
		warn = append(warn, "text was blank: the attachments carry the message")
	}

	// --- DM targets need a channel id before anything can go out.
	chanID, se := d.channelFor(ctx, ru)
	if se != nil {
		return d.sendFailure(se, map[string]any{"target": ru.Alias}), nil
	}
	d.noteLastTarget(p.AgentID, ru.Target)

	extra := map[string]any{
		"target":     ru.Alias,
		"kind":       ru.Kind,
		"channel_id": chanID,
	}

	ids := make([]string, 0, len(parts))
	var lastRes *discord.SendResult
	for i, part := range parts {
		if i > 0 {
			// Each part is a separate request, and the caller may have hung up
			// between them; do not open one more than asked for.
			if err := ctx.Err(); err != nil {
				extra["parts_sent"] = len(ids)
				extra["message_ids"] = ids
				return errResult(discord.CodeCancelled,
					fmt.Sprintf("the caller went away after part %d; the rest was not sent", i), extra), nil
			}
		}
		req := discord.SendRequest{ChannelID: chanID, Text: part}
		if i == len(parts)-1 {
			req.Files = opened // attachments ride on the last part only
		}
		res, err := d.DC.Send(ctx, req)
		if err != nil {
			se := asSendError(err)
			extra["parts_sent"] = len(ids)
			extra["message_ids"] = ids
			extra["part_failed"] = i + 1
			if len(ids) > 0 {
				extra["delivered_before_failure"] = len(ids)
				notePartial(se, len(ids), len(parts))
			}
			return d.sendFailure(se, extra), nil
		}
		ids = append(ids, res.MessageID)
		lastRes = res
	}

	w := sendWire{OK: true}
	if len(parts) > 1 {
		w.PartsSent = len(ids)
	}
	if len(opened) > 0 {
		w.Attachments = attachmentView(opened)
	}
	if len(urls) > 0 {
		w.URLCount = len(urls)
	}
	if d.Cfg.DryRun {
		w.DryRun = true
		warn = append(warn, "dry-run: nothing was delivered to Discord")
	}
	if len(warn) > 0 {
		w.Warnings = warn
	}
	d.logSend(p, ru, chanID, ids, lastRes)
	return jsonResult(w), nil
}

// sendWire is the successful answer, and it is deliberately almost nothing: the
// text is on Discord now, so the agent only needs the facts that could change
// what it does next — did one message go out or five, what became of the files,
// and is this run not really sending. Which alias was picked, which channel it
// landed in, and the message ids Discord handed back are for the operator, so
// they went to the log.
type sendWire struct {
	OK          bool       `json:"ok"`
	PartsSent   int        `json:"parts_sent,omitempty"`
	Attachments []sentFile `json:"attachments,omitempty"`
	URLCount    int        `json:"url_count,omitempty"`
	DryRun      bool       `json:"dry_run,omitempty"`
	Warnings    []string   `json:"warnings,omitempty"`
}

// logSend records what a send actually did: which alias resolved to which
// channel id, every message id Discord handed back, and when it stamped them.
// None of that is on the wire any more -- the ids an operator needs to read a
// transcript against are here, in the one line a successful send leaves behind.
func (d *Deps) logSend(p router.Principal, ru *router.Rule, chanID string, ids []string, last *discord.SendResult) {
	lg := d.debug()
	if !lg.Enabled(context.Background(), slog.LevelDebug) {
		return
	}
	fields := []any{"agent", p.AgentID, "target", ru.Alias, "kind", ru.Kind,
		"channel_id", chanID, "parts", len(ids), "message_ids", ids}
	if last != nil {
		fields = append(fields, "delivered", last.ChannelID, "created_at", tz.Format(last.CreatedAt))
	}
	lg.Debug("send", fields...)
}

// ---------------------------------------------------------------------------
// target resolution
// ---------------------------------------------------------------------------

// routeSend turns the caller's "to" into a rule, applying the same ACL a named
// target gets. "last" is expanded here and then re-checked, so a remembered
// pointer can never outlive a permission change.
func (d *Deps) routeSend(p router.Principal, ref string) (*router.Rule, string) {
	r := strings.TrimSpace(ref)
	if r == "" {
		return nil, "empty_target"
	}
	if strings.EqualFold(r, "last") {
		last := d.lastTarget(p.AgentID)
		if last == "" {
			return nil, "no_last_target"
		}
		r = last
	}
	return d.Router.SendTarget(p, r)
}

// channelFor gives the REST layer a channel id: direct for a channel, created on
// demand for a DM peer or for "me".
func (d *Deps) channelFor(ctx context.Context, ru *router.Rule) (string, *discord.SendError) {
	switch ru.Kind {
	case router.KindChannel:
		if strings.TrimSpace(ru.ID) == "" {
			return "", &discord.SendError{Code: discord.CodeBadRequest,
				Msg: "the routing entry for " + ru.Alias + " has no channel id"}
		}
		return ru.ID, nil
	case router.KindDM:
		id, err := d.DC.DMChannel(ctx, ru.ID)
		if err != nil {
			return "", asSendError(err)
		}
		return id, nil
	case router.KindMe:
		self := d.DC.SelfID()
		if self == "" {
			return "", &discord.SendError{Code: discord.CodeNotConnected,
				Msg: "'me' needs the bot's own id and READY has not arrived yet: call discord_status again in a second"}
		}
		id, err := d.DC.DMChannel(ctx, self)
		if err != nil {
			return "", asSendError(err)
		}
		return id, nil
	default:
		return "", &discord.SendError{Code: "unknown_target",
			Msg: "unsupported target kind " + ru.Kind}
	}
}

// targetHint makes each rejection reason actionable for the model.
func (d *Deps) targetHint(reason string) string {
	switch reason {
	case "empty_target":
		return "to is required: an alias from discord_status, channel:<id>, dm:<id>, #name, me or last"
	case "no_last_target":
		return "this agent has not sent anything yet, so 'last' has nothing to point at; name a target"
	case "unknown_target":
		return "that name is not in routing.json; use one of the aliases listed in this result's targets"
	case "not_allowlisted":
		return "not covered by --allow-channels/--allow-guilds, or it is in --deny-channels; the operator decides"
	case "denied_channel":
		return "that channel is in the deny list, which is absolute: nobody may read or write it"
	case "wildcard_target":
		return "that alias is a wildcard rule (dm:* or channel:*), which is a policy, not an address: name the concrete peer, e.g. dm:<user_id>"
	case "foreign_agent":
		return "that target belongs to a different agent; run this server with --agent for the right persona"
	case "write_denied":
		return "you may not speak here: this target is read-only for you (its write default, or your agent's override); the operator decides in routing.json"
	default:
		return "cannot resolve that target"
	}
}

// targetList is the recovery data: for the two "you named the wrong thing"
// reasons, what the model could have named instead. Bounded, because a big
// channel list in an error answer is pure token waste.
func (d *Deps) targetList(reason string, p router.Principal) map[string]any {
	if reason != "unknown_target" && reason != "no_last_target" && reason != "empty_target" && reason != "wildcard_target" {
		return nil
	}
	names := d.writableTargets(p, 15)
	if len(names) == 0 {
		return nil
	}
	return map[string]any{"targets": names}
}

func (d *Deps) writableTargets(p router.Principal, limit int) []string {
	out := make([]string, 0, limit)
	for _, ru := range d.Router.Targets(p) {
		if !ru.Write || ru.Wildcard {
			// A wildcard alias is not sendable as-is (it has no address), so
			// suggesting it would only set the caller up for wildcard_target.
			continue
		}
		out = append(out, ru.Alias)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// last target
// ---------------------------------------------------------------------------

// noteLastTarget remembers where this agent just spoke, for to:"last" and (in
// M2) for the poll that answers it. Keyed by agent so two personas on one
// server never inherit each other's conversation.
func (d *Deps) noteLastTarget(agentID, target string) {
	if agentID == "" {
		agentID = "<default>"
	}
	d.last.Store(agentID, target)
}

func (d *Deps) lastTarget(agentID string) string {
	if agentID == "" {
		agentID = "<default>"
	}
	v, ok := d.last.Load(agentID)
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

// ---------------------------------------------------------------------------
// failures
// ---------------------------------------------------------------------------

func asSendError(err error) *discord.SendError {
	var se *discord.SendError
	if errors.As(err, &se) && se != nil {
		return se
	}
	return &discord.SendError{Code: discord.CodeAPIError, Msg: truncateErr(err)}
}

// notePartial rewrites a mid-split failure so the answer says how much of the
// message is already on Discord. Without it the honest-looking move for the
// model is to resend everything, which duplicates part 1 in front of everyone.
func notePartial(se *discord.SendError, sent, total int) {
	if se == nil || sent == 0 {
		return
	}
	se.Msg = fmt.Sprintf("%d of %d part(s) landed before the failure: %s. "+
		"Do not resend the whole message; continue from part %d.",
		sent, total, se.Msg, sent+1)
}

func truncateErr(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

// sendFailure turns a SendError into the tool answer. The code is the branch
// point, the hint is what to do next, and the structured fields carry what the
// operator needs to read a log line against.
func (d *Deps) sendFailure(se *discord.SendError, extra map[string]any) *mcp.ToolResult {
	m := map[string]any{}
	if se.RetryAfterMS > 0 {
		m["retry_after_ms"] = se.RetryAfterMS
	}
	if se.Status != 0 {
		m["http_status"] = se.Status
	}
	if se.DiscordCode != "" {
		m["discord_code"] = se.DiscordCode
	}
	if se.BreakerOpen {
		m["breaker_open"] = true
	}
	for k, v := range extra {
		m[k] = v
	}
	if d.Cfg.DryRun {
		m["dry_run"] = true
	}
	return errResult(se.Code, sendAdvice(se), m)
}

func sendAdvice(se *discord.SendError) string {
	switch se.Code {
	case discord.CodeRateLimited:
		if se.BreakerOpen {
			return "this 429 tripped the circuit breaker: sending is paused for a minute, so retrying now is useless"
		}
		return "wait retry_after_ms, then send again; if this repeats, send fewer, shorter messages"
	case discord.CodeCircuitOpen:
		return "sending is paused after repeated 429s; wait retry_after_ms (discord_status shows the countdown)"
	case discord.CodeBudgetExhausted:
		return "this process spent its per-minute send budget; wait retry_after_ms or raise --max-sends-per-min"
	case discord.CodeNotConnected:
		return "no gateway session: check discord_status (bad token, or the gateway refused us)"
	case discord.CodeUnauthorized:
		return "discord rejected the token; only the operator can fix that (rotate it in the env file)"
	case discord.CodeForbidden:
		return "the bot may not speak here, or this user does not accept DMs from bots"
	case discord.CodeUnknownChannel:
		return "no such channel or guild: re-check the id, or pick an alias from discord_status"
	case discord.CodeCannotSend:
		return "the channel exists but we cannot write to it (deleted, or a thread that is locked)"
	case discord.CodeBadRequest:
		return "discord rejected the payload: content too long, or an attachment it did not accept"
	case discord.CodeServerError, discord.CodeNetwork:
		return "discord or the network hiccuped; sending the same call once more is safe"
	case discord.CodeCancelled:
		return "the call was cancelled before the message left"
	case discord.CodeDMFailed:
		return "the DM channel could not be opened: this user may not allow DMs from bots"
	case discord.CodeFileFailed:
		return "an attachment could not be read at upload time; the file may have changed underneath us"
	default:
		if se.Msg != "" {
			return se.Msg
		}
		return "the send failed; check discord_status"
	}
}

// sentFile is the wire view of one upload that went out. The agent named the
// file, so what it needs back is confirmation of which one rode along and how
// big it was when we read it; the MIME type we sniffed is an upload detail, not
// a fact the model acts on.
type sentFile struct {
	Name string `json:"name"`
	Size int64  `json:"size,omitempty"`
}

// attachmentView projects the opened files onto what the model gets back.
func attachmentView(opened []*files.Opened) []sentFile {
	out := make([]sentFile, 0, len(opened))
	for _, o := range opened {
		out = append(out, sentFile{Name: o.Name, Size: o.Size})
	}
	return out
}

// ---------------------------------------------------------------------------
// splitting
// ---------------------------------------------------------------------------

// splitMessage breaks a long body into parts of at most limit runes, preferring
// a line end, then a sentence final, then a space, and hard-cutting only when a
// stretch offers nothing after the halfway point. Cutting at a newline keeps a
// pasted URL whole, which is why the line pass runs first.
//
// The returned hardCut flag is a warning worth telling the model: something was
// cut mid-word.
func splitMessage(s string, limit int) (parts []string, hardCut bool) {
	if limit < 20 {
		limit = 20 // a limit under one word would loop forever
	}
	r := []rune(strings.TrimRight(s, " \t\r\n"))
	if len(r) == 0 {
		return nil, false
	}
	if len(r) <= limit {
		return []string{string(r)}, false
	}
	min := limit / 2
	for len(r) > limit {
		win := r[:limit]
		cut := lastBreak(win, min)
		if cut < 0 {
			cut = limit
			hardCut = true
		}
		part := strings.TrimRight(string(win[:cut]), " \t\r\n")
		r = trimLeftSpace(r[cut:])
		if part == "" {
			continue // the break landed inside a whitespace run
		}
		parts = append(parts, part)
	}
	if tail := strings.TrimRight(string(r), " \t\r\n"); tail != "" {
		parts = append(parts, tail)
	}
	return parts, hardCut
}

// lastBreak returns the exclusive cut index, or -1 when nothing in r[min:] will
// do. Every candidate is at least min runes long so we never emit confetti.
func lastBreak(r []rune, min int) int {
	for i := len(r) - 1; i >= min; i-- {
		if r[i] == '\n' {
			return i + 1
		}
	}
	for i := len(r) - 1; i >= min; i-- {
		switch r[i] {
		case '。', '．', '！', '？', '!', '?', '；', ';', '…':
			j := i + 1
			for j < len(r) && isCloser(r[j]) {
				j++
			}
			if j > min {
				return j
			}
		}
	}
	for i := len(r) - 1; i >= min; i-- {
		if r[i] == ' ' || r[i] == '\t' {
			return i + 1
		}
	}
	return -1
}

func isCloser(r rune) bool {
	switch r {
	case '”', '"', '’', '\'', '）', ')', '】', ']', '》', '」', '』', '。', '！', '？':
		return true
	}
	return false
}

func trimLeftSpace(r []rune) []rune {
	i := 0
	for i < len(r) {
		switch r[i] {
		case ' ', '\t', '\n', '\r':
			i++
			continue
		}
		break
	}
	return r[i:]
}
