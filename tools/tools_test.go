package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mertcikla/codeindex/config"
)

func writeExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
}

func TestResolveNameAliasOnPath(t *testing.T) {
	dir := t.TempDir()
	want := filepath.Join(dir, "scip_dart")
	writeExecutable(t, want)
	t.Setenv("PATH", dir)

	got, err := ResolveName("scip-dart")
	if err != nil {
		t.Fatalf("ResolveName: %v", err)
	}
	if got != want {
		t.Fatalf("ResolveName = %q, want %q", got, want)
	}
}

func TestResolveNameWellKnownDir(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, ".pub-cache", "bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(bin, "scip_dart")
	writeExecutable(t, want)
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())

	got, err := ResolveName("scip-dart")
	if err != nil {
		t.Fatalf("ResolveName: %v", err)
	}
	if got != want {
		t.Fatalf("ResolveName = %q, want %q", got, want)
	}
}

func TestResolveNameDirectPath(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "scip-go")
	writeExecutable(t, exe)
	got, err := ResolveName(exe)
	if err != nil {
		t.Fatalf("ResolveName: %v", err)
	}
	if got != exe {
		t.Fatalf("ResolveName = %q, want %q", got, exe)
	}
}

func TestResolveNameMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	if _, err := ResolveName("scip-nonexistent-tool"); err == nil {
		t.Fatal("ResolveName accepted a missing tool")
	}
}

func TestGoToolsIncludeLanguageServerHealth(t *testing.T) {
	selected := ForFamilies([]string{"go"})
	if len(selected) != 2 || selected[1].Name != "gopls" || !selected[1].LanguageServer {
		t.Fatalf("Go tools: %+v", selected)
	}
	tool := filepath.Join(t.TempDir(), "gopls")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\necho 'golang.org/x/tools/gopls v0.24.0'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Tools.Gopls = tool
	status := CheckTools(context.Background(), cfg, selected[1:])[0]
	if status.Found || !strings.Contains(status.Error, "LSP health check:") || status.Version == "" {
		t.Fatalf("accepted a version-only fake server: %+v", status)
	}
}

func TestTypeScriptToolsIncludeLanguageServerHealth(t *testing.T) {
	selected := ForFamilies([]string{"web"})
	if len(selected) != 2 || selected[1].Name != "typescript-language-server" || !selected[1].LanguageServer {
		t.Fatalf("TypeScript tools: %+v", selected)
	}
	tool := filepath.Join(t.TempDir(), "typescript-language-server")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\necho '5.1.3'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Tools.TypeScriptLanguageServer = tool
	status := CheckTools(context.Background(), cfg, selected[1:])[0]
	if status.Found || !strings.Contains(status.Error, "LSP health check:") {
		t.Fatalf("accepted a version-only fake server: %+v", status)
	}
}

func TestResolveUsesConfiguredPath(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "scip-go")
	writeExecutable(t, exe)
	cfg := config.Default()
	cfg.Tools.SCIPGo = exe
	tool := Tool{Name: "scip-go", path: func(config.Config) string { return cfg.Tools.SCIPGo }}

	got, err := Resolve(tool, cfg)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != exe {
		t.Fatalf("Resolve = %q, want %q", got, exe)
	}
}

func TestCheckFlagsBelowMinimum(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "scip-go")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\necho 'scip-go 0.1.1'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Tools.SCIPGo = exe
	tool := Tool{Name: "scip-go", VersionArgs: []string{"--version"}, MinVersion: "0.1.26", path: func(config.Config) string { return cfg.Tools.SCIPGo }}

	status := checkOne(context.Background(), tool, cfg)
	if !status.Found || !status.BelowMinimum || status.Minimum != "0.1.26" {
		t.Fatalf("status = %+v, want below-minimum", status)
	}
}

func TestVersionAtLeast(t *testing.T) {
	cases := []struct {
		version, minimum    string
		atLeast, comparable bool
	}{
		{"scip-go 0.1.26", "0.1.26", true, true},
		{"scip-go 0.1.25", "0.1.26", false, true},
		{"rust-analyzer 1.95.0 (59807616 2026-04-14)", "1.80.0", true, true},
		{"scip-ruby 0.5.0 git abc", "0.5.0", true, true},
		{"usage: scip-php [options]", "0.1.0", false, false},
		{"scip-java version 0.0.0-SNAPSHOT", "0.1.0", false, true},
	}
	for _, c := range cases {
		atLeast, comparable := versionAtLeast(c.version, c.minimum)
		if atLeast != c.atLeast || comparable != c.comparable {
			t.Fatalf("versionAtLeast(%q,%q) = (%v,%v), want (%v,%v)", c.version, c.minimum, atLeast, comparable, c.atLeast, c.comparable)
		}
	}
}

func TestProbeVersion(t *testing.T) {
	const dockerWarning = "WARNING: The requested image's platform (linux/amd64) does not match the detected host platform (linux/arm64/v8) and no specific platform was requested"
	cases := []struct {
		name, output, want string
		fail               bool
	}{
		{name: "bare version", output: "0.4.0", want: "0.4.0"},
		{name: "prefixed version", output: "v0.4.0", want: "v0.4.0"},
		{name: "tool banner", output: "scip-go 0.2.7", want: "scip-go 0.2.7"},
		{name: "version label", output: "Version: 0.2.14", want: "Version: 0.2.14"},
		{name: "tool version label", output: "scip-clang version 0.4.0", want: "scip-clang version 0.4.0"},
		{name: "prerelease", output: "scip-java version 0.0.0-SNAPSHOT", want: "scip-java version 0.0.0-SNAPSHOT"},
		{name: "build metadata", output: "rust-analyzer 1.95.0 (59807616 2026-04-14)", want: "rust-analyzer 1.95.0 (59807616 2026-04-14)"},
		{name: "docker warning", output: dockerWarning},
		{name: "warning before version", output: dockerWarning + "\nscip-go 0.2.7", want: "scip-go 0.2.7"},
		{name: "warning with version number", output: "WARNING: Docker 27.1.0 requires an update\n0.4.0", want: "0.4.0"},
		{name: "help text", output: "Usage: scip-php [options]\n  --version Show version\n  --memory 512 Maximum memory"},
		{name: "help with runtime version", output: "Usage: scip-php [options]\nRequires PHP 8.2.0 or later\nSee https://example.com/releases/1.2.3"},
		{name: "warning and help", output: dockerWarning + "\nUsage: scip-php [options]"},
		{name: "runtime version", output: "PHP 8.2.0 (cli)"},
		{name: "failed probe", output: "scip-go 0.2.7", fail: true},
		{name: "empty output"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			exe := filepath.Join(t.TempDir(), "indexer")
			// Emit diagnostics on stderr too, as Docker launchers do.
			script := "#!/bin/sh\ncat >&2 <<'PROBE_OUTPUT'\n" + c.output + "\nPROBE_OUTPUT\n"
			if c.fail {
				script += "exit 1\n"
			}
			if err := os.WriteFile(exe, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			if got := probeVersion(context.Background(), exe, []string{"--version"}); got != c.want {
				t.Fatalf("probeVersion = %q, want %q", got, c.want)
			}
		})
	}
}
