package handlers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// computerGateway is the desktop enforcement gateway (internal/desktopgateway)
// the `computer` tool proxies to. A local interface so this package need not
// import the gateway package (the concrete *desktopgateway.Gateway satisfies
// it); the gateway owns control-lock arbitration, scale-from-zero, and
// per-sidecar auth (design §9).
type computerGateway interface {
	Screenshot(ctx context.Context, workspaceID string) ([]byte, error)
	Input(ctx context.Context, workspaceID string, action json.RawMessage) error
}

// SetComputerGateway wires the desktop computer-use gateway. Unset (nil) leaves
// the `computer` tool reporting the desktop unavailable — the per-tier gate.
func (h *MCPHandler) SetComputerGateway(g computerGateway) { h.computerGateway = g }

// init registers the `computer` tool (append, so the large mcpAllTools literal
// stays untouched). The tool is the agent's hands+eyes on the workspace desktop.
func init() {
	mcpAllTools = append(mcpAllTools, mcpTool{
		Name:        "computer",
		Description: "Drive the workspace desktop like a human: take a screenshot and click/type/key/scroll. Use when a task needs a real browser or GUI (open a site, click through a web app) rather than an API. Requires a vision-capable model to read the screenshot.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"action": map[string]interface{}{"type": "string", "enum": []string{"screenshot", "click", "type", "key", "scroll"}, "description": "The action to perform."},
				"x":      map[string]interface{}{"type": "integer", "description": "Click X (0..width-1)."},
				"y":      map[string]interface{}{"type": "integer", "description": "Click Y (0..height-1)."},
				"button": map[string]interface{}{"type": "string", "enum": []string{"left", "middle", "right"}, "description": "Mouse button for click (default left)."},
				"text":   map[string]interface{}{"type": "string", "description": "Text to type."},
				"keys":   map[string]interface{}{"type": "string", "description": "Key chord, e.g. \"ctrl+v\", \"Return\"."},
				"amount": map[string]interface{}{"type": "integer", "description": "Scroll notches, signed (+down/-up)."},
			},
			"required": []string{"action"},
		},
	})
}

// toolComputer proxies a computer-use action to the enforcement gateway.
//
// FOLLOW-UPS (need the workspace-write path + the adapter lookup, so they're
// flagged, not faked): (1) per the SSOT contract the production screenshot
// result_shape is attachment_uri (a workspace:/…png reference); this returns a
// base64 data URI as a working interim (the contract's alternative "image_block"
// shape) so the tool is usable on vision-capable adapters now. (2) The tool
// should be advertised ONLY to vision-capable adapters (a text-only adapter
// cannot see the screenshot) — the mcpToolList gate for that is the follow-up.
func (h *MCPHandler) toolComputer(ctx context.Context, workspaceID string, args map[string]interface{}) (string, error) {
	if h.computerGateway == nil {
		return "", fmt.Errorf("desktop computer-use is not available on this deployment")
	}
	action, _ := args["action"].(string)
	switch action {
	case "screenshot":
		png, err := h.computerGateway.Screenshot(ctx, workspaceID)
		if err != nil {
			return "", err
		}
		return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png), nil
	case "click", "type", "key", "scroll":
		raw, err := json.Marshal(map[string]interface{}{
			"type":   action,
			"x":      argInt(args["x"]),
			"y":      argInt(args["y"]),
			"button": argStr(args["button"]),
			"text":   argStr(args["text"]),
			"keys":   argStr(args["keys"]),
			"amount": argInt(args["amount"]),
		})
		if err != nil {
			return "", err
		}
		if err := h.computerGateway.Input(ctx, workspaceID, raw); err != nil {
			return "", err
		}
		return "ok", nil
	default:
		return "", fmt.Errorf("unsupported computer action %q (want screenshot|click|type|key|scroll)", action)
	}
}

func argInt(v interface{}) int {
	switch n := v.(type) {
	case float64: // JSON numbers decode to float64
		return int(n)
	case int:
		return n
	}
	return 0
}

func argStr(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
