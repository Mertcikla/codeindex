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
)

// Exercise real cross-module references, requiring the LSP path rather than
// accepting a silent SCIP fallback as proof of incremental parity.
func TestGoLSPModuleContextParity(t *testing.T) {
	if os.Getenv("CODEINDEX_TEST_LSP") == "" {
		t.Skip("set CODEINDEX_TEST_LSP=1 for workspace / replacement gopls / SCIP parity")
	}
	for _, mode := range []string{"ancestor workspace", "explicit workspace", "local replacement"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			workspace, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(workspace, "app")
			put := func(name, text string) {
				t.Helper()
				path := filepath.Join(workspace, name)
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(text), 0600); err != nil {
					t.Fatal(err)
				}
			}
			put("dep/go.mod", "module example.com/dependency\n\ngo 1.26\n")
			put("dep/dep.go", "package dependency\ntype Service interface { Call(int) int }\nvar API Service\nfunc Help(n int) int { return n }\nfunc Other(n int) int { return n + 1 }\n")
			module := "module example.com/app\n\ngo 1.26\n\nrequire example.com/dependency v0.0.0\n"
			switch mode {
			case "ancestor workspace":
				t.Setenv("GOWORK", "")
				put("go.work", "go 1.26\n\nuse (\n ./app\n ./dep\n)\n")
			case "explicit workspace":
				t.Setenv("GOWORK", filepath.Join(workspace, ".dev", "go.work"))
				put(".dev/go.work", "go 1.26\n\nuse (\n ../app\n ../dep\n)\n")
			case "local replacement":
				t.Setenv("GOWORK", "off")
				module += "\nreplace example.com/dependency => ../dep\n"
			}
			put("app/go.mod", module)
			original := "package worker\nimport \"example.com/dependency\"\nfunc Work(n int) int { return dependency.API.Call(dependency.Help(n + 1)) }\nfunc Other(n int) int { return n }\n"
			put("app/worker/worker.go", original)
			put("app/caller/caller.go", "package caller\nimport \"example.com/app/worker\"\nfunc Call() int { return worker.Work(1) }\n")
			for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"-c", "user.name=Fixture", "-c", "user.email=fixture@example.com", "commit", "-qm", "base"}} {
				cmd := exec.CommandContext(ctx, "git", args...)
				cmd.Dir = root
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git: %v %s", err, out)
				}
			}
			cfg := config.Default()
			session := NewWatchSession(ctx, cfg, root)
			defer session.Close()
			pipeline := Pipeline{Config: cfg, Semantics: session}
			req := &pb.IndexRequest{Directory: root}
			snapshot, initial, err := pipeline.Build(ctx, req, nil)
			if err != nil || len(snapshot.GetWarnings()) > 0 {
				t.Fatalf("base: %v %v", err, snapshot.GetWarnings())
			}
			base := baseFor(snapshot, initial)
			for _, edit := range []struct{ name, text string }{
				{"body", strings.Replace(original, "n + 1", "n + 2", 1)},
				{"new dependency callee", strings.Replace(original, "dependency.Help", "dependency.Other", 1)},
				{"shifted anchors", strings.Replace(original, "return dependency.API.Call(dependency.Help(n + 1))", "\n// 😀 shifted body\n_ = \"π\"\nreturn Other(dependency.Help(n))\n", 1)},
				{"revert", original},
			} {
				t.Run(edit.name, func(t *testing.T) {
					put("app/worker/worker.go", edit.text)
					var details []string
					started := time.Now()
					current, incremental, reused, err := pipeline.BuildIncremental(ctx, req, func(p Progress) { details = append(details, p.Detail) }, base)
					elapsed := time.Since(started)
					if err != nil || reused || len(current.GetWarnings()) > 0 || !strings.Contains(strings.Join(details, "\n"), "Go LSP semantic refresh") {
						t.Fatalf("LSP: %v reused=%v warnings=%v\n%s", err, reused, current.GetWarnings(), strings.Join(details, "\n"))
					}
					started = time.Now()
					freshSnapshot, fresh, err := (Pipeline{Config: cfg}).Build(ctx, req, nil)
					if err != nil || len(freshSnapshot.GetWarnings()) > 0 {
						t.Fatalf("SCIP: %v %v", err, freshSnapshot.GetWarnings())
					}
					got, want := normalizeGraph(t, incremental), normalizeGraph(t, fresh)
					if !reflect.DeepEqual(got, want) {
						missing, extra := difference(want, got), difference(got, want)
						for _, line := range missing[:min(3, len(missing))] {
							t.Logf("missing: %s", line)
						}
						for _, line := range extra[:min(3, len(extra))] {
							t.Logf("extra: %s", line)
						}
						t.Fatalf("graph differs: %d missing, %d extra", len(missing), len(extra))
					}
					t.Logf("%d facts, %d edges; graph diff: 0 missing, 0 extra; LSP %s vs SCIP %s", len(fresh.Facts), len(fresh.EdgeFacts), elapsed, time.Since(started))
					base = baseFor(current, incremental)
				})
			}
		})
	}
}
