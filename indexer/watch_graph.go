package indexer

import (
	pb "buf.build/gen/go/tldiagramcom/diagram/protocolbuffers/go/codeindex/v1"
	"github.com/mertcikla/codeindex/graph"
	"google.golang.org/protobuf/proto"
)

// A guarded body refresh proves that declaration bindings outside edited documents
// are unchanged. Carry their fully enriched facts and observations, remapping
// incoming edges to the edited declarations' new immutable identities.
// API/configuration/membership changes and failed semantic refreshes import normally.
type watchGraphPatch struct {
	base  *graph.Graph
	kept  map[string]bool
	remap map[string]string
}

func syntaxFact(f *pb.CodeFact) bool {
	for _, e := range f.Evidence {
		if e.GetProducer() == "tree-sitter" {
			return true
		}
	}
	return false
}

func prepareWatchGraphPatch(base *IncrementalBase, g *graph.Graph, kept map[string]bool) *watchGraphPatch {
	patch := &watchGraphPatch{base: base.Graph, kept: kept, remap: map[string]string{}}
	current := map[string]*pb.CodeFact{}
	for _, f := range g.Facts {
		if current[f.LogicalKey] != nil {
			return nil
		}
		current[f.LogicalKey] = f
	}
	for id, f := range base.Graph.Facts {
		if !syntaxFact(f) {
			continue
		}
		if kept[f.GetAnchor().GetPath()] {
			continue
		}
		next := current[f.LogicalKey]
		if next == nil || next.Kind != f.Kind || next.Language != f.Language {
			return nil
		}
		patch.remap[id] = next.Id
	}
	return patch
}

func (p *watchGraphPatch) seed(g *graph.Graph, table *symbols) {
	table.skip = p.kept
	for id, f := range p.base.Facts {
		if !p.kept[f.GetAnchor().GetPath()] || !syntaxFact(f) {
			continue
		}
		c := proto.Clone(f).(*pb.CodeFact)
		c.SnapshotId = g.SnapshotID
		g.Facts[id] = c
		// One syntax declaration can receive several definition symbols, such as
		// an embedded field. Preserve every binding and its original source site.
		for _, e := range f.Evidence {
			if e.GetProducer() == "scip" && e.GetDerivation() == "matched declaration" && e.GetAnchor() != nil {
				table.define(symbolKey(e.OriginalId, e.Anchor.Path), id, e.Anchor)
			}
		}
	}
}

func (p *watchGraphPatch) carryEdges(g *graph.Graph) {
	for id, e := range p.base.EdgeFacts {
		if g.Facts[e.FromFactId] == nil {
			continue
		}
		from := p.base.Facts[e.FromFactId]
		if from == nil || !p.kept[from.GetAnchor().GetPath()] || !syntaxFact(from) {
			continue
		}
		to := e.ToFactId
		if next, ok := p.remap[to]; ok {
			to = next
		}
		if to != "" && g.Facts[to] == nil {
			continue
		}
		g.EdgeFacts[id] = &pb.EdgeFact{Id: id, RepositoryId: g.RepositoryID, SnapshotId: g.SnapshotID, Kind: e.Kind, FromFactId: e.FromFactId, ToFactId: to, TargetSymbolKey: e.TargetSymbolKey, LogicalKey: e.LogicalKey, Weight: e.Weight}
	}
}
