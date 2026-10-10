package jsonstore

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	pb "buf.build/gen/go/tldiagramcom/diagram/protocolbuffers/go/codeindex/v1"
	"github.com/mertcikla/codeindex/graph"
)

func buildGraph(t *testing.T) (*graph.Graph, *pb.Snapshot) {
	t.Helper()
	g := graph.NewGraph("repo", "snap-1")
	g.AddFact(pb.FactKind_FACT_KIND_FUNCTION, "Alpha", "go",
		&pb.SourceAnchor{Path: "a.go", StartByte: 10, EndByte: 20, StartLine: 1, EndLine: 2}, "func Alpha(){}", "func Alpha()", nil)
	g.AddFact(pb.FactKind_FACT_KIND_METHOD, "Beta", "go",
		&pb.SourceAnchor{Path: "b.go", StartByte: 5, EndByte: 15, StartLine: 1, EndLine: 1}, "func Beta(){}", "func Beta()", nil)
	g.AddInfraFact(pb.FactKind_FACT_KIND_DEPLOYABLE, "docker", "web", "dockerfile", "scanner",
		&pb.SourceAnchor{Path: "Dockerfile", StartByte: 0, EndByte: 12, StartLine: 1, EndLine: 1}, "FROM x")
	g.Sources["a.go"] = &graph.Source{Path: "a.go", Language: "go", Text: []byte("package a"), Hash: "hash-a"}
	g.Sources["b.go"] = &graph.Source{Path: "b.go", Language: "go", Text: []byte("package b"), Hash: "hash-b"}
	g.ProjectArtifacts["."] = graph.ProjectArtifact{Fingerprint: "fp", Data: []byte(`{"go":1}`)}

	from := g.Facts[graph.ID("snap-1", "fact", "a.go", "10", "20", pb.FactKind_FACT_KIND_FUNCTION.String(), "Alpha")]
	to := g.Facts[graph.ID("snap-1", "fact", "b.go", "5", "15", pb.FactKind_FACT_KIND_METHOD.String(), "Beta")]
	if g.AddEdgeFact(pb.EdgeKind_EDGE_KIND_CALLS, from.GetId(), to.GetId(), "sym",
		&pb.SourceAnchor{Path: "a.go", StartByte: 12, EndByte: 14, StartLine: 1, EndLine: 1}, nil) == nil {
		t.Fatal("edge fact rejected")
	}

	snap := &pb.Snapshot{
		Id: "snap-1", RepositoryId: "repo", IngestionStatus: "complete",
		Sources: []*pb.SourceFile{
			{Path: "a.go", Hash: "hash-a", Size: 9},
			{Path: "b.go", Hash: "hash-b", Size: 9},
		},
		Projects: []*pb.Project{{Root: ".", Language: "go"}},
	}
	return g, snap
}

func TestPublishRoundTripsThroughTheStore(t *testing.T) {
	ctx := context.Background()
	g, snap := buildGraph(t)
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := st.Publish(ctx, "/repo", snap, g); err != nil {
		t.Fatalf("publish: %v", err)
	}

	gotSnap, err := st.Snapshot(ctx, "snap-1")
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if gotSnap.GetId() != "snap-1" || len(gotSnap.GetSources()) != 2 {
		t.Fatalf("snapshot round-trip: %+v", gotSnap)
	}

	facts, err := st.Facts(ctx, "snap-1", pb.FactKind_FACT_KIND_UNSPECIFIED, "", "", 0)
	if err != nil || len(facts) != len(g.Facts) {
		t.Fatalf("facts = %d (err %v), want %d", len(facts), err, len(g.Facts))
	}
	edges, err := st.EdgeFacts(ctx, "snap-1", pb.EdgeKind_EDGE_KIND_UNSPECIFIED, "", "", 0)
	if err != nil || len(edges) != len(g.EdgeFacts) {
		t.Fatalf("edges = %d (err %v), want %d", len(edges), err, len(g.EdgeFacts))
	}

	reloaded, err := st.LoadGraph(ctx, "snap-1")
	if err != nil {
		t.Fatalf("load graph: %v", err)
	}
	if len(reloaded.Facts) != len(g.Facts) || len(reloaded.EdgeFacts) != len(g.EdgeFacts) {
		t.Fatalf("reloaded graph: %d facts, %d edges", len(reloaded.Facts), len(reloaded.EdgeFacts))
	}
	if len(reloaded.Sources) != len(g.Sources) {
		t.Fatalf("reloaded sources = %d, want %d", len(reloaded.Sources), len(g.Sources))
	}
	if _, ok := reloaded.ProjectArtifacts["."]; !ok {
		t.Fatal("reloaded project artifacts missing")
	}
	if _, ok := reloaded.Facts[graph.ID("snap-1", "fact", "a.go", "10", "20", pb.FactKind_FACT_KIND_FUNCTION.String(), "Alpha")]; !ok {
		t.Fatal("fact id not preserved across the round-trip")
	}
}

