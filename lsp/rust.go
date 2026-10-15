package lsp

import (
	"context"
	"fmt"
)

type rustStatus struct {
	Health    string `json:"health"`
	Quiescent bool   `json:"quiescent"`
	Message   string `json:"message"`
}

// StartRust uses the same Cargo/proc-macro defaults as rust-analyzer scip.
// Flycheck is disabled: native pull diagnostics provide the per-batch barrier,
// without spawning cargo check on each save.
func StartRust(ctx context.Context, executable, root string) (*Client, error) {
	return start(ctx, executable, nil, root, "rust", map[string]any{"checkOnSave": false})
}

// WaitRustReady rejects incomplete Cargo/proc-macro loading. An initialize
// response alone only establishes transport availability, not semantic readiness.
func (c *Client) WaitRustReady(ctx context.Context) error {
	for {
		c.mu.Lock()
		status, changed := c.rustStatus, c.rustChanged
		c.mu.Unlock()
		if status != nil && status.Quiescent {
			if status.Health != "ok" {
				return fmt.Errorf("rust-analyzer workspace %s: %s", status.Health, status.Message)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.done:
			return fmt.Errorf("rust-analyzer disconnected before workspace became ready")
		case <-changed:
		}
	}
}
