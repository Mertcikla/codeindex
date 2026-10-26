package lsp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Standalone SDKs exercise protocol compatibility independently of SCIP's pinned
// compiler. The indexer parity fixture covers the explicit SCIP SDK separately.
func TestTypeScriptClientCompatibility(t *testing.T) {
	if os.Getenv("CODEINDEX_TEST_TS_HEALTH") == "" {
		t.Skip("set CODEINDEX_TEST_TS_HEALTH for the Docker compatibility matrix")
	}
	// Failing rows (no tsserver.js or no synchronous bridge) are asserted by
	// tools' health test, including its repair hint.
	if os.Getenv("CODEINDEX_TEST_TS_HEALTH") != "pass" || os.Getenv("SCIP_VERSION") != "" {
		t.Skip("standalone compatible SDK required; SCIP rows run graph parity")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	root := t.TempDir()
	path := filepath.Join(root, "main.ts")
	original := "function target(value: number): number { return value; }\ntarget(1);\n"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := StartTypeScript(ctx, "typescript-language-server", root, "")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	for _, tc := range []struct {
		name, source string
		valid        bool
	}{
		{"initial", original, true},
		{"invalid unsaved edit", strings.Replace(original, "target(1)", "target('bad')", 1), false},
		{"repaired unsaved edit", original, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := client.Sync(path, []byte(tc.source)); err != nil {
				t.Fatal(err)
			}
			if err := client.CheckTypeScriptDocument(ctx, path); (err == nil) != tc.valid {
				t.Fatalf("valid=%v diagnostics=%v", tc.valid, err)
			}
			locations, err := client.Definition(ctx, path, Position{Line: 1, Character: 1})
			if err != nil || len(locations) != 1 || locations[0].URI != URI(path) || locations[0].Range.Start.Line != 0 {
				t.Fatalf("definition: %+v %v", locations, err)
			}
		})
	}
}