func TestPublishWritesDeterministicDocuments(t *testing.T) {
	ctx := context.Background()
	g, snap := buildGraph(t)
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	factsPath := filepath.Join(st.dir, "repo", "snap-1", "facts.json")
	for i := 0; i < 3; i++ {
		if err := st.Publish(ctx, "/repo", snap, g); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	again, err := st.Facts(ctx, "snap-1", pb.FactKind_FACT_KIND_UNSPECIFIED, "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	// Path order is a.go, b.go, Dockerfile: the documents are sorted by anchor
	// path regardless of the graph's map iteration order.
	want := []string{"Dockerfile", "a.go", "b.go"} // byte order: uppercase before lowercase
	if len(again) != len(want) {
		t.Fatalf("facts = %d, want %d", len(again), len(want))
	}
	for i, f := range again {
		if f.GetAnchor().GetPath() != want[i] {
			t.Fatalf("fact %d path = %q, want %q", i, f.GetAnchor().GetPath(), want[i])
		}
	}
	if _, err := os.Stat(factsPath); err != nil {
		t.Fatalf("facts.json missing: %v", err)
	}
}

func TestFactsFilters(t *testing.T) {
	ctx := context.Background()
	g, snap := buildGraph(t)
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := st.Publish(ctx, "/repo", snap, g); err != nil {
		t.Fatalf("publish: %v", err)
	}

	funcs, err := st.Facts(ctx, "snap-1", pb.FactKind_FACT_KIND_FUNCTION, "", "", 0)
	if err != nil || len(funcs) != 1 || funcs[0].GetName() != "Alpha" {
		t.Fatalf("kind filter = %v (err %v)", funcs, err)
	}
	prefixed, err := st.Facts(ctx, "snap-1", pb.FactKind_FACT_KIND_UNSPECIFIED, "b.", "", 0)
	if err != nil || len(prefixed) != 1 || prefixed[0].GetName() != "Beta" {
		t.Fatalf("path prefix filter = %v (err %v)", prefixed, err)
	}
	limited, err := st.Facts(ctx, "snap-1", pb.FactKind_FACT_KIND_UNSPECIFIED, "", "", 2)
	if err != nil || len(limited) != 2 {
		t.Fatalf("limit = %d (err %v)", len(limited), err)
	}
	after, err := st.Facts(ctx, "snap-1", pb.FactKind_FACT_KIND_UNSPECIFIED, "", limited[len(limited)-1].GetId(), 0)
	if err != nil || len(after) != 1 {
		t.Fatalf("after cursor = %d (err %v)", len(after), err)
	}
	if missing, err := st.Facts(ctx, "nope", pb.FactKind_FACT_KIND_UNSPECIFIED, "", "", 0); err == nil || missing != nil {
		t.Fatalf("unknown snapshot: %v (err %v)", missing, err)
	}
}

func TestPublishHistoricalLeavesLatestAlone(t *testing.T) {
	ctx := context.Background()
	g, snap := buildGraph(t)
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := st.Publish(ctx, "/repo", snap, g); err != nil {
		t.Fatalf("publish: %v", err)
	}
	latest := filepath.Join(st.dir, "repo", "latest")
	got, err := os.ReadFile(latest)
	if err != nil || string(got) != "snap-1\n" {
		t.Fatalf("latest = %q (err %v)", got, err)
	}
	old := buildGraphHistorical(t)
	if err := st.PublishHistorical(ctx, "/repo", old, g); err != nil {
		t.Fatalf("historical: %v", err)
	}
	got, err = os.ReadFile(latest)
	if err != nil || string(got) != "snap-1\n" {
		t.Fatalf("latest moved after PublishHistorical: %q (err %v)", got, err)
	}
	if snaps, err := st.Snapshots(ctx, "repo"); err != nil || len(snaps) != 2 {
		t.Fatalf("snapshots = %d (err %v), want 2", len(snaps), err)
	}
}

func buildGraphHistorical(t *testing.T) *pb.Snapshot {
	t.Helper()
	return &pb.Snapshot{Id: "snap-0", RepositoryId: "repo", IngestionStatus: "complete", Provenance: "commit"}
}

func TestIdentityQueriesDegradeForStandaloneUse(t *testing.T) {
	ctx := context.Background()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if id, ok, err := st.RepositoryByRemoteKey(ctx, "github.com/o/r"); ok || err != nil || id != "" {
		t.Fatalf("RepositoryByRemoteKey = %q, %v, %v", id, ok, err)
	}
	if err := st.EnsureRepositoryIdentity(ctx, "id", "/root", "https://github.com/o/r", "github.com/o/r", true); err != nil {
		t.Fatalf("EnsureRepositoryIdentity: %v", err)
	}
	exists, err := st.RepositoryExists(ctx, "id")
	if err != nil || exists {
		t.Fatalf("RepositoryExists = %v (err %v)", exists, err)
	}
}
