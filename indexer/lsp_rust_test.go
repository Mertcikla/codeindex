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
	"github.com/mertcikla/codeindex/graph"
)

func TestRustBodyEdit(t *testing.T) {
	source := func(code string) *graph.Source {
		return &graph.Source{Path: "src/lib.rs", Language: "rust", Text: []byte(code)}
	}
	for _, tc := range []struct {
		name, before, after string
		ok                  bool
	}{
		{"function", "pub fn a()->i32{b()}", "pub fn a()->i32{b()+1}", true},
		{"method", "impl A { fn a(&self)->i32{self.b()} }", "impl A { fn a(&self)->i32{self.b()+1} }", true},
		{"closure", "fn a()->i32{b()}", "fn a()->i32{let local=||b();local()}", true},
		{"signature", "fn a()->i32{1}", "fn a()->i64{1}", false},
		{"const evaluation", "const fn a()->usize{1}", "const fn a()->usize{2}", false},
		{"async", "async fn a()->i32{1}", "async fn a()->i32{2}", false},
		{"opaque return", "fn a()->impl Copy{1}", "fn a()->impl Copy{2}", false},
		{"macro", "fn a(){b()}", "fn a(){println!(\"x\");b()}", false},
		{"removed macro", "fn a(){println!(\"x\");b()}", "fn a(){b()}", false},
		{"nested item", "fn a(){b()}", "fn a(){fn nested(){b()}nested()}", false},
		{"attribute", "fn a(){b()}", "fn a(){#[cfg(test)] b()}", false},
		{"invalid", "fn a(){b()}", "fn a(){", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := rustBodyEdit(source(tc.before), source(tc.after))
			if (err == nil) != tc.ok {
				t.Fatalf("eligible=%v want %v: %v", err == nil, tc.ok, err)
			}
		})
	}
}

func TestRustLSPSemanticParity(t *testing.T) {
	testRustLSPSemanticParity(t, false)
}

func TestRustLSPOperatorParity(t *testing.T) { testRustLSPSemanticParity(t, true) }

