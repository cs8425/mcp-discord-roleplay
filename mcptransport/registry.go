package mcptransport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
)

var (
	ErrBadParam  = errors.New("mcptransport: bad parameter")
	ErrDuplicate = errors.New("mcptransport: duplicate tool name")
	ErrUnknown   = errors.New("mcptransport: unknown tool")
)

// Tool is a tool definition as returned by tools/list.
type Tool struct {
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	InputSchema Schema `json:"inputSchema"`
}

// Call is a tools/call invocation handed to a Handler.
type Call struct {
	Name      string
	Arguments json.RawMessage
}

// Handler processes one tool call. Return a non-nil error for protocol-level
// failures (bad arguments, internal error); use (*ToolResult).IsError for
// tool-execution errors the model can recover from.
type Handler func(ctx context.Context, call *Call) (*ToolResult, error)

// Registry maps tool names to handlers and lists tools for tools/list.
type Registry struct {
	prefix   string
	tools    []*Tool
	handlers map[string]Handler
	schemas  map[string]Schema // by full name; used to coerce string/number booleans
}

// NewRegistry returns a Registry that prefixes every registered tool name
// with prefix (empty prefix = names unchanged).
func NewRegistry(prefix string) *Registry {
	return &Registry{
		prefix:   prefix,
		tools:    make([]*Tool, 0, 16),
		handlers: make(map[string]Handler),
		schemas:  make(map[string]Schema),
	}
}

// Register adds a tool and its handler. The tool name is prefixed in place.
func (r *Registry) Register(t *Tool, h Handler) error {
	if t == nil || t.Name == "" || h == nil {
		return ErrBadParam
	}
	name := r.prefix + t.Name
	if _, ok := r.handlers[name]; ok {
		return fmt.Errorf("%w: %s", ErrDuplicate, name)
	}
	t.Name = name
	r.tools = append(r.tools, t)
	r.handlers[name] = h
	r.schemas[name] = t.InputSchema
	return nil
}

// Dispatch runs the handler for call.Name. A panicking handler is recovered and
// turned into a tool-execution error so that a single bad tool call can never
// take down the whole server process. This matters because stdio runs each
// request in its own goroutine WITHOUT a recover (an unrecovered panic there
// kills the process), whereas Streamable HTTP only recovers at the net/http
// layer. Recovering here covers both transports from one choke point.
func (r *Registry) Dispatch(ctx context.Context, call *Call) (res *ToolResult, err error) {
	h, ok := r.handlers[call.Name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknown, call.Name)
	}
	// Silently normalize string/number values that the schema declares as
	// boolean (e.g. "true"/"1"/"T") so typed handlers using bool/*bool do not
	// choke. Best-effort: any issue leaves Arguments unchanged. Covers both
	// transports since they funnel through this single dispatch point.
	if schema, ok := r.schemas[call.Name]; ok && len(call.Arguments) > 0 {
		call.Arguments = coerceBooleans(call.Arguments, &schema)
	}
	defer func() {
		if p := recover(); p != nil {
			log.Printf("mcptransport: tool %q panicked and was recovered: %v", call.Name, p)
			res = ResultError(fmt.Sprintf("tool %s panicked: %v", call.Name, p))
			err = nil
		}
	}()
	return h(ctx, call)
}

// List returns the registered tools in registration order (deterministic).
func (r *Registry) List() []*Tool {
	out := make([]*Tool, len(r.tools))
	copy(out, r.tools)
	return out
}
