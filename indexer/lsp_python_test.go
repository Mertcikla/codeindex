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

func TestPythonBodyEdit(t *testing.T) {
	source := func(code string) *graph.Source {
		return &graph.Source{Path: "main.py", Language: "python", Text: []byte(code)}
	}
	for _, tc := range []struct {
		name, before, after string
		ok                  bool
	}{
		{"typed function", "def run(a: int) -> int:\n    return help(a)\n", "def run(a: int) -> int:\n    return other(a)\n", true},
		{"local binding", "def run(a: int) -> int:\n    return help(a)\n", "def run(a: int) -> int:\n    value=other(a)\n    return value\n", true},
		{"return inference", "def run(a: int):\n    return 1\n", "def run(a: int):\n    return 2\n", false},
		{"parameter inference", "def run(a) -> int:\n    return 1\n", "def run(a) -> int:\n    return 2\n", false},
		{"docstring", "def run(a: int) -> int:\n    \"\"\"old\"\"\"\n    return 1\n", "def run(a: int) -> int:\n    \"\"\"new\"\"\"\n    return 2\n", false},
		{"global write", "def run(a: int) -> int:\n    return 1\n", "def run(a: int) -> int:\n    global value\n    value=a\n    return value\n", false},
		{"attribute write", "def run(a: int) -> int:\n    return 1\n", "def run(a: int) -> int:\n    a.value=1\n    return 2\n", false},
		{"annotated local", "def run(a: int) -> int:\n    return 1\n", "def run(a: int) -> int:\n    value: int=a\n    return value\n", false},
		{"nested function", "def run(a: int) -> int:\n    return 1\n", "def run(a: int) -> int:\n    def local() -> int:\n        return a\n    return local()\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := pythonBodyEdit(source(tc.before), source(tc.after))
			if (err == nil) != tc.ok {
				t.Fatalf("eligible=%v want %v: %v", err == nil, tc.ok, err)
			}
		})
	}
}

func TestPythonLSPSemanticParity(t *testing.T) {
	testPythonLSPSemanticParity(t, false)
}

func TestPythonLSPNestedProjectParity(t *testing.T) { testPythonLSPSemanticParity(t, true) }

