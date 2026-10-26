package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mertcikla/codeindex/config"
	"github.com/mertcikla/codeindex/lsp"
)

func TestMatchesTypeScriptHealthDefinition(t *testing.T) {
	health := filepath.Join(t.TempDir(), "space π # % +", "__codeindex_health__.ts")
	uri := lsp.URI(health)
	for _, tt := range []struct {
		name string
		uri  string
		line int
		char int
		want bool
	}{
		{name: "canonical", uri: uri, char: 9, want: true},
		{name: "alternate escapes", uri: strings.ReplaceAll(uri, "__codeindex", "%5F%5Fcodeindex"), char: 9, want: true},
		{name: "different file", uri: lsp.URI(health + ".other"), char: 9},
		{name: "different line", uri: uri, line: 1, char: 9},
		{name: "different character", uri: uri, char: 10},
		{name: "malformed escape", uri: uri + "%zz", char: 9},
		{name: "non-file URI", uri: "https://example.com/health.ts", char: 9},
	} {
		t.Run(tt.name, func(t *testing.T) {
			loc := lsp.Location{URI: tt.uri, Range: lsp.Range{Start: lsp.Position{Line: tt.line, Character: tt.char}}}
			if got := matchesTypeScriptHealthDefinition(health, loc); got != tt.want {
				t.Errorf("matchesTypeScriptHealthDefinition(%q, %+v) = %v, want %v", health, loc, got, tt.want)
			}
		})
	}
}

func TestTypeScriptSDK(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node.js required for npm SDK resolution")
	}
	for _, layout := range []string{"nested", "hoisted", "pnpm", "missing tsserver", "missing package", "nearer incompatible"} {
		t.Run(layout, func(t *testing.T) {
			root := t.TempDir()
			root, err := filepath.EvalSymlinks(root)
			if err != nil {
				t.Fatal(err)
			}
			entry := filepath.Join(root, "node_modules", "@sourcegraph", "scip-typescript", "dist", "main.js")
			write := func(path, text string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(text), 0700); err != nil {
					t.Fatal(err)
				}
			}
			module := filepath.Join(root, "node_modules", "typescript")
			if layout == "nested" || layout == "nearer incompatible" {
				module = filepath.Join(filepath.Dir(filepath.Dir(entry)), "node_modules", "typescript")
			}
			if layout == "pnpm" {
				entry = filepath.Join(root, "node_modules", ".pnpm", "scip-typescript@0.4.0", "node_modules", "@sourcegraph", "scip-typescript", "dist", "main.js")
				module = filepath.Join(root, "node_modules", ".pnpm", "typescript@5.6.3", "node_modules", "typescript")
				link := filepath.Join(root, "node_modules", ".pnpm", "scip-typescript@0.4.0", "node_modules", "typescript")
				if err := os.MkdirAll(filepath.Dir(link), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(module, link); err != nil {
					t.Fatal(err)
				}
			}
			write(entry, "#!/usr/bin/env node\n")
			bin := filepath.Join(root, "bin", "scip-typescript")
			if err := os.MkdirAll(filepath.Dir(bin), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(entry, bin); err != nil {
				t.Fatal(err)
			}
			want := filepath.Join(module, "lib", "tsserver.js")
			if layout != "missing package" {
				write(filepath.Join(module, "package.json"), `{"name":"typescript","version":"7.0.2"}`)
			}
			if layout == "nearer incompatible" {
				write(filepath.Join(root, "node_modules", "typescript", "lib", "tsserver.js"), "")
			} else if layout != "missing tsserver" && layout != "missing package" {
				write(want, "")
			}
			got, err := TypeScriptSDK(context.Background(), bin)
			if strings.HasPrefix(layout, "missing") || layout == "nearer incompatible" {
				if err == nil || !strings.Contains(err.Error(), "tsserver.js") {
					t.Fatalf("accepted incompatible SDK: %q %v", got, err)
				}
			} else if err != nil || got != want {
				t.Fatalf("SDK = %q, %v; want %q", got, err, want)
			}
		})
	}
}

// Used by scripts/test-typescript-compat.sh in fresh containers. A failing
// installation must report both the failure and a usable repair command.
func TestTypeScriptLanguageServerIntegration(t *testing.T) {
	expect := os.Getenv("CODEINDEX_TEST_TS_HEALTH")
	if expect == "" {
		t.Skip("set CODEINDEX_TEST_TS_HEALTH=pass or fail for real npm tools")
	}
	if expect != "pass" && expect != "fail" {
		t.Fatalf("invalid health expectation %q", expect)
	}
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	status := CheckTools(context.Background(), cfg, ForFamilies([]string{"web"})[1:])[0]
	t.Logf("TypeScript language server: %+v", status)
	if status.Found != (expect == "pass") || status.Version == "" {
		t.Fatalf("unexpected health: %+v", status)
	}
	if !status.Found && (!strings.Contains(status.Error, "LSP health check:") || !strings.Contains(status.Error, "npm install -g typescript-language-server typescript@6")) {
		t.Fatalf("missing actionable repair: %+v", status)
	}
	if status.Found && status.BelowMinimum {
		t.Fatalf("working matrix version marked outdated: %+v", status)
	}
}
