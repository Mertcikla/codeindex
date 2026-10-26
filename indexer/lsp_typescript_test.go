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

func TestTypeScriptBodyEdit(t *testing.T) {
	source := func(code string) *graph.Source {
		return &graph.Source{Path: "a.ts", Language: "typescript", Text: []byte(code)}
	}
	for _, tc := range []struct {
		name, before, after string
		ok                  bool
	}{
		{"function", "export function A():number{return B()}\nfunction B():number{return 1}", "export function A():number{\nreturn B()+1\n}\nfunction B():number{return 1}", true},
		{"arrow", "export const A = (): number => B()", "export const A = (): number => B()+1", true},
		{"method", "class C { run():number {return 1} }", "class C { run():number {return 2} }", true},
		{"inference", "function A(){return 1}", "function A(){return 'changed'}", false},
		{"signature", "function A():number{return 1}", "function A():string{return 'changed'}", false},
		{"constructor", "class A { constructor(){B()} }", "class A { constructor(){C()} }", false},
		{"object shape", "function A():object{return {x:1}}", "function A():object{return {y:1}}", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := typeScriptBodyEdit(source(tc.before), source(tc.after))
			if (err == nil) != tc.ok {
				t.Fatalf("eligible=%v want %v: %v", err == nil, tc.ok, err)
			}
		})
	}
}

