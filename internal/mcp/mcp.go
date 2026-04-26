// Package mcp wires the Model Context Protocol server on top of
// github.com/mark3labs/mcp-go and exposes both stdio and HTTP/SSE
// transports. Tool handlers are registered through this package rather
// than against mcp-go directly so callers can stay decoupled from the
// upstream API.
//
// Choice of SDK: we use mark3labs/mcp-go instead of the official Go SDK
// (modelcontextprotocol/go-sdk). At the time of writing the mark3labs
// library is further along on tool/middleware ergonomics — in particular
// the tool-handler middleware chain makes the `not_paired` pre-handler
// trivial — and is already referenced in REQUIREMENTS.md. If the official
// SDK catches up we can swap implementations behind this package without
// touching tool registrations.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// ErrorCode is a stable string identifier for structured tool errors.
// Clients are expected to branch on these rather than on transport
// errors or free-text messages.
type ErrorCode string

const (
	// ErrNotPaired indicates whatsmeow is not paired/logged-in. Tool
	// calls fail with this until the device has successfully paired.
	ErrNotPaired ErrorCode = "not_paired"
	// ErrInvalidArgument indicates the caller supplied arguments that
	// the tool refused to accept (empty JID, negative limit, ...).
	ErrInvalidArgument ErrorCode = "invalid_argument"
	// ErrNotFound indicates the requested entity could not be located,
	// neither locally nor via whatsmeow's server-side lookup.
	ErrNotFound ErrorCode = "not_found"
	// ErrInternal is the catch-all bucket for failures that are not the
	// caller's fault (DB error, whatsmeow transport error, ...).
	ErrInternal ErrorCode = "internal"
	// ErrAlreadyPaired indicates a pair flow was requested but the
	// device is already linked. Mirrors wa.ErrAlreadyPaired.
	ErrAlreadyPaired ErrorCode = "already_paired"
	// ErrPairInProgress indicates another pair flow is already running
	// (admin SSE or MCP). Mirrors wa.ErrPairInProgress.
	ErrPairInProgress ErrorCode = "pair_in_progress"
	// ErrNotPairing indicates the caller asked to drive an in-progress
	// pair flow but no such flow is active. Mirrors wa.ErrNotPairing.
	ErrNotPairing ErrorCode = "not_pairing"
)

// NotPairedMessage is the stable message returned alongside ErrNotPaired.
// Keep this stable — clients MAY surface it directly.
const NotPairedMessage = "WhatsApp client is not paired. Pair the device via the admin API before calling tools."

// PairingState reports whether whatsmeow is currently paired and logged
// in. Implementations must be safe for concurrent use; they are called
// on every tool invocation.
type PairingState interface {
	IsPaired() bool
}

// PairingStateFunc adapts a plain function to the PairingState interface.
type PairingStateFunc func() bool

// IsPaired implements PairingState.
func (f PairingStateFunc) IsPaired() bool { return f() }

// AlwaysPaired is a PairingState that reports the client as paired. It
// is useful for tests where the whatsmeow layer is not running.
var AlwaysPaired PairingState = PairingStateFunc(func() bool { return true })

// NeverPaired is the inverse of AlwaysPaired — useful for tests that
// need to exercise the not_paired path.
var NeverPaired PairingState = PairingStateFunc(func() bool { return false })

// Handler is the signature of a tool callback. It receives the decoded
// JSON arguments from the client and returns either a JSON-serialisable
// result or an error. Returning a non-nil error surfaces as a
// transport-level failure; tools should prefer returning a structured
// error *Result* (via ErrorResult) instead.
type Handler func(ctx context.Context, args json.RawMessage) (any, error)

// Tool is the registry-level description of a tool. Name is the
// snake_case identifier clients call; Description is human-readable;
// InputSchema and OutputSchema are the JSONSchema objects advertised
// to clients for validation and display; Handler is the callback.
type Tool struct {
	Name         string
	Description  string
	InputSchema  json.RawMessage
	OutputSchema json.RawMessage
	Handler      Handler
}

// Registry is the in-memory set of tools known to a Server. Tools are
// registered before Run; the registry is frozen at Run time.
type Registry struct {
	tools map[string]Tool
}

// NewRegistry returns an empty tool registry.
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// Register adds t. It returns an error if the name is empty, the
// handler is nil, or a tool with the same name is already registered.
func (r *Registry) Register(t Tool) error {
	if t.Name == "" {
		return errors.New("mcp: tool name must not be empty")
	}
	if t.Handler == nil {
		return fmt.Errorf("mcp: tool %q has nil handler", t.Name)
	}
	if _, dup := r.tools[t.Name]; dup {
		return fmt.Errorf("mcp: tool %q already registered", t.Name)
	}
	r.tools[t.Name] = t
	return nil
}

