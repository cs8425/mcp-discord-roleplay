// tools_test.go covers the helpers every handler answers through -- the one place
// all three tools' payloads become a string.

package tools

import (
	"strings"
	"testing"
)

// A tool payload is JSON, not a page, so `<` and `>` must arrive as themselves. Go's
// default escaping turns each into six bytes of `<>`, and because the payload is
// itself a string inside the envelope, that sequence survives one layer of parsing:
// an operator reading a log line sees `\u003c:gaze:img@1\u003e` where the message said
// `<:gaze:img@1>`, and every handle and every `<@id>` mention pays ten bytes against a
// --poll-max-bytes budget that is already tight.
func TestJSONResultKeepsAngleBracketsAndAmpersandsRaw(t *testing.T) {
	res := jsonResult(map[string]any{
		"text": "<:gaze:img@1> <@700000000000000001> https://x/y?format=jpg&name=medium",
	})
	if res.IsError {
		t.Fatalf("marshalling a map must not fail: %+v", res)
	}
	got := res.Content[0].Text
	for _, dead := range []string{`\u003c`, `\u003e`, `\u0026`} {
		if strings.Contains(got, dead) {
			t.Errorf("HTML escaping survived into the payload (%s): %s", dead, got)
		}
	}
	if !strings.Contains(got, "<:gaze:img@1>") {
		t.Errorf("the handle must read the way we wrote it: %s", got)
	}
	if !strings.Contains(got, "&") {
		t.Errorf("a url query separator must stay one byte: %s", got)
	}
}

// Encoder appends a newline Marshal did not. The answer is compared as a whole string
// in places, and a trailing newline the transport has to tolerate is a fact worth
// pinning rather than discovering in a diff.
func TestJSONResultHasNoTrailingNewline(t *testing.T) {
	got := jsonResult(map[string]any{"a": 1}).Content[0].Text
	if strings.HasSuffix(got, "\n") {
		t.Errorf("the payload must be exactly the JSON document, got %q", got)
	}
	if got != `{"a":1}` {
		t.Errorf("want compact JSON, got %q", got)
	}
}

// Map keys stay sorted, which is what makes an answer byte-for-byte reproducible --
// the property the wire-key contract tests lean on.
func TestJSONResultStillSortsMapKeys(t *testing.T) {
	got := jsonResult(map[string]any{"z": 1, "a": 2, "m": 3}).Content[0].Text
	if got != `{"a":2,"m":3,"z":1}` {
		t.Errorf("keys must be sorted: %s", got)
	}
}