func TestTypeScriptLSPSemanticParity(t *testing.T) {
	if os.Getenv("CODEINDEX_TEST_TS_LSP") == "" {
		t.Skip("set CODEINDEX_TEST_TS_LSP=1 for real TypeScript LSP/SCIP parity")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	if sdk := os.Getenv("CODEINDEX_TEST_TS_WORKSPACE_SDK"); sdk != "" {
		if err := os.MkdirAll(filepath.Join(root, "node_modules"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(sdk, filepath.Join(root, "node_modules", "typescript")); err != nil {
			t.Fatal(err)
		}
	}
	put := func(name, code string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(code), 0600); err != nil {
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
	put("package.json", `{"name":"typescript-lsp-fixture","version":"1.0.0"}`)
	put("tsconfig.json", `{"compilerOptions":{"strict":true,"target":"ES2022","module":"commonjs","jsx":"preserve"},"include":["*.ts","*.tsx"]}`)
	api := "export interface Service { call(a:number):number }\nexport class Worker implements Service { constructor(public value:number){}; call(a:number):number{return this.value+a} }\nexport function help(a:number):number{return a+1}\nexport function other(a:number):number{return a*2}\nexport function overloaded(a:number):number;\nexport function overloaded(a:string):number;\nexport function overloaded(a:number|string):number{return typeof a==='number'?a:a.length}\n"
	original := "import {help as renamed, other, Worker, overloaded} from './api'\nexport function run(a:number):number { return renamed(a)+overloaded(a)+new Worker(a).call(a) }\nexport function text(a:string):string { return a.toUpperCase() }\nexport const arrow = (a:number):number => renamed(a)\nexport class Sub extends Worker { override call(a:number):number { return super.call(a) } }\nexport class Secret { #value=1; read():number { return this.#value } }\nexport class Numeric { 0():number { return 1 }; read():number { return this[0]() } }\n"
	put("api.ts", api)
	put("main.ts", original)
	put("jsx-types.d.ts", "export {}; declare global { namespace JSX { interface Element {} interface IntrinsicElements { span: {title?: string} } } }\n")
	view := "import {help,other} from './api';\nexport function View(a:number):JSX.Element { return <span title='label'>{help(a)}</span> }\n"
	put("view.tsx", view)
	git("init", "-q")
	git("add", ".")
	git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.com", "commit", "-qm", "base")
	cfg := config.Default()
	session := NewWatchSession(ctx, cfg, root)
	session.compiler.close()
	session.compiler = nil
	defer session.Close()
	pipeline := Pipeline{Config: cfg, Semantics: session}
	req := &pb.IndexRequest{Directory: root}
	snap, g, err := pipeline.Build(ctx, req, nil)
	if err != nil || len(snap.GetWarnings()) > 0 {
		t.Fatalf("baseline: %v %v", err, snap.GetWarnings())
	}
	// This suite exercises the legacy LSP after a live baseline is established.
	snap.Provenance = "working_tree"
	base := baseFor(snap, g)
	for _, tc := range []struct {
		name, source string
		fast         bool
		view         string
	}{
		{"changed target", strings.Replace(original, "return renamed(a)+", "return other(a)+", 1), true, ""},
		{"shift and unicode", strings.Replace(original, "return renamed(a)+", "\nvoid '😀'; return other(a)+", 1), true, ""},
		{"arrow", strings.Replace(original, "=> renamed(a)", "=> other(a)", 1), true, ""},
		{"local variables", strings.Replace(original, "return renamed(a)+", "const local=other(a); return local+", 1), true, ""},
		{"nested function", strings.Replace(original, "return renamed(a)+", "function local():number { return other(a) }; return local()+", 1), true, ""},
		{"callback", strings.Replace(original, "return renamed(a)+", "const local=():number=>other(a); return local()+", 1), true, ""},
		{"template", strings.Replace(original, "return a.toUpperCase()", "return `${a.toUpperCase()}`", 1), true, ""},
		{"super", strings.Replace(original, "return super.call(a)", "return super.call(a)+1", 1), true, ""},
		{"private field", strings.Replace(original, "return this.#value", "return this.#value+1", 1), true, ""},
		{"numeric method", strings.Replace(original, "return this[0]()", "return this[0]()+1", 1), true, ""},
		{"revert", original, true, ""},
		{"two files including TSX", strings.Replace(original, "=> renamed(a)", "=> other(a)", 1), true, strings.Replace(view, "{help(a)}", "{other(a)}", 1)},
		{"invalid", strings.Replace(original, "return renamed(a)+", "return missing(a)+", 1), false, ""},
		{"after failed edit", strings.Replace(original, "return renamed(a)+", "return other(a)+", 1), true, ""},
		{"API", strings.Replace(original, "run(a:number):number", "run(a:number):any", 1), false, ""},
		{"new external", strings.Replace(original, "toUpperCase", "toLowerCase", 1), false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			put("main.ts", tc.source)
			if tc.view != "" {
				put("view.tsx", tc.view)
			} else {
				put("view.tsx", view)
			}
			var details []string
			started := time.Now()
			current, incremental, reused, err := pipeline.BuildIncremental(ctx, req, func(p Progress) { details = append(details, p.Detail) }, base)
			if err != nil || reused || len(current.GetWarnings()) > 0 {
				t.Fatalf("incremental: %v reused=%v warnings=%v", err, reused, current.GetWarnings())
			}
			fast := strings.Contains(strings.Join(details, "\n"), "TypeScript LSP semantic refresh")
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
				t.Fatalf("tld graph differs: %d missing, %d extra", len(missing), len(extra))
			}
			if tc.fast {
				current.Provenance = "working_tree"
				base = baseFor(current, incremental)
			}
		})
	}
}

// Representative repositories are copied before editing. This uses their
// actual compiler configuration and optionally their existing node_modules;
// dependency directories are never modified or included in snapshot inputs.
func TestTypeScriptLSPRepositoryParity(t *testing.T) {
	repositories := os.Getenv("CODEINDEX_TS_LSP_REPOS")
	if repositories == "" {
		t.Skip("set CODEINDEX_TS_LSP_REPOS to representative TypeScript repositories")
	}
	for _, repo := range filepath.SplitList(repositories) {
		t.Run(filepath.Base(repo), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			root := t.TempDir()
			copyParityRepository(t, repo, root)
			cfg := config.Default()
			req := &pb.IndexRequest{Directory: root}
			git := func(args ...string) {
				t.Helper()
				cmd := exec.CommandContext(ctx, "git", args...)
				cmd.Dir = root
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git: %v %s", err, out)
				}
			}
			git("init", "-q")
			git("add", ".")
			git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.com", "commit", "-qm", "base")
			session := NewWatchSession(ctx, cfg, root)
			defer session.Close()
			pipeline := Pipeline{Config: cfg, Semantics: session}
			snap, g, err := pipeline.Build(ctx, req, nil)
			if err != nil || len(snap.GetWarnings()) > 0 {
				t.Fatalf("baseline: %v %v", err, snap.GetWarnings())
			}
			var paths []string
			for _, path := range sortedSourcePaths(g.Sources) {
				src := g.Sources[path]
				if src.Language != "typescript" || strings.HasSuffix(path, ".d.ts") {
					continue
				}
				lang := parserLanguage(src.Language)
				tree := parseDeclarationTree(lang, src.Text)
				if tree == nil {
					continue
				}
				var functions []*tsNode
				var walk func(*tsNode)
				walk = func(n *tsNode) {
					if n == nil {
						return
					}
					if n.Kind() == "function_declaration" {
						if n.ChildByFieldName("body") != nil && n.ChildByFieldName("return_type") != nil {
							functions = append(functions, n)
						}
						return
					}
					for i := 0; i < n.NamedChildCount(); i++ {
						walk(n.NamedChild(i))
					}
				}
				walk(wrapNode(tree.RootNode(), lang))
				var changed []byte
				for i, f := range functions {
					if len(functions) < 2 {
						break
					}
					other := functions[(i+1)%len(functions)]
					body := f.ChildByFieldName("body")
					at := int(body.StartByte()) + 1
					name := declarationName(other, src.Text)
					text := string(src.Text[:at]) + "\nvoid " + name + ";\n" + string(src.Text[at:])
					after := &graph.Source{Path: path, Language: src.Language, Text: []byte(text)}
					if _, err := typeScriptBodyEdit(src, after); err == nil {
						changed = after.Text
						break
					}
				}
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
				t.Fatalf("need eligible typed functions; got %v", paths)
			}
			var details []string
			started := time.Now()
			current, incremental, _, err := pipeline.BuildIncremental(ctx, req, func(p Progress) { details = append(details, p.Detail) }, baseFor(snap, g))
			elapsed := time.Since(started)
			if err != nil || len(current.GetWarnings()) > 0 || !strings.Contains(strings.Join(details, "\n"), "TypeScript LSP semantic refresh") {
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
				t.Fatalf("tld graph differs: %d missing, %d extra", len(missing), len(extra))
			}
			t.Logf("%v: %d facts / %d edges; LSP %s vs SCIP %s", paths, len(fresh.Facts), len(fresh.EdgeFacts), elapsed, time.Since(started))
		})
	}
}
