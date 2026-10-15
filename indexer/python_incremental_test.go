package indexer

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	pb "buf.build/gen/go/tldiagramcom/diagram/protocolbuffers/go/codeindex/v1"
	"github.com/mertcikla/codeindex/config"
	"github.com/mertcikla/codeindex/graph"
	"github.com/mertcikla/codeindex/tools"
	scip "github.com/scip-code/scip/bindings/go/scip"
	"google.golang.org/protobuf/proto"
)

func TestPythonBodyChangeGuard(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 required")
	}
	before := "def work(value: str) -> str:\n    return value.strip() + 'a'\n"
	for _, tc := range []struct {
		name, after string
		safe        bool
	}{
		{"literal body", strings.Replace(before, "'a'", "'b'", 1), true},
		{"signature", strings.Replace(before, "-> str", "-> int", 1), false},
		{"inferred parameter", strings.ReplaceAll(before, "value: str", "value"), false},
		{"untyped return", strings.Replace(before, " -> str", "", 1), false},
		{"decorator", "@decorator\n" + before, false},
		{"global write", strings.Replace(before, "    return", "    global state\n    state = 1\n    return", 1), false},
		{"attribute write", strings.Replace(before, "    return", "    value.field = 1\n    return", 1), false},
		{"subscript write", strings.Replace(before, "    return", "    value[0] = 1\n    return", 1), false},
		{"nested function", strings.Replace(before, "    return", "    def nested(): pass\n    return", 1), false},
		{"import", strings.Replace(before, "    return", "    import sys\n    return", 1), false},
		{"module variable", before + "state = 1\n", false},
		{"syntax", before + "def ???", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, _ := json.Marshal([]string{before, tc.after})
			file := filepath.Join(t.TempDir(), "input.json")
			if err := os.WriteFile(file, data, 0600); err != nil {
				t.Fatal(err)
			}
			out, err := tools.Run(context.Background(), "python3", []string{"-I", "-c", pythonBodyChangeScript, file}, "")
			if err != nil || strings.TrimSpace(string(out)) != map[bool]string{true: "true", false: "false"}[tc.safe] {
				t.Fatalf("guard=%s err=%v want %v", out, err, tc.safe)
			}
		})
	}
}

func TestPythonTargetMergeBindingsAndVersions(t *testing.T) {
	ctx := context.Background()
	project := &pb.Project{Root: ".", Language: "python"}
	code := []byte("def work(value: str) -> str:\n    return value.strip() + 'b'\n")
	source := &graph.Source{Path: "worker.py", Language: "python", Text: code, Hash: graph.Hash(code)}
	g := graph.NewGraph("repo", "snapshot")
	g.Sources[source.Path] = source
	symbol := func(version string) string { return "scip-python python fixture " + version + " worker/work()." }
	doc := func(version, reference string) *scip.Document {
		return &scip.Document{RelativePath: "worker.py", Symbols: []*scip.SymbolInformation{{Symbol: symbol(version)}, {Symbol: "scip-python python python-stdlib 3.11 builtins/str#"}}, Occurrences: []*scip.Occurrence{{Symbol: symbol(version), SymbolRoles: int32(scip.SymbolRole_Definition), Range: []int32{0, 4, 8}}, {Symbol: reference, Range: []int32{1, 17, 22}}}}
	}
	const original = "scip-python python python-stdlib 3.11 builtins/str#strip()."
	baseDoc := doc("1", original)
	cached, err := compactSCIPArtifact(ctx, g, project, bytes.NewReader(marshalIndex(t, &scip.Index{Metadata: &scip.Metadata{ToolInfo: &scip.ToolInfo{Version: "0.6.6"}}, Documents: []*scip.Document{baseDoc}})))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		alter func(*scip.Document)
		fail  bool
	}{
		{"same bindings new revision", func(*scip.Document) {}, false},
		{"context-sensitive alias", func(d *scip.Document) {
			d.Occurrences[1].Symbol = "scip-python python python-stdlib 3.11 posixpath/dirname()."
		}, true},
		{"new call", func(d *scip.Document) {
			d.Occurrences = append(d.Occurrences, proto.Clone(d.Occurrences[1]).(*scip.Occurrence))
		}, true},
		{"inferred metadata", func(d *scip.Document) { d.Symbols[0].Documentation = []string{"changed inferred signature"} }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fresh := doc("2", original)
			fresh.RelativePath = ""
			tc.alter(fresh)
			artifact := filepath.Join(t.TempDir(), "target.scip")
			raw := marshalIndex(t, &scip.Index{Metadata: &scip.Metadata{ToolInfo: &scip.ToolInfo{Version: "0.6.6"}}, Documents: []*scip.Document{fresh}})
			if err := os.WriteFile(artifact, raw, 0600); err != nil {
				t.Fatal(err)
			}
			err := mergePythonArtifact(ctx, cached, artifact, project, g, "worker.py")
			if (err != nil) != tc.fail {
				t.Fatalf("merge err=%v want failure=%v", err, tc.fail)
			}
			if err == nil {
				r, e := os.Open(artifact)
				if e != nil {
					t.Fatal(e)
				}
				defer r.Close()
				docs, e := readSCIPDocuments(ctx, g, project, r)
				if e != nil || len(docs) != 1 || docs[0].RelativePath != "worker.py" || docs[0].Symbols[0].Symbol != symbol("2") {
					t.Fatalf("merged=%v err=%v", docs, e)
				}
			}
		})
	}
}