func testRustLSPSemanticParity(t *testing.T, overloaded bool) {
	t.Helper()
	if os.Getenv("CODEINDEX_TEST_RUST_LSP") == "" {
		t.Skip("set CODEINDEX_TEST_RUST_LSP=1 for real Rust LSP/SCIP parity")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	root := t.TempDir()
	put := func(name, code string) {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(code), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	put("Cargo.toml", "[package]\nname=\"rust-lsp-fixture\"\nversion=\"0.1.0\"\nedition=\"2021\"\n")
	put(".gitignore", "target/\n")
	api := "pub fn alpha(a:i32)->i32 { a+1 }\npub fn beta(a:i32)->i32 { a*2 }\npub struct Worker { pub value:i32 }\npub trait Service { fn call(&self,a:i32)->i32; }\nimpl Service for Worker { fn call(&self,a:i32)->i32 { alpha(a)+self.value } }\n"
	original := "pub mod api;\nuse api::{alpha as renamed,beta,Worker,Service};\npub fn run(a:i32)->i32 { renamed(a) }\npub fn method(a:i32)->i32 { let worker=Worker{value:a}; worker.call(a) }\npub fn external(a:i32)->i32 { a.abs() }\npub fn external64(a:i64)->i64 { a.abs() }\n"
	if overloaded {
		original += "#[derive(Clone, Copy)] pub struct Number(pub i32);\nimpl std::ops::Add for Number { type Output=Number; fn add(self,rhs:Number)->Number { Number(self.0+rhs.0) } }\npub fn operator(a:Number,b:Number)->Number { a+b }\n"
	}
	put("src/api.rs", api)
	put("src/lib.rs", original)
	cmd := exec.CommandContext(ctx, "cargo", "generate-lockfile", "--offline")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cargo: %v %s", err, out)
	}
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
		t.Fatalf("baseline: %v %v", err, snap.GetWarnings())
	}
	base := baseFor(snap, g)
	cases := []struct {
		name, source, api string
		fast              bool
	}{
		{"changed target", strings.Replace(original, "{ renamed(a) }", "{ beta(a) }", 1), api, true},
		{"unicode", strings.Replace(original, "{ renamed(a) }", "{ let _=\"😀\";\n beta(a)+1 }", 1), api, true},
		{"locals and closure", strings.Replace(original, "{ renamed(a) }", "{ let local=||beta(a); local() }", 1), api, true},
		{"shorthand fields", strings.Replace(original, "{ renamed(a) }", "{ let value=a; let worker=Worker{value}; let Worker{value}=worker; value }", 1), api, true},
		{"match pattern", strings.Replace(original, "{ renamed(a) }", "{ match (Worker{value:a}) { Worker{value}=>value } }", 1), api, true},
		{"method", strings.Replace(original, "worker.call(a)", "worker.call(a)+beta(a)", 1), api, true},
		{"external", strings.Replace(original, "a.abs()", "a.abs()+1", 1), api, true},
		{"shared external macro", strings.ReplaceAll(original, "a.abs()", "a.abs()+1"), api, true},
		{"two files", strings.Replace(original, "{ renamed(a) }", "{ beta(a) }", 1), strings.Replace(api, "{ alpha(a)+self.value }", "{ beta(a)+self.value }", 1), true},
		{"revert", original, api, true},
		{"invalid", strings.Replace(original, "{ renamed(a) }", "{ missing(a) }", 1), api, false},
		{"after failed edit", strings.Replace(original, "{ renamed(a) }", "{ beta(a) }", 1), api, true},
		{"API", strings.Replace(original, "run(a:i32)->i32", "run(a:i32)->i64", 1), api, false},
		{"macro", strings.Replace(original, "{ renamed(a) }", "{ println!(\"changed\"); renamed(a) }", 1), api, false},
		{"new external", strings.Replace(original, "a.abs()", "a.signum()", 1), api, false},
		{"Cargo inputs", original, api, false},
	}
	if overloaded {
		cases = cases[:1]
		cases[0].name = "overloaded operator"
		cases[0].source = strings.Replace(original, "{ a+b }", "{ b+a }", 1)
		cases[0].fast = false
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "Cargo inputs" {
				put("Cargo.toml", "[package]\nname=\"rust-lsp-fixture\"\nversion=\"0.1.0\"\nedition=\"2021\"\ndescription=\"changed\"\n")
			}
			put("src/lib.rs", tc.source)
			put("src/api.rs", tc.api)
			var details []string
			started := time.Now()
			current, incremental, reused, err := pipeline.BuildIncremental(ctx, req, func(p Progress) { details = append(details, p.Detail) }, base)
			if err != nil || reused || len(current.GetWarnings()) > 0 {
				t.Fatalf("incremental: %v reused=%v warnings=%v", err, reused, current.GetWarnings())
			}
			fast := strings.Contains(strings.Join(details, "\n"), "Rust LSP semantic refresh")
			if fast != tc.fast {
				t.Fatalf("LSP=%v want %v:\n%s", fast, tc.fast, strings.Join(details, "\n"))
			}
			t.Logf("incremental %s", time.Since(started))
			freshSnap, fresh, err := (Pipeline{Config: cfg}).Build(ctx, req, nil)
			if err != nil || len(freshSnap.GetWarnings()) > 0 {
				t.Fatalf("SCIP: %v %v", err, freshSnap.GetWarnings())
			}
			got, want := normalizeGraph(t, incremental), normalizeGraph(t, fresh)
			if !reflect.DeepEqual(got, want) {
				missing, extra := difference(want, got), difference(got, want)
				for _, line := range missing[:min(3, len(missing))] {
					t.Logf("missing %s", line)
				}
				for _, line := range extra[:min(3, len(extra))] {
					t.Logf("extra %s", line)
				}
				t.Fatalf("graph differs: %d missing, %d extra", len(missing), len(extra))
			}
			if tc.fast {
				base = baseFor(current, incremental)
			}
		})
	}
}

