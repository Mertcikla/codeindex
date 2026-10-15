package lsp

import (
	"context"
	"encoding/json"
	"fmt"
)

// TypeScriptRequest uses the server's documented tsserver bridge. Synchronous
// diagnostics provide a barrier after didChange; asynchronous notifications can
// refer to an earlier document version and cannot establish snapshot parity.
func (c *Client) TypeScriptRequest(ctx context.Context, command string, args any, result any) error {
	var response *struct {
		Success bool            `json:"success"`
		Message string          `json:"message"`
		Body    json.RawMessage `json:"body"`
	}
	err := c.Call(ctx, "workspace/executeCommand", map[string]any{"command": "typescript.tsserverRequest", "arguments": []any{command, args, map[string]any{"executionTarget": 0, "expectsResult": true, "isAsync": false, "lowPriority": false}}}, &response)
	if err != nil {
		return err
	}
	if response == nil || !response.Success || len(response.Body) == 0 {
		return fmt.Errorf("tsserver %s failed: %+v", command, response)
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(response.Body, result)
}

func (c *Client) CheckTypeScriptDocument(ctx context.Context, path string) error {
	for _, command := range []string{"syntacticDiagnosticsSync", "semanticDiagnosticsSync"} {
		var diagnostics []struct {
			Category string `json:"category"`
			Text     string `json:"text"`
		}
		if err := c.TypeScriptRequest(ctx, command, map[string]any{"file": path, "includeLinePosition": true}, &diagnostics); err != nil {
			return err
		}
		for _, d := range diagnostics {
			if d.Category == "error" {
				return fmt.Errorf("%s: %s", path, d.Text)
			}
		}
	}
	return nil
}
