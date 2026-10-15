package indexer

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	pb "buf.build/gen/go/tldiagramcom/diagram/protocolbuffers/go/codeindex/v1"
	"github.com/mertcikla/codeindex/config"
	"github.com/mertcikla/codeindex/graph"
	"google.golang.org/protobuf/proto"
)

func TestGoBodyOnlyChange(t *testing.T) {
	before := "package worker\nimport \"fmt\"\n// Work does work.\nfunc Work(a int) int { fmt.Println(a); return a + 1 }\n"
	for _, tc := range []struct {
		name, after string
		eligible    bool
	}{
		{"body", strings.Replace(before, "a + 1", "a + 2", 1), true},
		{"multiline body", strings.Replace(before, "fmt.Println(a); return a + 1", "\nfmt.Println(a)\nreturn a + 2\n", 1), true},
		{"docs", strings.Replace(before, "does work", "does other work", 1), true},
		{"signature", strings.ReplaceAll(before, "int", "int64"), false},
		{"import", strings.Replace(before, "\"fmt\"", "f \"fmt\"", 1), false},
		{"global", before + "var Default = 1\n", false},
		{"local type", strings.Replace(before, "fmt.Println(a);", "type Local int; fmt.Println(a);", 1), false},
		{"build constraint", "//go:build linux\n\n" + before, false},
		{"directive", strings.Replace(before, "// Work", "//go:noinline\n// Work", 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := goBodyOnlyChange([]byte(before), []byte(tc.after)); got != tc.eligible {
				t.Fatalf("eligible=%v, want %v", got, tc.eligible)
			}
		})
	}
}

// Real-tool parity covers both changed and untouched packages, cross-package
// implementation links, and a new module version between snapshots.
func TestGoPackageIndexParity(t *testing.T) {
	tool := os.Getenv("CODEINDEX_TEST_SCIP_GO")
	if tool == "" {
		t.Skip("set CODEINDEX_TEST_SCIP_GO to scip-go v0.2.7")
	}
	ctx := context.Background()
	if version := toolVersion(ctx, tool, []string{"--version"}); version != "0.2.7" {
		t.Fatalf("need scip-go 0.2.7, got %q", version)
	}
	root := t.TempDir()
	put := func(name, code string) {
		t.Helper()
		full := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(code), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	put("go.mod", "module example.com/fixture\n\ngo 1.26\n")
	put("contract/contract.go", "package contract\ntype Worker interface { Work(int) int }\n")
	put("api/api.go", "package api\ntype Service interface { Call(int) int }\nvar API Service\n")
	original := "package worker\nimport \"example.com/fixture/api\"\ntype Worker struct{}\n// Work adds one.\nfunc (Worker) Work(a int) int { return api.API.Call(a + 1) }\n"
	put("worker/worker.go", original)
	put("caller/caller.go", "package caller\nimport \"example.com/fixture/worker\"\nfunc Call() int { return (worker.Worker{}).Work(1) }\n")
	// Test-only source must never produce indexed facts.
	put("worker/worker_test.go", "package worker\nfunc TestOnly() {}\n")
	git("init", "-q")
	git("add", ".")
	git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.com", "commit", "-qm", "base")
	git("tag", "v0.0.1")
	cfg := config.Default()
	cfg.Tools.SCIPGo = tool
	pipeline := Pipeline{Config: cfg}
	req := &pb.IndexRequest{Directory: root}
	snap, base, err := pipeline.Build(ctx, req, nil)
	if err != nil || len(snap.GetWarnings()) != 0 {
		t.Fatalf("base: %v %v", err, snap.GetWarnings())
	}
	cases := []struct {
		name, source string
		partial      bool
	}{
		{"body", strings.Replace(original, "a + 1", "a + 2", 1), true},
		{"docs", strings.Replace(original, "adds one", "adds two", 1), true},
		{"declaration", original + "func Added() {}\n", false},
		{"invalid body", strings.Replace(original, "a + 1", "missing + 1", 1), false},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			put("worker/worker.go", tc.source)
			git("add", ".")
			git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.com", "commit", "-qm", tc.name)
			git("tag", []string{"v0.0.2", "v0.0.3", "v0.0.4", "v0.0.5"}[i])
			var progress []string
			_, incremental, reused, err := pipeline.BuildIncremental(ctx, req, func(p Progress) { progress = append(progress, p.Detail) }, baseFor(snap, base))
			if err != nil || reused {
				t.Fatalf("incremental err=%v reused=%v", err, reused)
			}
			details := strings.Join(progress, "\n")
			partial := strings.Contains(details, "packages ./worker") && !strings.Contains(details, "refresh unavailable")
			if partial != tc.partial {
				t.Fatalf("partial=%v want %v:\n%s", partial, tc.partial, details)
			}
			freshSnap, fresh, err := pipeline.Build(ctx, req, nil)
			if err != nil || len(freshSnap.GetWarnings()) != 0 {
				t.Fatalf("fresh: %v %v", err, freshSnap.GetWarnings())
			}
			got, want := normalizeGraph(t, incremental), normalizeGraph(t, fresh)
			if !reflect.DeepEqual(got, want) {
				dir := t.TempDir()
				_ = os.WriteFile(filepath.Join(dir, "partial.jsonl"), []byte(strings.Join(got, "\n")), 0600)
				_ = os.WriteFile(filepath.Join(dir, "full.jsonl"), []byte(strings.Join(want, "\n")), 0600)
				t.Fatalf("facts/edges differ (%d vs %d); artifacts in %s", len(got), len(want), dir)
			}
		})
	}
	t.Run("corrupt cached artifact", func(t *testing.T) {
		put("worker/worker.go", strings.Replace(original, "a + 1", "a + 3", 1))
		broken := *base
		broken.ProjectArtifacts = map[string]graph.ProjectArtifact{}
		for key, artifact := range base.ProjectArtifacts {
			artifact.Data = []byte("invalid SCIP cache")
			broken.ProjectArtifacts[key] = artifact
		}
		var details []string
		snapshot, incremental, _, err := pipeline.BuildIncremental(ctx, req, func(p Progress) { details = append(details, p.Detail) }, baseFor(snap, &broken))
		if err != nil || len(snapshot.GetWarnings()) != 0 || !strings.Contains(strings.Join(details, "\n"), "refresh unavailable") {
			t.Fatalf("expected full-scan fallback: %v %v %v", err, snapshot.GetWarnings(), details)
		}
		_, fresh, err := pipeline.Build(ctx, req, nil)
		if err != nil || !reflect.DeepEqual(normalizeGraph(t, incremental), normalizeGraph(t, fresh)) {
			t.Fatalf("fallback differs from full scan: %v", err)
		}
	})

}

