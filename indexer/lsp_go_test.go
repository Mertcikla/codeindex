package indexer

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	pb "buf.build/gen/go/tldiagramcom/diagram/protocolbuffers/go/codeindex/v1"
	"github.com/mertcikla/codeindex/config"
	"github.com/mertcikla/codeindex/gitstate"
	"github.com/mertcikla/codeindex/graph"
)

func TestGoLSPEditMapping(t *testing.T) {
	source := func(text string) *graph.Source {
		return &graph.Source{Path: "a.go", Language: "go", Text: []byte(text)}
	}
	before := "package p\nfunc A() { B() }\nfunc B() {}\n"
	after := "package p\nfunc A() {\n B()\n B()\n}\nfunc B() {}\n"
	edit, err := goBodyEdit(source(before), source(after))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"func A", "func B"} {
		got, ok := edit.offset(strings.Index(before, name))
		if !ok || got != strings.Index(after, name) {
			t.Fatalf("%s: %d %v", name, got, ok)
		}
	}
	for _, changed := range []string{strings.Replace(after, "func B", "// docs\nfunc B", 1), strings.Replace(after, "B() {}", "B() int { return 0 }", 1)} {
		if _, err := goBodyEdit(source(before), source(changed)); err == nil {
			t.Fatal("accepted declaration metadata change")
		}
	}
}

