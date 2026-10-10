// Package digest computes a stable fingerprint of an indexed snapshot.
//
// The projection deliberately excludes volatile identifiers (snapshot,
// repository, fact and edge ids, all of which embed the snapshot id) and tool
// versions, so the same repository content indexed by two engines digests
// identically. Cross-snapshot identity is taken from the logical keys instead:
// every fact and edge already carries the logical key that survives
// re-indexing.
package digest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	pb "buf.build/gen/go/tldiagramcom/diagram/protocolbuffers/go/codeindex/v1"
)

type anchorProjection struct {
	Path        string `json:"path"`
	StartByte   uint32 `json:"start_byte"`
	EndByte     uint32 `json:"end_byte"`
	StartLine   uint32 `json:"start_line"`
	StartColumn uint32 `json:"start_column"`
	EndLine     uint32 `json:"end_line"`
	EndColumn   uint32 `json:"end_column"`
}

type factProjection struct {
	LogicalKey    string           `json:"logical_key"`
	Kind          string           `json:"kind"`
	Language      string           `json:"language"`
	Anchor        anchorProjection `json:"anchor"`
	Name          string           `json:"name"`
	QualifiedName string           `json:"qualified_name"`
	SymbolKey     string           `json:"symbol_key"`
	BodyHash      string           `json:"body_hash"`
	Imports       []string         `json:"imports,omitempty"`
}

type edgeProjection struct {
	LogicalKey      string           `json:"logical_key"`
	Kind            string           `json:"kind"`
	TargetSymbolKey string           `json:"target_symbol_key"`
	Anchor          anchorProjection `json:"anchor"`
	Weight          float64          `json:"weight,omitempty"`
}

type sourceProjection struct {
	Path string `json:"path"`
	Hash string `json:"hash"`
	Size uint64 `json:"size"`
}

type projectProjection struct {
	Root       string `json:"root"`
	Language   string `json:"language"`
	ConfigPath string `json:"config_path"`
}

type snapshotProjection struct {
	Sources  []sourceProjection  `json:"sources"`
	Projects []projectProjection `json:"projects"`
	Warnings []string            `json:"warnings,omitempty"`
	Facts    []factProjection    `json:"facts"`
	Edges    []edgeProjection    `json:"edges"`
}

// Snapshot digests a built snapshot and its facts and edges into a sha256
// fingerprint. Facts and edges may be passed in any order; the projection is
// sorted before hashing.
//
// The projection deliberately omits two fields that are not stable even within
// a single engine today: signature and documentation, which the tree-sitter and
// SCIP producers render differently for facts both of them observe, with the
// winner depending on processing order. The evidence chain is likewise
// provenance metadata rather than extracted content. The body hash covers the
// body, and the logical key, anchor and symbol key cover identity.
func Snapshot(snap *pb.Snapshot, facts []*pb.CodeFact, edges []*pb.EdgeFact) string {
	p := snapshotProjection{
		Sources:  []sourceProjection{},
		Projects: []projectProjection{},
		Facts:    []factProjection{},
		Edges:    []edgeProjection{},
	}
	if snap != nil {
		for _, src := range snap.Sources {
			if src == nil {
				continue
			}
			p.Sources = append(p.Sources, sourceProjection{Path: src.GetPath(), Hash: src.GetHash(), Size: src.GetSize()})
		}
		for _, prj := range snap.Projects {
			if prj == nil {
				continue
			}
			p.Projects = append(p.Projects, projectProjection{Root: prj.GetRoot(), Language: prj.GetLanguage(), ConfigPath: prj.GetConfigPath()})
		}
		p.Warnings = append(p.Warnings, snap.Warnings...)
	}
	for _, f := range facts {
		if f == nil {
			continue
		}
		p.Facts = append(p.Facts, factProjection{
			LogicalKey:    f.GetLogicalKey(),
			Kind:          f.GetKind().String(),
			Language:      f.GetLanguage(),
			Anchor:        projectAnchor(f.GetAnchor()),
			Name:          f.GetName(),
			QualifiedName: f.GetQualifiedName(),
			SymbolKey:     f.GetSymbolKey(),
			BodyHash:      f.GetBodyHash(),
			Imports:       f.GetImports(),
		})
	}
	for _, e := range edges {
		if e == nil {
			continue
		}
		p.Edges = append(p.Edges, edgeProjection{
			LogicalKey:      e.GetLogicalKey(),
			Kind:            e.GetKind().String(),
			TargetSymbolKey: e.GetTargetSymbolKey(),
			Anchor:          projectAnchor(e.GetAnchor()),
			Weight:          e.GetWeight(),
		})
	}
	sort.Slice(p.Sources, func(i, j int) bool { return p.Sources[i].Path < p.Sources[j].Path })
	sort.Slice(p.Projects, func(i, j int) bool { return p.Projects[i].Root < p.Projects[j].Root })
	sort.Strings(p.Warnings)
	sort.Slice(p.Facts, func(i, j int) bool { return factLess(&p.Facts[i], &p.Facts[j]) })
	sort.Slice(p.Edges, func(i, j int) bool { return edgeLess(&p.Edges[i], &p.Edges[j]) })

	encoded, err := json.Marshal(p)
	if err != nil {
		// The projection is plain data; a marshal failure is impossible.
		return fmt.Sprintf("sha256:%064x", sha256.Sum256(nil))
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func projectAnchor(a *pb.SourceAnchor) anchorProjection {
	if a == nil {
		return anchorProjection{}
	}
	return anchorProjection{
		Path:        a.GetPath(),
		StartByte:   a.GetStartByte(),
		EndByte:     a.GetEndByte(),
		StartLine:   a.GetStartLine(),
		StartColumn: a.GetStartColumn(),
		EndLine:     a.GetEndLine(),
		EndColumn:   a.GetEndColumn(),
	}
}

// factLess orders facts by logical key first, then by every remaining
// projected field. The full tiebreaker chain is required: facts share a
// logical key whenever the kind, name and path repeat (methods with the same
// name on different receivers, for example), and the graph stores them in a
// map whose iteration order is random.
func factLess(a, b *factProjection) bool {
	if a.LogicalKey != b.LogicalKey {
		return a.LogicalKey < b.LogicalKey
	}
	if a.Anchor.StartByte != b.Anchor.StartByte {
		return a.Anchor.StartByte < b.Anchor.StartByte
	}
	if a.Anchor.EndByte != b.Anchor.EndByte {
		return a.Anchor.EndByte < b.Anchor.EndByte
	}
	if a.Name != b.Name {
		return a.Name < b.Name
	}
	if a.SymbolKey != b.SymbolKey {
		return a.SymbolKey < b.SymbolKey
	}
	return a.BodyHash < b.BodyHash
}

// edgeLess orders edges the same way, since repeated observations of one
// logical edge collapse onto a single entry but distinct edges can share a
// logical key across anchors.
func edgeLess(a, b *edgeProjection) bool {
	if a.LogicalKey != b.LogicalKey {
		return a.LogicalKey < b.LogicalKey
	}
	if a.Anchor.StartByte != b.Anchor.StartByte {
		return a.Anchor.StartByte < b.Anchor.StartByte
	}
	if a.Anchor.EndByte != b.Anchor.EndByte {
		return a.Anchor.EndByte < b.Anchor.EndByte
	}
	return a.TargetSymbolKey < b.TargetSymbolKey
}
