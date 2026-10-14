package indexer

import (
	"encoding/hex"
	"testing"

	pb "buf.build/gen/go/tldiagramcom/diagram/protocolbuffers/go/codeindex/v1"
	"github.com/mertcikla/codeindex/graph"
)

func TestPackageExportKeysIncludeEntryPoint(t *testing.T) {
	build := func(snapshot string) map[string]*pb.EdgeFact {
		g := graph.NewGraph("repo", snapshot)
		for path, text := range map[string]string{
			"package.json": `{"exports":{"./one":{"import":"./index.ts","default":"./index.ts"},"./two":"./index.ts"}}`,
			"index.ts":     "export function Run() {}",
		} {
			src := &graph.Source{Path: path, Text: []byte(text), Hash: graph.Hash([]byte(text)), Language: "typescript"}
			g.Sources[path] = src
			g.AddFact(pb.FactKind_FACT_KIND_FILE, path, "typescript", src.Anchor(0, len(src.Text)), text, "", nil)
		}
		addPackageExports(g)
		from := graph.LogicalFactKey(pb.FactKind_FACT_KIND_FILE, "package.json", "package.json")
		target := graph.LogicalFactKey(pb.FactKind_FACT_KIND_FILE, "index.ts", "index.ts")
		wanted := map[string]bool{}
		for _, name := range []string{"./one", "./two"} {
			wanted[graph.EdgeLogicalKey(pb.EdgeKind_EDGE_KIND_REFERENCES, from, target, "public-export", name)] = true
		}
		out := map[string]*pb.EdgeFact{}
		for _, e := range g.EdgeFacts {
			raw, err := hex.DecodeString(e.LogicalKey)
			if err != nil || len(raw) != 32 {
				t.Fatalf("export key is not a fixed-size digest: %q", e.LogicalKey)
			}
			if !wanted[e.LogicalKey] {
				t.Fatal("export qualifier was omitted from identity")
			}
			out[e.LogicalKey] = e
		}
		return out
	}
	base, next := build("base"), build("next")
	if len(base) != 2 || len(next) != 2 {
		t.Fatalf("entry points collapsed: %d/%d", len(base), len(next))
	}
	for key, e := range base {
		if next[key] == nil || next[key].Id == e.Id || next[key].Anchor != nil || len(next[key].Evidence) != 0 || e.Anchor != nil || len(e.Evidence) != 0 {
			t.Fatal("export identity changed or provenance was retained")
		}
	}
}