func testPythonLSPSemanticParity(t *testing.T, nested bool) {
	t.Helper()
	if os.Getenv("CODEINDEX_TEST_PYRIGHT") == "" {
		t.Skip("set CODEINDEX_TEST_PYRIGHT=1 for real Pyright/SCIP parity")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	projectDir := root
	if nested {
		projectDir = filepath.Join(root, "service")
		if err := os.MkdirAll(projectDir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	put := func(name, code string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(projectDir, name), []byte(code), 0600); err != nil {
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
	put("pyproject.toml", "[project]\nname=\"pyright-fixture\"\n")
	put("pyrightconfig.json", `{"include":["*.py"],"pythonVersion":"3.11","typeCheckingMode":"basic"}`)
	api := "def help(a: int) -> int:\n    return a+1\n\ndef other(a: int) -> int:\n    return a*2\n\nclass Worker:\n    value: int = 1\n    def call(self, a: int) -> int:\n        return help(a)+self.value\n"
	original := "from api import help as renamed, other, Worker\n\ndef run(a: int) -> int:\n    \"\"\"Preserved function documentation.\"\"\"\n    return renamed(a)\n\ndef method(a: int) -> int:\n    worker=Worker()\n    return worker.call(a)\n\ndef text(a: str) -> str:\n    return a.upper()\n\ndef inspect(a: object) -> bool:\n    return isinstance(a, str)\n"
	put("api.py", api)
	put("main.py", original)
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
		{"changed target", strings.Replace(original, "return renamed(a)", "return other(a)", 1), api, true},
		{"Unicode", strings.Replace(original, "return renamed(a)", "value='😀'; return other(a)", 1), api, true},
		{"local binding", strings.Replace(original, "return renamed(a)", "value=other(a)\n    return value", 1), api, true},
		{"lambda", strings.Replace(original, "return renamed(a)", "value=lambda x: other(x)\n    return value(a)", 1), api, true},
		{"comprehension", strings.Replace(original, "return renamed(a)", "values=[other(x) for x in (a,)]\n    return values[0]", 1), api, true},
		{"keyword arguments", strings.Replace(original, "return renamed(a)", "return other(a=a)", 1), api, true},
		{"method", strings.Replace(original, "return worker.call(a)", "return worker.call(a)+other(a)", 1), api, true},
		{"known external", strings.Replace(original, "return a.upper()", "return a.upper()+'!'", 1), api, true},
		{"formatted string", strings.Replace(original, "return a.upper()", `return f"{a.upper()}"`, 1), api, true},
		{"pruned builtin", strings.Replace(original, "return isinstance(a, str)", "return not isinstance(a, str)", 1), api, true},
		{"two files", strings.Replace(original, "return renamed(a)", "return other(a)", 1), strings.Replace(api, "return help(a)+self.value", "return other(a)+self.value", 1), true},
		{"revert", original, api, true},
		{"invalid", strings.Replace(original, "return renamed(a)", "return missing(a)", 1), api, false},
		{"after failed edit", strings.Replace(original, "return renamed(a)", "return other(a)", 1), api, true},
		{"docstring", strings.Replace(original, "Preserved function documentation.", "Changed documentation.", 1), api, false},
		{"inferred API", strings.Replace(original, "def run(a: int) -> int:", "def run(a: int):", 1), api, false},
		{"new external", strings.Replace(original, "a.upper()", "a.lower()", 1), api, false},
		{"new revision", strings.Replace(original, "return renamed(a)", "return other(a)+1", 1), api, false},
	}
	if nested {
		cases = cases[:1]
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			put("main.py", tc.source)
			put("api.py", tc.api)
			if tc.name == "new revision" {
				git("add", ".")
				git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.com", "commit", "-qm", "new revision")
			}
			var details []string
			started := time.Now()
			current, incremental, reused, err := pipeline.BuildIncremental(ctx, req, func(p Progress) { details = append(details, p.Detail) }, base)
			if err != nil || reused || len(current.GetWarnings()) > 0 {
				t.Fatalf("incremental: %v reused=%v warnings=%v", err, reused, current.GetWarnings())
			}
			fast := strings.Contains(strings.Join(details, "\n"), "Python Pyright semantic refresh")
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

// Representative inputs are copied and gain real references in one/two files.
func TestPythonLSPRepositoryParity(t *testing.T) {
	repositories := os.Getenv("CODEINDEX_PYRIGHT_REPOS")
	if repositories == "" {
		t.Skip("set CODEINDEX_PYRIGHT_REPOS to representative Python repositories")
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
				if src.Language != "python" {
					continue
				}
				lang := parserLanguage("python")
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
					if n.Kind() == "class_definition" || n.Kind() == "decorated_definition" {
						return
					}
					if n.Kind() == "function_definition" {
						body, name := n.ChildByFieldName("body"), n.ChildByFieldName("name")
						if body == nil || name == nil || n.ChildByFieldName("type_parameters") != nil {
							return
						}
						active := false
						for _, f := range g.Facts {
							if f.Anchor != nil && f.Anchor.Path == path && f.SymbolKey != "" && f.Name == string(src.Text[name.StartByte():name.EndByte()]) && f.Anchor.StartByte <= name.StartByte() && f.Anchor.EndByte >= name.EndByte() {
								active = true
								break
							}
						}
						if !active {
							return
						}
						_, column := src.Position(int(body.StartByte()))
						indent := strings.Repeat(" ", int(column))
						at := int(body.StartByte())
						injection := "_ = " + string(src.Text[name.StartByte():name.EndByte()]) + "\n" + indent
						first := body.NamedChild(0)
						if first != nil && first.Kind() == "expression_statement" {
							first = first.NamedChild(0)
						}
						if first != nil && (first.Kind() == "string" || first.Kind() == "concatenated_string") {
							at = int(first.EndByte())
							injection = "\n" + indent + "_ = " + string(src.Text[name.StartByte():name.EndByte()])
						}
						text := string(src.Text[:at]) + injection + string(src.Text[at:])
						after := &graph.Source{Path: path, Language: "python", Text: []byte(text)}
						if _, err := pythonBodyEdit(src, after); err == nil {
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
				client, release, err := session.python.lock(ctx, session.root)
				if err != nil {
					t.Fatal(err)
				}
				err = client.Sync(filepath.Join(session.root, filepath.FromSlash(path)), src.Text)
				if err == nil {
					session.python.hashes[path] = src.Hash
					err = client.CheckPythonDocument(ctx, filepath.Join(session.root, filepath.FromSlash(path)))
				}
				release()
				if err != nil {
					t.Logf("skip baseline diagnostics %s: %v", path, err)
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
				t.Fatal("repository needs eligible annotated functions")
			}
			var details []string
			started := time.Now()
			current, incremental, _, err := pipeline.BuildIncremental(ctx, req, func(p Progress) { details = append(details, p.Detail) }, baseFor(snap, g))
			elapsed := time.Since(started)
			if err != nil || len(current.GetWarnings()) > 0 || !strings.Contains(strings.Join(details, "\n"), "Python Pyright semantic refresh") {
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
