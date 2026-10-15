package lsp

import (
	"context"
	"encoding/json"
	"fmt"
)

func StartPyright(ctx context.Context, executable, root string) (*Client, error) {
	return start(ctx, executable, []string{"--stdio"}, root, "python", map[string]any{})
}

// Pyright reports its version in the startup notification rather than serverInfo
// or a --version flag on pyright-langserver. Access is serialized with the reader.
func (c *Client) PyrightVersion() string { c.mu.Lock(); defer c.mu.Unlock(); return c.pyrightVersion }

// Pyright implements pull diagnostics without advertising diagnosticProvider.
// A result ID proves the opened file was actually analyzed: unknown files can
// otherwise return an empty report indistinguishable from a clean file.
func (c *Client) CheckPythonDocument(ctx context.Context, path string) error {
	var report *struct {
		Kind     string          `json:"kind"`
		ResultID string          `json:"resultId"`
		Items    json.RawMessage `json:"items"`
	}
	if err := c.Call(ctx, "textDocument/diagnostic", map[string]any{"textDocument": map[string]any{"uri": URI(path)}}, &report); err != nil {
		return err
	}
	if report == nil || report.Kind != "full" || report.ResultID == "" || len(report.Items) == 0 || string(report.Items) == "null" {
		return fmt.Errorf("Pyright did not analyze %s", path)
	}
	var diagnostics []struct {
		Severity int    `json:"severity"`
		Message  string `json:"message"`
	}
	if err := json.Unmarshal(report.Items, &diagnostics); err != nil {
		return err
	}
	for _, d := range diagnostics {
		if d.Severity == 1 {
			return fmt.Errorf("%s: %s", path, d.Message)
		}
	}
	return nil
}
