package codex

import (
	"encoding/json"
	"testing"
)

func TestNativeMCPApprovalCorrelation(t *testing.T) {
	// Shape captured from Codex 0.153.1 with Astra, read-only fake Notes,
	// and an explicitly denied request. No model prose supplies authority.
	params := `{"threadId":"thread","turnId":"turn","serverName":"authorized_notes","mode":"form","_meta":{"codex_approval_kind":"mcp_tool_call","persist":["session","always"],"tool_description":"Read fake notes","tool_params":{"note_id":"shopping"},"tool_params_display":[{"name":"note_id","value":"shopping"}]},"message":"Allow Notes?","requestedSchema":{"type":"object","properties":{}}}`
	item := json.RawMessage(`{"type":"mcpToolCall","id":"call","server":"authorized_notes","tool":"notes_get","status":"inProgress","arguments":{"note_id":"shopping"}}`)
	msg := rpcMessage{ID: json.RawMessage(`0`), Method: "mcpServer/elicitation/request", Params: json.RawMessage(params)}
	req, ok := decodeApproval(msg, "thread", "turn", map[string]json.RawMessage{"call": item})
	if !ok || req.ItemID != "call" || req.MCP.ToolName != "notes_get" || string(req.MCP.Arguments) != `{"note_id":"shopping"}` {
		t.Fatalf("expected correlated native approval, got %#v supported=%v", req, ok)
	}
	for _, tc := range []struct {
		name  string
		items map[string]json.RawMessage
	}{
		{"missing", nil},
		{"changed arguments", map[string]json.RawMessage{"call": json.RawMessage(`{"type":"mcpToolCall","id":"call","server":"authorized_notes","tool":"notes_get","status":"inProgress","arguments":{"note_id":"private"}}`)}},
		{"completed", map[string]json.RawMessage{"call": json.RawMessage(`{"type":"mcpToolCall","id":"call","server":"authorized_notes","tool":"notes_get","status":"completed","arguments":{"note_id":"shopping"}}`)}},
		{"wrong server", map[string]json.RawMessage{"call": json.RawMessage(`{"type":"mcpToolCall","id":"call","server":"other","tool":"notes_get","status":"inProgress","arguments":{"note_id":"shopping"}}`)}},
		{"ambiguous", map[string]json.RawMessage{"call": item, "other": json.RawMessage(`{"type":"mcpToolCall","id":"other","server":"authorized_notes","tool":"notes_get","status":"inProgress","arguments":{"note_id":"shopping"}}`)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := decodeApproval(msg, "thread", "turn", tc.items); ok {
				t.Fatal("accepted uncorrelated approval")
			}
		})
	}
	var p map[string]any
	if err := json.Unmarshal(msg.Params, &p); err != nil {
		t.Fatal(err)
	}
	p["_meta"].(map[string]any)["persistent_grant"] = true
	msg.Params, _ = json.Marshal(p)
	if _, ok := decodeApproval(msg, "thread", "turn", map[string]json.RawMessage{"call": item}); ok {
		t.Fatal("accepted unknown approval metadata")
	}
}
