package jsonstore

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	pb "buf.build/gen/go/tldiagramcom/diagram/protocolbuffers/go/codeindex/v1"
	"github.com/mertcikla/codeindex/graph"
)

func TestIntegerSequenceSurvivesProcessExit(t *testing.T) {
	if dir := os.Getenv("CODEINDEX_TEST_EXIT_STORE"); dir != "" {
		st, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.AllocateIDs(context.Background(), 10); err != nil {
			t.Fatal(err)
		}
		os.Exit(0) // Deliberately skip Close, as an interrupted indexer would.
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestIntegerSequenceSurvivesProcessExit$")
	cmd.Env = append(os.Environ(), "CODEINDEX_TEST_EXIT_STORE="+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("helper process: %s %v", out, err)
	}
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("interrupted run left a stale lock: %v", err)
	}
	defer func() { _ = st.Close() }()
	if _, err := Open(dir); err == nil {
		t.Fatal("concurrent process lock accepted")
	}
	if first, err := st.AllocateIDs(context.Background(), 1); err != nil || first != 11 {
		t.Fatalf("sequence=%d err=%v", first, err)
	}
}

func TestPublishedExtractionUsesIntegerIDs(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	root := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	snap := &pb.Snapshot{Id: graph.ID("snapshot"), RepositoryId: graph.RepositoryID(root)}
	g := graph.NewGraph(snap.RepositoryId, snap.Id)
	src := &graph.Source{Path: "a.go", Text: []byte("func A(){}"), Hash: "hash"}
	g.Sources[src.Path] = src
	fact := g.AddFact(pb.FactKind_FACT_KIND_FUNCTION, "A", "go", src.Anchor(0, 10), "A", "", nil)
	if err := st.Publish(ctx, root, snap, g); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{snap.Id, snap.RepositoryId, fact.Id} {
		if _, err := strconv.ParseInt(id, 10, 64); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, snap.RepositoryId, snap.Id, "snapshot.json")); err != nil {
		t.Fatal(err)
	}
	repository, found, err := st.RepositoryByRoot(ctx, root)
	if err != nil || !found || repository != snap.RepositoryId {
		t.Fatalf("registration=%s %v %v", repository, found, err)
	}
}

func TestOldSnapshotsAreClearedWithoutTouchingOtherFiles(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "old-repository", "old-snapshot")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(legacy, "snapshot.json"), &pb.Snapshot{Id: "old-snapshot", RepositoryId: "old-repository"}); err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(unrelated, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("old snapshot remains: %v", err)
	}
	if raw, err := os.ReadFile(unrelated); err != nil || string(raw) != "keep" {
		t.Fatalf("unrelated file changed: %q %v", raw, err)
	}
	if snapshots, err := st.Snapshots(context.Background(), "old-repository"); err != nil || len(snapshots) != 0 {
		t.Fatalf("legacy snapshots: %v %v", snapshots, err)
	}
}