// Copies preserve actual Cargo manifests, features and dependency resolution.
// Each edited function gains a real function reference; all persisted graph
// fields must agree with a fresh SCIP extraction after one/two-file changes.
func TestRustLSPRepositoryParity(t *testing.T) {
	repositories := os.Getenv("CODEINDEX_RUST_LSP_REPOS")
	if repositories == "" {
		t.Skip("set CODEINDEX_RUST_LSP_REPOS to representative Rust repositories")
	}
	for _, repo := range filepath.SplitList(repositories) {
		t.Run(filepath.Base(repo), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			root := t.TempDir()
			copyParityRepository(t, repo, root)
			run := func(exe string, args ...string) {
				t.Helper()
				cmd := exec.CommandContext(ctx, exe, args...)
				cmd.Dir = root
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("%s: %v %s", exe, err, out)
				}
			}
			if _, err := os.Stat(filepath.Join(root, "Cargo.lock")); os.IsNotExist(err) {
				run("cargo", "generate-lockfile")
			}
			run("git", "init", "-q")
			run("git", "add", ".")
			run("git", "-c", "user.name=Fixture", "-c", "user.email=fixture@example.com", "commit", "-qm", "base")
			cfg := config.Default()
			session := NewWatchSession(ctx, cfg, root)
			defer session.Close()
			pipeline := Pipeline{Config: cfg, Semantics: session}
			req := &pb.IndexRequest{Directory: root}
			snap, g, err := pipeline.Build(ctx, req, nil)
			if err != nil || len(snap.GetWarnings()) > 0 {
				t.Fatalf("baseline: %v %v", err, snap.GetWarnings())
			}
			var paths []string
			for _, path := range sortedSourcePaths(g.Sources) {
				src := g.Sources[path]
				if src.Language != "rust" {
					continue
				}
				lang := parserLanguage("rust")
				tree := parseDeclarationTree(lang, src.Text)
				if tree == nil {
					continue
				}
				var changed []byte
				var walk func(*tsNode)
				walk = func(n *tsNode) {
					if n == nil || changed != nil {
						return
					}
					if n.Kind() == "impl_item" || n.Kind() == "trait_item" {
						return
					}
					if n.Kind() == "function_item" {
						body, name := n.ChildByFieldName("body"), n.ChildByFieldName("name")
						if body == nil || name == nil || n.ChildByFieldName("type_parameters") != nil {
							return
						}
						active := false
						for _, f := range g.Facts {
							if f.Anchor != nil && f.Anchor.Path == path && f.Name == string(src.Text[name.StartByte():name.EndByte()]) && f.Anchor.StartByte <= name.StartByte() && f.Anchor.EndByte >= name.EndByte() {
								active = true
								break
							}
						}
						if !active {
							return
						}
						at := int(body.StartByte()) + 1
						text := string(src.Text[:at]) + "\nlet _ = " + string(src.Text[name.StartByte():name.EndByte()]) + ";\n" + string(src.Text[at:])
						after := &graph.Source{Path: path, Language: "rust", Text: []byte(text)}
						if _, err := rustBodyEdit(src, after); err == nil {
							changed = after.Text
						}
						return
					}
					for i := 0; i < n.NamedChildCount(); i++ {
						walk(n.NamedChild(i))
					}
				}
				walk(wrapNode(tree.RootNode(), lang))
				tree.Release()
				if changed == nil {
					continue
				}
				if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(path)), changed, 0600); err != nil {
					t.Fatal(err)
				}
				paths = append(paths, path)
				if len(paths) == 2 {
					break
				}
			}
			if len(paths) == 0 {
				t.Fatal("repository needs eligible non-generic functions")
			}
			var details []string
			started := time.Now()
			current, incremental, _, err := pipeline.BuildIncremental(ctx, req, func(p Progress) { details = append(details, p.Detail) }, baseFor(snap, g))
			elapsed := time.Since(started)
			if err != nil || len(current.GetWarnings()) > 0 || !strings.Contains(strings.Join(details, "\n"), "Rust LSP semantic refresh") {
				t.Fatalf("LSP: %v %v\n%s", err, current.GetWarnings(), strings.Join(details, "\n"))
			}
			started = time.Now()
			freshSnap, fresh, err := (Pipeline{Config: cfg}).Build(ctx, req, nil)
			if err != nil || len(freshSnap.GetWarnings()) > 0 {
				t.Fatalf("SCIP: %v %v", err, freshSnap.GetWarnings())
			}
			got, want := normalizeGraph(t, incremental), normalizeGraph(t, fresh)
			if !reflect.DeepEqual(got, want) {
				missing, extra := difference(want, got), difference(got, want)
				for _, line := range missing[:min(3, len(missing))] {
					t.Logf("missing %s", line)
				}
				for _, line := range extra[:min(3, len(extra))] {
					t.Logf("extra %s", line)
				}
				t.Fatalf("graph differs: %d missing, %d extra", len(missing), len(extra))
			}
			t.Logf("%v: %d facts / %d edges; LSP %s vs SCIP %s", paths, len(fresh.Facts), len(fresh.EdgeFacts), elapsed, time.Since(started))
		})
	}
}