// Names returns the registered tool names in unspecified order. Intended
// for diagnostics and tests.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.tools))
	for n := range r.tools {
		names = append(names, n)
	}
	return names
}

// apply registers every tool in the registry against the mcp-go server.
func (r *Registry) apply(s *mcpserver.MCPServer) {
	for _, t := range r.tools {
		s.AddTool(newMCPTool(t), adaptHandler(t.Handler))
	}
}

func newMCPTool(t Tool) mcpgo.Tool {
	input := t.InputSchema
	if len(input) == 0 {
		// An object-typed empty schema is the safe default: it tells
		// the client "takes an object, no required fields".
		input = json.RawMessage(`{"type":"object","properties":{}}`)
	}
	// NewToolWithRawSchema only sets RawInputSchema; set the raw output
	// schema directly on the returned struct since mcp-go's builder has
	// no "with raw input + raw output" constructor.
	tool := mcpgo.NewToolWithRawSchema(t.Name, t.Description, input)
	if len(t.OutputSchema) > 0 {
		tool.RawOutputSchema = t.OutputSchema
	}
	return tool
}

func adaptHandler(h Handler) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		raw, err := json.Marshal(req.GetArguments())
		if err != nil {
			return mcpgo.NewToolResultErrorFromErr("invalid arguments", err), nil
		}
		result, err := h(ctx, raw)
		if err != nil {
			return mcpgo.NewToolResultErrorFromErr("tool execution failed", err), nil
		}
		if tr, ok := result.(*mcpgo.CallToolResult); ok {
			return tr, nil
		}
		return mcpgo.NewToolResultStructuredOnly(result), nil
	}
}

// NotPairedError constructs the canonical structured tool error
// returned when whatsmeow is not paired.
func NotPairedError() *mcpgo.CallToolResult {
	return ErrorResult(ErrNotPaired, NotPairedMessage)
}

// ErrorResult is the canonical shape for structured tool errors. Clients
// are expected to branch on `code` (an ErrorCode) rather than on the free
// text in `message`. Callers pass a non-empty ErrorCode and a human-
// readable message; the result is marked IsError=true so mcp-go surfaces
// it as a tool-level failure rather than a transport error.
func ErrorResult(code ErrorCode, message string) *mcpgo.CallToolResult {
	payload := map[string]any{
		"code":    string(code),
		"message": message,
	}
	body, _ := json.Marshal(payload)
	return &mcpgo.CallToolResult{
		Content: []mcpgo.Content{
			mcpgo.TextContent{Type: "text", Text: string(body)},
		},
		StructuredContent: payload,
		IsError:           true,
	}
}

// NotFoundError is the canonical "lookup by id/jid found nothing" error.
func NotFoundError(message string) *mcpgo.CallToolResult {
	return ErrorResult(ErrNotFound, message)
}

// InvalidArgumentError is the canonical bad-input tool error.
func InvalidArgumentError(message string) *mcpgo.CallToolResult {
	return ErrorResult(ErrInvalidArgument, message)
}

// InternalError wraps an unexpected error (typically a database failure
// downstream of user input validation) as a structured tool result so
// the transport layer doesn't leak it as a protocol-level crash.
func InternalError(message string) *mcpgo.CallToolResult {
	return ErrorResult(ErrInternal, message)
}

// AlreadyPairedError is the canonical "device is already paired" error
// for the pairing tools.
func AlreadyPairedError(message string) *mcpgo.CallToolResult {
	return ErrorResult(ErrAlreadyPaired, message)
}

// PairInProgressError is the canonical "another pair flow is active"
// error for the pairing tools.
func PairInProgressError(message string) *mcpgo.CallToolResult {
	return ErrorResult(ErrPairInProgress, message)
}

// NotPairingError is the canonical "no active pair flow" error for
// callers of pairing_complete (and PairPhone via pairing_start).
func NotPairingError(message string) *mcpgo.CallToolResult {
	return ErrorResult(ErrNotPairing, message)
}

// pairingMiddleware short-circuits every tool call with a structured
// not_paired error when the pairing state reports false. Registered as
// the outermost middleware so nothing downstream runs pre-pair.
//
// Tools whose names appear in exempt are passed through unconditionally;
// this is how the pairing tools themselves (and the ping health check)
// remain callable before the device has been linked.
func pairingMiddleware(state PairingState, exempt map[string]struct{}) mcpserver.ToolHandlerMiddleware {
	return func(next mcpserver.ToolHandlerFunc) mcpserver.ToolHandlerFunc {
		return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			if _, ok := exempt[req.Params.Name]; ok {
				return next(ctx, req)
			}
			if state != nil && !state.IsPaired() {
				return NotPairedError(), nil
			}
			return next(ctx, req)
		}
	}
}
