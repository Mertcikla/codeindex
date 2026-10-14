package symbols

import (
	"testing"

	pb "buf.build/gen/go/tldiagramcom/diagram/protocolbuffers/go/codeindex/v1"
	"github.com/mertcikla/codeindex/graph"
	"google.golang.org/protobuf/proto"
)

func fixture(version string) (*graph.Graph, *pb.CodeFact) {
	symbol := "scip-go gomod fixture " + version + " pkg/A()."
	f := &pb.CodeFact{SymbolKey: symbol, QualifiedName: symbol,
		Anchor:   &pb.SourceAnchor{Path: "a.go", SourceHash: "source", StartByte: 2},
		Evidence: []*pb.Evidence{{Producer: "scip", OriginalId: symbol}}}
	g := graph.NewGraph("repo", "snapshot")
	g.Facts["a"] = f
	return g, f
}

func TestVersionsNormalizeAndRestoreWithoutMutatingInput(t *testing.T) {
	g1, f1 := fixture("v1")
	before := proto.Clone(f1)
	first := ForFingerprint(f1, Versions(g1))
	if !proto.Equal(before, f1) || first == f1 || first.SymbolKey == f1.SymbolKey {
		t.Fatal("normalization must copy the fact and replace only version metadata")
	}
	g2, f2 := fixture("v2")
	second := ForFingerprint(f2, Versions(g2))
	if !proto.Equal(first, second) {
		t.Fatal("package version changed the reusable payload")
	}
	Restore(first, Versions(g2))
	if !proto.Equal(first, f2) {
		t.Fatal("snapshot versions did not restore the original fact metadata")
	}
}

func TestVersionBoundaries(t *testing.T) {
	g, f := fixture("v2")
	var payloads []*pb.CodeFact
	for _, version := range []string{"v1", "v2"} {
		f.Evidence = []*pb.Evidence{{Producer: "scip", OriginalId: "scip-go gomod dependency " + version + " pkg/Other()."}}
		payloads = append(payloads, ForFingerprint(f, Versions(g)))
	}
	if proto.Equal(payloads[0], payloads[1]) {
		t.Fatal("different dependency versions were merged")
	}
	original := "scip-go gomod fixture v1 pkg/Other()."
	f.Evidence = []*pb.Evidence{{Producer: "scip", OriginalId: original}}
	versions := Versions(g)
	for key := range versions {
		versions[key] = "v3"
	}
	Restore(f, versions)
	if f.Evidence[0].OriginalId != original {
		t.Fatal("restoration rewrote explicit provenance for another version")
	}
	_, a := fixture("v1")
	_, b := fixture("v2")
	g.Facts["a"], g.Facts["b"] = a, b
	versions = Versions(g)
	if len(versions) != 0 || proto.Equal(ForFingerprint(a, versions), ForFingerprint(b, versions)) {
		t.Fatal("ambiguous package versions were merged")
	}
	for _, value := range []string{"", "local 1", "plain.application.Name"} {
		f := &pb.CodeFact{SymbolKey: value, QualifiedName: value}
		if !proto.Equal(ForFingerprint(f, nil), f) {
			t.Fatal("non-package metadata was changed")
		}
	}
}
