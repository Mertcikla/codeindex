package digest

import (
	"strings"
	"testing"

	pb "buf.build/gen/go/tldiagramcom/diagram/protocolbuffers/go/codeindex/v1"
)

func fact(id, path, name, hash string) *pb.CodeFact {
	return &pb.CodeFact{
		Id:         id,
		SnapshotId: "snap",
		Kind:       pb.FactKind_FACT_KIND_FUNCTION,
		Name:       name,
		Language:   "go",
		Anchor: &pb.SourceAnchor{
			Path: path, StartByte: 10, EndByte: 20,
			StartLine: 1, EndLine: 2, SourceHash: "hash",
		},
		BodyHash:   hash,
		LogicalKey: "fact|" + path + "|FACT_KIND_FUNCTION|" + name,
	}
}

func edge(id, from, to string) *pb.EdgeFact {
	return &pb.EdgeFact{
		Id: id, Kind: pb.EdgeKind_EDGE_KIND_CALLS,
		FromFactId: from, ToFactId: to, TargetSymbolKey: to,
		Anchor:     &pb.SourceAnchor{Path: "a.go", StartByte: 1, EndByte: 5, StartLine: 1, EndLine: 1},
		LogicalKey: "edge|CALLS|" + from + "|" + to,
	}
}

func snap() *pb.Snapshot {
	return &pb.Snapshot{
		Id: "snap", RepositoryId: "repo",
		Sources:  []*pb.SourceFile{{Path: "a.go", Hash: "hash", Size: 100}, {Path: "b.go", Hash: "hash2", Size: 50}},
		Projects: []*pb.Project{{Root: ".", Language: "go"}},
		Warnings: []string{"w2", "w1"},
	}
}

func TestSnapshotIsOrderIndependent(t *testing.T) {
	f1 := fact("f1", "a.go", "Alpha", "h1")
	f2 := fact("f2", "a.go", "Alpha", "h1") // same content, different id
	f3 := fact("f3", "b.go", "Beta", "h2")
	e1, e2 := edge("e1", "f1", "sym1"), edge("e2", "f2", "sym2")

	forward := Snapshot(snap(), []*pb.CodeFact{f1, f2, f3}, []*pb.EdgeFact{e1, e2})
	reverse := Snapshot(snap(), []*pb.CodeFact{f3, f2, f1}, []*pb.EdgeFact{e2, e1})
	if forward != reverse {
		t.Fatalf("digest depends on input order:\n %s\n %s", forward, reverse)
	}
}

func TestSnapshotIgnoresVolatileIdentifiers(t *testing.T) {
	a := fact("f1", "a.go", "Alpha", "h1")
	b := fact("totally-different-id", "a.go", "Alpha", "h1")

	otherSnap := &pb.Snapshot{Id: "other-snap", RepositoryId: "other-repo", Sources: snap().Sources, Projects: snap().Projects, Warnings: snap().Warnings}
	if Snapshot(otherSnap, []*pb.CodeFact{a}, nil) != Snapshot(snap(), []*pb.CodeFact{b}, nil) {
		t.Fatal("digest changed when only ids changed")
	}
}

func TestSnapshotDetectsContentChanges(t *testing.T) {
	base := Snapshot(snap(), []*pb.CodeFact{fact("f1", "a.go", "Alpha", "h1")}, nil)

	if moved := Snapshot(snap(), []*pb.CodeFact{fact("f1", "b.go", "Alpha", "h1")}, nil); moved == base {
		t.Fatal("digest unchanged for a moved anchor")
	}
	if renamed := Snapshot(snap(), []*pb.CodeFact{fact("f1", "a.go", "Beta", "h1")}, nil); renamed == base {
		t.Fatal("digest unchanged for a renamed fact")
	}
	if changed := Snapshot(snap(), []*pb.CodeFact{fact("f1", "a.go", "Alpha", "h2")}, nil); changed == base {
		t.Fatal("digest unchanged for a changed body")
	}
	if fewer := Snapshot(snap(), nil, nil); fewer == base {
		t.Fatal("digest unchanged for a missing fact")
	}
}

func TestSnapshotIsStableAcrossRepeats(t *testing.T) {
	facts := []*pb.CodeFact{fact("f1", "a.go", "Alpha", "h1"), fact("f2", "b.go", "Beta", "h3")}
	edges := []*pb.EdgeFact{edge("e1", "f1", "sym1"), edge("e2", "f2", "sym2")}
	first := Snapshot(snap(), facts, edges)
	for i := 0; i < 20; i++ {
		if got := Snapshot(snap(), facts, edges); got != first {
			t.Fatalf("digest drifted on repeat %d: %s != %s", i, got, first)
		}
	}
	if !strings.HasPrefix(first, "sha256:") {
		t.Fatalf("digest %q is not a sha256", first)
	}
}