// This compares every persisted tld fact and edge, not SCIP's unused metadata
// or just summary counts. Set CODEINDEX_TEST_LSP=1 to run against real tools.
func TestGoLSPSemanticParity(t *testing.T) {
	if os.Getenv("CODEINDEX_TEST_LSP") == "" {
		t.Skip("set CODEINDEX_TEST_LSP=1 for real gopls / SCIP parity")
	}
	t.Setenv("GOWORK", "off")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	put := func(name, text string) {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	put("go.mod", "module example.com/lspfixture\n\ngo 1.26\n")
	put("contract/contract.go", "package contract\ntype Worker interface { Work(int) int }\n")
	put("api/api.go", "package api\ntype Service interface { Call(int) int }\nvar API Service\nfunc Help(a int) int { return a }\n")
	original := "package worker\nimport (\"example.com/lspfixture/api\"; \"fmt\")\ntype Worker struct{}\n// Work adds one.\nfunc (Worker) Work(a int) int { fmt.Print(\"\"); return api.API.Call(a + 1) }\nfunc Other(a int) int { return a }\n"
	put("worker/worker.go", original)
	put("caller/caller.go", "package caller\nimport \"example.com/lspfixture/worker\"\nfunc Call() int { return (worker.Worker{}).Work(1) }\n")
	git("init", "-q")
	git("add", ".")
	git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.com", "commit", "-qm", "base")
	cfg := config.Default()
	session := NewWatchSession(ctx, cfg, root)
	defer session.Close()
	pipeline := Pipeline{Config: cfg, Semantics: session}
	req := &pb.IndexRequest{Directory: root}
	snap, g, err := pipeline.Build(ctx, req, nil)
	if err != nil || len(snap.GetWarnings()) > 0 {
		t.Fatalf("base: %v %v", err, snap.GetWarnings())
	}
	base := baseFor(snap, g)
	for _, tc := range []struct {
		name, text string
		fast       bool
	}{
		{"body", strings.Replace(original, "a + 1", "a + 2", 1), true},
		{"new external callee", strings.Replace(original, "fmt.Print", "fmt.Println", 1), true},
		{"different targets and shifted anchors", strings.Replace(original, "return api.API.Call(a + 1)", "\n// 😀 utf16 and non-ASCII π\n_ = \"😀\"; return Other(api.Help(a))\n", 1), true},
		{"revert", original, true},
		{"invalid", strings.Replace(original, "a + 1", "unknown + 1", 1), false},
		{"revert after failure", strings.Replace(original, "a + 1", "a + 3", 1), true},
		{"API change", original + "func Added() {}\n", false},
		{"docs", strings.Replace(original, "adds one", "adds two", 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			put("worker/worker.go", tc.text)
			var details []string
			started := time.Now()
			state, captureErr := gitstate.CaptureQuick(ctx, root)
			if captureErr != nil {
				t.Fatal(captureErr)
			}
			pipeline.WatchState = &state
			current, incremental, reused, err := pipeline.BuildIncremental(ctx, req, func(p Progress) { details = append(details, p.Detail) }, base)
			if err != nil || reused || len(current.GetWarnings()) > 0 {
				t.Fatalf("incremental: %v reused=%v warnings=%v", err, reused, current.GetWarnings())
			}
			t.Logf("incremental %s", time.Since(started))
			fast := strings.Contains(strings.Join(details, "\n"), "Go LSP semantic refresh")
			if tc.fast && !strings.Contains(strings.Join(details, "\n"), "Go graph patch") {
				t.Fatalf("did not exercise graph patch: %v", details)
			}
			if fast != tc.fast {
				t.Fatalf("LSP=%v want %v:\n%s", fast, tc.fast, strings.Join(details, "\n"))
			}
			freshPipeline := Pipeline{Config: cfg}
			freshSnap, fresh, err := freshPipeline.Build(ctx, req, nil)
			if err != nil || len(freshSnap.GetWarnings()) > 0 {
				t.Fatalf("full: %v %v", err, freshSnap.GetWarnings())
			}
			got, want := normalizeGraph(t, incremental), normalizeGraph(t, fresh)
			if !reflect.DeepEqual(got, want) {
				// Keep the first mismatches visible in test output even when the
				// testing temporary directory is removed after the run.
				missing, extra := difference(want, got), difference(got, want)
				for _, line := range missing[:min(3, len(missing))] {
					t.Logf("missing: %s", line)
				}
				for _, line := range extra[:min(3, len(extra))] {
					t.Logf("extra: %s", line)
				}
				t.Fatalf("tld graph differs: %d missing, %d extra", len(missing), len(extra))
			}
			if tc.name != "invalid" && tc.name != "API change" && tc.name != "docs" {
				base = baseFor(current, incremental)
			}
		})
	}
}

func difference(a, b []string) []string {
	set := map[string]bool{}
	for _, line := range b {
		set[line] = true
	}
	var out []string
	for _, line := range a {
		if !set[line] {
			out = append(out, line)
		}
	}
	return out
}

// CODEINDEX_LSP_REPOS names representative local repositories, separated by the
// platform path-list separator. Tests copy them before editing; originals are
// never touched. The two-file delta adds real dependency edges, rather than
// merely changing a constant and comparing unchanged edge counts.
func TestGoLSPRepositoryParity(t *testing.T) {
	repositories := os.Getenv("CODEINDEX_LSP_REPOS")
	if repositories == "" {
		t.Skip("set CODEINDEX_LSP_REPOS to representative Go repositories")
	}
	t.Setenv("GOWORK", "off")
	for _, repo := range filepath.SplitList(repositories) {
		t.Run(filepath.Base(repo), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			root := t.TempDir()
			copyParityRepository(t, repo, root)
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
			snap, g, err := pipeline.Build(ctx, req, nil)
			if err != nil || len(snap.GetWarnings()) > 0 {
				t.Fatalf("base: %v %v", err, snap.GetWarnings())
			}
			var changed []string
			for _, path := range sortedSourcePaths(g.Sources) {
				src := g.Sources[path]
				if src.Language != "go" {
					continue
				}
				set := token.NewFileSet()
				file, err := parser.ParseFile(set, path, src.Text, 0)
				if err != nil {
					t.Fatal(err)
				}
				var functions []*ast.FuncDecl
				for _, decl := range file.Decls {
					if f, ok := decl.(*ast.FuncDecl); ok && f.Recv == nil && f.Body != nil && f.Type.TypeParams == nil && f.Name.Name != "init" && f.Name.Name != "_" {
						functions = append(functions, f)
					}
				}
				if len(functions) < 2 {
					continue
				}
				offset := set.Position(functions[0].Body.Lbrace).Offset + 1
				text := string(src.Text[:offset]) + "\n_ = " + functions[1].Name.Name + "\n" + string(src.Text[offset:])
				if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(path)), []byte(text), 0600); err != nil {
					t.Fatal(err)
				}
				changed = append(changed, path)
				if len(changed) == 2 {
					break
				}
			}
			if len(changed) != 2 {
				t.Fatalf("need two editable files, found %v", changed)
			}
			started := time.Now()
			var details []string
			state, captureErr := gitstate.CaptureQuick(ctx, root)
			if captureErr != nil {
				t.Fatal(captureErr)
			}
			pipeline.WatchState = &state
			current, incremental, _, err := pipeline.BuildIncremental(ctx, req, func(p Progress) { details = append(details, p.Detail) }, baseFor(snap, g))
			elapsed := time.Since(started)
			if err != nil || len(current.GetWarnings()) > 0 || !strings.Contains(strings.Join(details, "\n"), "Go LSP semantic refresh") {
				t.Fatalf("LSP: %v %v %v", err, current.GetWarnings(), details)
			}
			started = time.Now()
			fullSnap, full, err := (Pipeline{Config: cfg}).Build(ctx, req, nil)
			if err != nil || len(fullSnap.GetWarnings()) > 0 {
				t.Fatalf("SCIP: %v %v", err, fullSnap.GetWarnings())
			}
			got, want := normalizeGraph(t, incremental), normalizeGraph(t, full)
			if !reflect.DeepEqual(got, want) {
				missing, extra := difference(want, got), difference(got, want)
				for _, line := range missing[:min(3, len(missing))] {
					t.Logf("missing: %s", line)
				}
				for _, line := range extra[:min(3, len(extra))] {
					t.Logf("extra: %s", line)
				}
				t.Fatalf("tld graph differs: %d missing, %d extra", len(missing), len(extra))
			}
			t.Logf("%v: %d facts, %d edges; LSP %s vs SCIP %s", changed, len(full.Facts), len(full.EdgeFacts), elapsed, time.Since(started))
		})
	}
}

func sortedSourcePaths(sources map[string]*graph.Source) []string {
	paths := make([]string, 0, len(sources))
	for path := range sources {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	return paths
}

// Never copy Git metadata: a linked worktree's .git file can point back to the
// original checkout. Running git init/add/commit on that copy would modify it.
func copyParityRepository(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Name() == ".git" {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.MkdirAll(filepath.Join(to, rel), 0700)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("parity fixture contains a nonregular file: %s", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(to, rel), data, 0600)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestParityCopyExcludesGitMetadata(t *testing.T) {
	from, to := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(from, ".git"), []byte("gitdir: /original/worktree"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(from, "a.go"), []byte("package a"), 0600); err != nil {
		t.Fatal(err)
	}
	copyParityRepository(t, from, to)
	if _, err := os.Stat(filepath.Join(to, ".git")); !os.IsNotExist(err) {
		t.Fatal("copied linked-worktree Git metadata")
	}
	if _, err := os.Stat(filepath.Join(to, "a.go")); err != nil {
		t.Fatal(err)
	}
}
