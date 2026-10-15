package indexer

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	pb "buf.build/gen/go/tldiagramcom/diagram/protocolbuffers/go/codeindex/v1"
	"github.com/mertcikla/codeindex/config"
	"github.com/mertcikla/codeindex/gitstate"
)

func TestTypeScriptCompilerSemanticParity(t *testing.T) { testTypeScriptCompilerParity(t, false) }
func TestMixedTypeScriptCompilerParity(t *testing.T) {
	if _, err := exec.LookPath("scip-go"); err != nil {
		t.Skip("scip-go required")
	}
	testTypeScriptCompilerParity(t, true)
}
func testTypeScriptCompilerParity(t *testing.T, mixed bool) {
	if os.Getenv("CODEINDEX_TEST_TS_LSP") == "" {
		t.Skip("set CODEINDEX_TEST_TS_LSP=1 for real compiler/SCIP parity")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	put := func(name, text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	put("package.json", `{"name":"compiler-fixture","version":"1.0.0"}`)
	cfgText := `{"compilerOptions":{"strict":true,"target":"ES2022","module":"commonjs","jsx":"preserve"},"include":["*.ts","*.tsx"]}`
	put("tsconfig.json", cfgText)
	put("tsconfig.lib.json", cfgText)
	put("jsx.d.ts", "export {};declare global{namespace JSX{interface Element{} interface IntrinsicElements{span:{title?:string}}}}")
	api := "export function helper(n:number){return n+1}\nexport function other(n:number){return n+2}\n"
	view := "import {helper,other} from './api';export default function View(){return <span title='before'>{helper(1)}</span>}\n"
	if mixed {
		put("go.mod", "module example.com/mixed\n\ngo 1.26\n")
		put("base.go", "package mixed\nfunc Base() {}\n")
	}
	put("api.ts", api)
	put("view.tsx", view)
	put("caller.ts", "import View from './view';export function useView(){return View()}\n")
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "-qm", "base"}} {
		c := exec.CommandContext(ctx, "git", args...)
		c.Dir = root
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	cfg := config.Default()
	session := NewWatchSession(ctx, cfg, root)
	defer session.Close()
	var manifests gitstate.ManifestCache
	var syntax SyntaxCache
	pipeline := Pipeline{Config: cfg, Semantics: session, ManifestCache: &manifests, SyntaxCache: &syntax}
	req := &pb.IndexRequest{Directory: root}
	snap, g, err := pipeline.Build(ctx, req, nil)
	if err != nil || len(snap.GetWarnings()) > 0 {
		t.Fatalf("baseline: %v %v", err, snap.GetWarnings())
	}
	base := baseFor(snap, g)
	base.Snapshot.Provenance = "commit"
	for _, tc := range []struct {
		name, source, api string
		fast              bool
	}{
		{"committed baseline requires live SCIP", strings.Replace(view, "before", "live", 1), api, false},
		{"inferred TSX label", strings.Replace(view, "before", "after", 1), api, true},
		{"changed callee", strings.Replace(view, "{helper(1)}", "{other(1)}", 1), api, true},
		{"revert", view, api, true},
		{"API change", view, strings.Replace(api, "return n+1", "return 'different'", 1), false},
		{"after API fallback", strings.Replace(view, "before", "again", 1), strings.Replace(api, "return n+1", "return 'different'", 1), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			put("view.tsx", tc.source)
			put("api.ts", tc.api)
			state, err := gitstate.CaptureQuick(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			pipeline.WatchState = &state
			var details []string
			current, incremental, reused, err := pipeline.BuildIncremental(ctx, req, func(p Progress) { details = append(details, p.Detail) }, base)
			if err != nil || reused || len(current.GetWarnings()) > 0 {
				t.Fatalf("refresh: %v %v %v", err, reused, current.GetWarnings())
			}
			fast := strings.Contains(strings.Join(details, "\n"), "TypeScript compiler semantic refresh")
			if fast != tc.fast {
				t.Fatalf("compiler=%v want %v:\n%s", fast, tc.fast, strings.Join(details, "\n"))
			}
			freshSnap, fresh, err := (Pipeline{Config: cfg}).Build(ctx, req, nil)
			if err != nil || len(freshSnap.GetWarnings()) > 0 {
				t.Fatalf("full: %v %v", err, freshSnap.GetWarnings())
			}
			if !reflect.DeepEqual(normalizeGraph(t, incremental), normalizeGraph(t, fresh)) {
				t.Fatalf("compiler graph differs: missing %v extra %v", difference(normalizeGraph(t, fresh), normalizeGraph(t, incremental)), difference(normalizeGraph(t, incremental), normalizeGraph(t, fresh)))
			}
			base = baseFor(current, incremental)
		})
	}
}