func TestPythonTargetIndexParity(t *testing.T) {
	tool := os.Getenv("CODEINDEX_TEST_SCIP_PYTHON")
	if tool == "" {
		t.Skip("set CODEINDEX_TEST_SCIP_PYTHON to scip-python 0.6.6")
	}
	ctx := context.Background()
	if toolVersion(ctx, tool, []string{"--version"}) != "0.6.6" {
		t.Fatal("need scip-python 0.6.6")
	}
	root := t.TempDir()
	put := func(name, code string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(code), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	put("pyproject.toml", "[project]\nname = 'fixture'\n")
	put("pyrightconfig.json", `{"include":["worker.py","caller.py"]}`)
	original := "def work(value: str) -> str:\n    return value.strip() + 'a'\n"
	put("worker.py", original)
	put("caller.py", "from worker import work\ndef call() -> str:\n    return work('value')\n")
	git("init", "-q")
	git("add", ".")
	git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.com", "commit", "-qm", "base")
	cfg := config.Default()
	cfg.Tools.SCIPPython = tool
	pipeline := Pipeline{Config: cfg}
	req := &pb.IndexRequest{Directory: root}
	snap, base, err := pipeline.Build(ctx, req, nil)
	if err != nil || len(snap.GetWarnings()) > 0 {
		t.Fatalf("base: %v %v", err, snap.GetWarnings())
	}
	for _, tc := range []struct {
		name, code string
		target     bool
	}{
		{"literal", strings.Replace(original, "'a'", "'b'", 1), true},
		{"new binding", strings.Replace(original, ".strip()", ".strip().lower()", 1), false},
		{"API", strings.Replace(original, "value: str", "value", 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			put("worker.py", tc.code)
			git("add", ".")
			git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.com", "commit", "-qm", tc.name)
			var details []string
			s, incremental, reused, err := pipeline.BuildIncremental(ctx, req, func(p Progress) { details = append(details, p.Detail) }, baseFor(snap, base))
			if err != nil || reused || len(s.GetWarnings()) > 0 {
				t.Fatalf("incremental %v reused=%v warnings=%v", err, reused, s.GetWarnings())
			}
			log := strings.Join(details, "\n")
			target := strings.Contains(log, "target worker.py") && !strings.Contains(log, "target refresh unavailable")
			if target != tc.target {
				t.Fatalf("target=%v want %v:\n%s", target, tc.target, log)
			}
			_, fresh, err := pipeline.Build(ctx, req, nil)
			if err != nil || !reflect.DeepEqual(normalizeGraph(t, incremental), normalizeGraph(t, fresh)) {
				t.Fatalf("parity failure: %v", err)
			}
		})
	}
}

func TestPythonTargetEligibility(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 required")
	}
	ctx := context.Background()
	pr := &pb.Project{Root: "nested", Language: "python", ConfigPath: "nested/pyproject.toml"}
	before := []byte("def work(value: str) -> str:\n    return value.strip() + 'a'\n")
	after := bytes.ReplaceAll(before, []byte("'a'"), []byte("'b'"))
	baseGraph := graph.NewGraph("repo", "base")
	baseGraph.Sources["nested/worker.py"] = &graph.Source{Path: "nested/worker.py", Language: "python", Text: before, Hash: graph.Hash(before)}
	snap := &pb.Snapshot{ConfigHash: "config", Projects: []*pb.Project{pr}, Manifest: &pb.SnapshotManifest{Complete: true, Files: []*pb.SourceFile{{Path: "nested/worker.py", Hash: graph.Hash(before), Mode: "100644"}, {Path: "nested/pyproject.toml", Hash: "config", Mode: "100644"}}}}
	current := proto.Clone(snap).(*pb.Snapshot)
	current.Manifest.Files[0].Hash = graph.Hash(after)
	g := graph.NewGraph("repo", "current")
	g.Sources["nested/worker.py"] = &graph.Source{Path: "nested/worker.py", Language: "python", Text: after, Hash: graph.Hash(after)}
	base := baseFor(snap, baseGraph)
	if got := pythonChangedTarget(ctx, pr, current, g, base, t.TempDir()); got != "worker.py" {
		t.Fatalf("target=%q", got)
	}
	for _, tc := range []struct {
		name  string
		alter func(*pb.Snapshot)
	}{
		{"config", func(s *pb.Snapshot) { s.ConfigHash = "changed" }},
		{"pyproject", func(s *pb.Snapshot) { s.Manifest.Files[1].Hash = "changed" }},
		{"file removed", func(s *pb.Snapshot) { s.Manifest.Files = s.Manifest.Files[1:] }},
		{"file added", func(s *pb.Snapshot) {
			s.Manifest.Files = append(s.Manifest.Files, &pb.SourceFile{Path: "nested/new.py"})
		}},
		{"mode", func(s *pb.Snapshot) { s.Manifest.Files[0].Mode = "100755" }},
		{"incomplete", func(s *pb.Snapshot) { s.Manifest.Complete = false }},
		{"multiple projects", func(s *pb.Snapshot) { s.Projects = append(s.Projects, &pb.Project{Root: "other", Language: "python"}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := proto.Clone(current).(*pb.Snapshot)
			tc.alter(s)
			if got := pythonChangedTarget(ctx, pr, s, g, base, t.TempDir()); got != "" {
				t.Fatalf("unsafe target=%q", got)
			}
		})
	}
	pr.ConfigPath = "nested/worker.py" // setup.py can itself be a Python source.
	if got := pythonChangedTarget(ctx, pr, current, g, base, t.TempDir()); got != "" {
		t.Fatal("project configuration file used as a body-only target")
	}
	pr.ConfigPath = "nested/pyproject.toml"

	snap.Warnings = []string{"previous project failure"}
	if got := pythonChangedTarget(ctx, pr, current, g, base, t.TempDir()); got != "" {
		t.Fatal("partial base reused")
	}
}

func TestPythonEnvironmentInvalidatesCache(t *testing.T) {
	cfg := config.Default()
	req := &pb.IndexRequest{}
	before := ConfigurationHash(cfg, req)
	t.Setenv("PYTHONPATH", os.Getenv("PYTHONPATH")+"/changed")
	if before == ConfigurationHash(cfg, req) {
		t.Fatal("Python environment did not invalidate caches")
	}
}

func TestPythonTargetRejectsChangedImportedBindings(t *testing.T) {
	ctx := context.Background()
	pr := &pb.Project{Root: ".", Language: "python"}
	g := graph.NewGraph("repo", "snapshot")
	const code = "def work(value: str) -> str:\n    return value.strip()\n"
	for _, name := range []string{"worker.py", "caller.py"} {
		g.Sources[name] = &graph.Source{Path: name, Language: "python", Text: []byte(code), Hash: graph.Hash([]byte(code))}
	}
	symbol := "scip-python python fixture 1 worker/work()."
	makeDoc := func(name string) *scip.Document {
		return &scip.Document{RelativePath: name, Symbols: []*scip.SymbolInformation{{Symbol: symbol}}, Occurrences: []*scip.Occurrence{{Symbol: symbol, SymbolRoles: int32(scip.SymbolRole_Definition), Range: []int32{0, 4, 8}}, {Symbol: "scip-python python python-stdlib 3.11 posixpath/join().", Range: []int32{1, 17, 22}}}}
	}
	metadata := &scip.Metadata{ToolInfo: &scip.ToolInfo{Version: "0.6.6"}}
	index := &scip.Index{Metadata: metadata, Documents: []*scip.Document{makeDoc("worker.py"), makeDoc("caller.py")}}
	cached, err := compactSCIPArtifact(ctx, g, pr, bytes.NewReader(marshalIndex(t, index)))
	if err != nil {
		t.Fatal(err)
	}
	index.Documents[0].RelativePath = ""
	index.Documents[1].RelativePath = "../caller.py"
	index.Documents[1].Occurrences[1].Symbol = "scip-python python python-stdlib 3.11 posixpath/dirname()."
	artifact := filepath.Join(t.TempDir(), "target.scip")
	if err := os.WriteFile(artifact, marshalIndex(t, index), 0600); err != nil {
		t.Fatal(err)
	}
	if err := mergePythonArtifact(ctx, cached, artifact, pr, g, "worker.py"); err == nil || !strings.Contains(err.Error(), "untouched document caller.py") {
		t.Fatalf("imported binding drift accepted: %v", err)
	}
}