func TestGoEnvironmentInvalidatesCache(t *testing.T) {
	cfg := config.Default()
	req := &pb.IndexRequest{}
	before := ConfigurationHash(cfg, req)
	t.Setenv("GOFLAGS", os.Getenv("GOFLAGS")+" -tags=changed")
	if before == ConfigurationHash(cfg, req) {
		t.Fatal("build flags did not invalidate caches")
	}
	envFile := filepath.Join(t.TempDir(), "env")
	t.Setenv("GOENV", envFile)
	if err := os.WriteFile(envFile, []byte("GOFLAGS=-tags=one\n"), 0600); err != nil {
		t.Fatal(err)
	}
	before = ConfigurationHash(cfg, req)
	if err := os.WriteFile(envFile, []byte("GOFLAGS=-tags=two\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if before == ConfigurationHash(cfg, req) {
		t.Fatal("persistent Go environment did not invalidate caches")
	}
}

func TestGoChangedPackagesSafety(t *testing.T) {
	t.Setenv("GOPACKAGESDRIVER", "")
	project := &pb.Project{Root: ".", ConfigPath: "go.mod", Language: "go"}
	before := []byte("package worker\nfunc Work() int { return 1 }\n")
	after := []byte("package worker\nfunc Work() int { return 2 }\n")
	manifest := func(files ...*pb.SourceFile) *pb.SnapshotManifest {
		return &pb.SnapshotManifest{Complete: true, Files: files}
	}
	source := func(path string, data []byte) *graph.Source {
		return &graph.Source{Path: path, Language: "go", Text: data, Hash: graph.Hash(data)}
	}
	initial := manifest(&pb.SourceFile{Path: "worker/work.go", Hash: graph.Hash(before), Mode: "100644"}, &pb.SourceFile{Path: "go.mod", Hash: "module", Mode: "100644"})
	baseSnapshot := &pb.Snapshot{Manifest: initial, ConfigHash: "config", Projects: []*pb.Project{project}}
	baseGraph := graph.NewGraph("repo", "base")
	baseGraph.Sources["worker/work.go"] = source("worker/work.go", before)
	base := baseFor(baseSnapshot, baseGraph)
	current := proto.Clone(baseSnapshot).(*pb.Snapshot)
	current.Manifest.Files[0].Hash = graph.Hash(after)
	g := graph.NewGraph("repo", "current")
	g.Sources["worker/work.go"] = source("worker/work.go", after)
	if got := goChangedPackages(project, current, g, base); !reflect.DeepEqual(got, []string{"./worker"}) {
		t.Fatalf("packages=%v", got)
	}
	for _, tc := range []struct {
		name  string
		alter func(*pb.Snapshot)
	}{
		{"config", func(s *pb.Snapshot) { s.ConfigHash = "changed" }},
		{"go.mod", func(s *pb.Snapshot) { s.Manifest.Files[1].Hash = "changed" }},
		{"removed", func(s *pb.Snapshot) { s.Manifest.Files = s.Manifest.Files[1:] }},
		{"added", func(s *pb.Snapshot) {
			s.Manifest.Files = append(s.Manifest.Files, &pb.SourceFile{Path: "worker/new.go", Hash: "new"})
		}},
		{"mode", func(s *pb.Snapshot) { s.Manifest.Files[0].Mode = "100755" }},
		{"incomplete", func(s *pb.Snapshot) { s.Manifest.Complete = false }},
		{"multi-module", func(s *pb.Snapshot) { s.Projects = append(s.Projects, &pb.Project{Language: "go", Root: "other"}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := proto.Clone(current).(*pb.Snapshot)
			tc.alter(snap)
			if got := goChangedPackages(project, snap, g, base); len(got) != 0 {
				t.Fatalf("unsafe partial: %v", got)
			}
		})
	}
	baseSnapshot.Warnings = []string{"previous failure"}
	if got := goChangedPackages(project, current, g, base); len(got) != 0 {
		t.Fatalf("reused incomplete base: %v", got)
	}
}
