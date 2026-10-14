package graph

import (
	"encoding/hex"
	"testing"

	pb "buf.build/gen/go/tldiagramcom/diagram/protocolbuffers/go/codeindex/v1"
)

func TestEdgeLogicalKey(t *testing.T) {
	kind := pb.EdgeKind_EDGE_KIND_CALLS
	base := EdgeLogicalKey(kind, "from|symbol", "target")
	raw, err := hex.DecodeString(base)
	if err != nil || len(raw) != 32 {
		t.Fatalf("key = %q, err = %v", base, err)
	}
	for _, key := range []string{
		EdgeLogicalKey(pb.EdgeKind_EDGE_KIND_REFERENCES, "from|symbol", "target"),
		EdgeLogicalKey(kind, "other", "target"),
		EdgeLogicalKey(kind, "from|symbol", "other"),
		EdgeLogicalKey(kind, "from", "symbol|target"),
		EdgeLogicalKey(kind, "from|symbol", "target", "public-export", "./one"),
		EdgeLogicalKey(kind, "from|symbol", "target", ""),
	} {
		if base == key {
			t.Fatal("different relationship identities collided")
		}
	}
	if EdgeLogicalKey(kind, "a\x00b", "c") == EdgeLogicalKey(kind, "a", "b\x00c") {
		t.Fatal("NUL-delimited names collided")
	}
	if EdgeLogicalKey(kind, "a", "b", "public-export", "./one") == EdgeLogicalKey(kind, "a", "b", "public-export", "./two") {
		t.Fatal("public-export qualifiers collided")
	}
}

func TestEdgeKeySurvivesObservationAndAdoption(t *testing.T) {
	g := NewGraph("repo", "base")
	g.Facts["a"] = &pb.CodeFact{LogicalKey: "fact|a"}
	g.Facts["b"] = &pb.CodeFact{LogicalKey: "fact|b"}
	a := &pb.SourceAnchor{Path: "a.go", StartByte: 1, EndByte: 2}
	e := g.AddEdgeFact(pb.EdgeKind_EDGE_KIND_CALLS, "a", "b", "", a)
	other := g.AddEdgeFact(e.Kind, "a", "b", "", &pb.SourceAnchor{Path: "a.go", StartByte: 3, EndByte: 4})
	if e.Id == other.Id || e.LogicalKey != other.LogicalKey {
		t.Fatal("observation site affected logical identity")
	}
	if len(g.EdgeFacts) != 2 || g.AddEdgeFact(e.Kind, "a", "b", "", a) != e {
		t.Fatal("observation deduplication or multiplicity changed")
	}
	for _, edge := range g.EdgeFacts {
		if edge.Anchor != nil || edge.Evidence != nil {
			t.Fatal("edge emitted provenance")
		}
	}
	next := NewGraph("repo", "next")
	next.Facts["new-a"] = &pb.CodeFact{LogicalKey: "fact|a"}
	next.Facts["new-b"] = &pb.CodeFact{LogicalKey: "fact|b"}
	fresh := next.AddEdgeFact(e.Kind, "new-a", "new-b", "", a)
	if fresh.LogicalKey != e.LogicalKey {
		t.Fatal("snapshot/entity IDs affected logical identity")
	}
	e.LogicalKey = g.LogicalEdgeKey(e.Kind, "a", "fact|b", "public-export", "./entry")
	e.Weight = 2.5
	next.EdgeFacts = map[string]*pb.EdgeFact{}
	adopted := next.AdoptEdgeFact(e, "new-a", "new-b", "")
	if adopted.LogicalKey != e.LogicalKey || adopted.Weight != e.Weight {
		t.Fatal("adoption lost qualified identity or weight")
	}
	adoptedOther := next.AdoptEdgeFact(other, "new-a", "new-b", "")
	if adoptedOther.Id == adopted.Id || next.AdoptEdgeFact(e, "new-a", "new-b", "") != adopted || len(next.EdgeFacts) != 2 {
		t.Fatal("adoption collapsed distinct observations or duplicated a row")
	}
	external := next.AddEdgeFact(e.Kind, "new-a", "", "external.symbol", a)
	if external.LogicalKey != EdgeLogicalKey(e.Kind, "fact|a", "external.symbol") {
		t.Fatal("unresolved target identity changed")
	}
}
