package mcp

import (
	"context"
	"encoding/json"
	"time"
)

// pingSchema is the input JSONSchema for the ping tool. It accepts an
// optional snake_case echo field to prove argument plumbing works.
var pingSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "echo": {
      "type": "string",
      "description": "Optional string echoed back in the response."
    }
  },
  "additionalProperties": false
}`)

// PingResult is the structured output of the ping tool.
type PingResult struct {
	Pong      bool   `json:"pong"`
	Echo      string `json:"echo,omitempty"`
	Paired    bool   `json:"paired"`
	Timestamp string `json:"timestamp"`
}

func registerBuiltins(reg *Registry, state PairingState) error {
	return reg.Register(Tool{
		Name:        "ping",
		Description: "Health-check tool. Returns pong with a server timestamp.",
		InputSchema: pingSchema,
		Handler: func(_ context.Context, args json.RawMessage) (any, error) {
			var in struct {
				Echo string `json:"echo"`
			}
			if len(args) > 0 {
				// Ignore decode errors here — the schema enforces the
				// shape and a stray unknown field shouldn't break a
				// liveness probe.
				_ = json.Unmarshal(args, &in)
			}
			return PingResult{
				Pong:      true,
				Echo:      in.Echo,
				Paired:    state != nil && state.IsPaired(),
				Timestamp: time.Now().UTC().Format(time.RFC3339),
			}, nil
		},
	})
}
