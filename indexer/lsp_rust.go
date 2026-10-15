package indexer

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	pb "buf.build/gen/go/tldiagramcom/diagram/protocolbuffers/go/codeindex/v1"
	"github.com/mertcikla/codeindex/graph"
	"github.com/mertcikla/codeindex/lsp"
	scip "github.com/scip-code/scip/bindings/go/scip"
	"google.golang.org/protobuf/proto"
)

// Other versions remain on SCIP until their emitted identities pass parity.
const rustLSPVersion = "rust-analyzer 1.95.0 (59807616 2026-04-14)"

func rustLocationKey(loc lsp.Location) (string, error) {
	path, err := lsp.Path(loc.URI)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s:%d:%d", filepath.Clean(path), loc.Range.Start.Line, loc.Range.Start.Character), nil
}

// refreshRustLSP preserves declaration metadata and only replaces occurrences
// inside changed bodies. SCIP and LSP are two modes of the same rust-analyzer
// binary; captured definition locations provide the canonical identity bridge.
func (s *WatchSession) refreshRustLSP(ctx context.Context, root string, project *pb.Project, snap *pb.Snapshot, g *graph.Graph, base *IncrementalBase, cached graph.ProjectArtifact, artifact string) error {
	edits, err := rustEdits(project, snap, g, base)
	if err != nil {
		return err
	}
	reader, err := scipArtifactReader(cached.Data)
	if err != nil {
		return err
	}
	defer func() { _ = reader.Close() }()
	docs, err := readSCIPDocuments(ctx, base.Graph, project, reader)
	if err != nil {
		return err
	}
	paths := make([]string, 0, len(edits))
	for path := range edits {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	documents := map[string]*scip.Document{}
	definitions := map[string]string{}
	defined := map[string]bool{}
	locationKey := func(path string, offset int) string { return fmt.Sprintf("%s:%d", path, offset) }
	for _, doc := range docs {
		path := scipDocumentPath(project, doc.RelativePath)
		if documents[path] != nil {
			return fmt.Errorf("multiple Rust configurations for %s", path)
		}
		documents[path] = doc
		encoding, _ := scipSourceEncoding(doc, base.Graph.Sources[path])
		for _, o := range doc.Occurrences {
			if !scip.SymbolRole_Definition.Matches(o) || strings.HasPrefix(o.Symbol, "local ") {
				continue
			}
			anchor, err := scipAnchor(base.Graph.Sources[path], o, encoding)
			if err != nil {
				return err
			}
			offset := int(anchor.StartByte)
			if edit := edits[path]; edit != nil {
				if edit.containedBefore(int(anchor.StartByte), int(anchor.EndByte)) {
					return fmt.Errorf("global Rust definition inside edited body")
				}
				var ok bool
				offset, ok = edit.offset(offset)
				if !ok {
					return fmt.Errorf("cannot remap Rust definition")
				}
			}
			key := locationKey(path, offset)
			if previous := definitions[key]; previous != "" && previous != o.Symbol {
				return fmt.Errorf("ambiguous Rust definition at %s", key)
			}
			definitions[key] = o.Symbol
			defined[o.Symbol] = true
		}
	}
	for _, path := range paths {
		if documents[path] == nil {
			return fmt.Errorf("changed Rust file has no SCIP document: %s", path)
		}
	}
	// SCIP records calls/references for operator desugarings that goto-definition
	// does not expose. If a captured impl could supply such an edge, retain SCIP
	// for operator edits. Deref coercions can be implicit in any expression.
	for symbol := range defined {
		if !strings.Contains(symbol, "impl#") {
			continue
		}
		parsed, err := scip.ParseSymbol(symbol)
		if err != nil {
			return err
		}
		if len(parsed.Descriptors) == 0 {
			continue
		}
		name := parsed.Descriptors[len(parsed.Descriptors)-1].Name
		for _, edit := range edits {
			if name == "deref" || name == "deref_mut" {
				return fmt.Errorf("captured Deref implementation requires SCIP")
			}
			if edit.operatorSensitive {
				switch name {
				case "add", "sub", "mul", "div", "rem", "add_assign", "sub_assign", "mul_assign", "div_assign", "rem_assign", "bitand", "bitor", "bitxor", "shl", "shr", "bitand_assign", "bitor_assign", "bitxor_assign", "shl_assign", "shr_assign", "neg", "not", "index", "index_mut", "eq", "ne", "partial_cmp", "lt", "le", "gt", "ge", "poll", "into_future", "branch", "from_residual":
					return fmt.Errorf("captured operator implementation requires SCIP")
				}
			}
		}
	}
	client, unlock, err := s.rust.lock(ctx, root)
	if err != nil {
		return err
	}
	defer unlock()
	if err := client.WaitRustReady(ctx); err != nil {
		return err
	}
	syncSources := func(sources map[string]*graph.Source) error {
		for path := range s.rust.hashes {
			if sources[path] == nil {
				return fmt.Errorf("opened Rust source removed")
			}
		}
		for path, src := range sources {
			_, opened := s.rust.hashes[path]
			if src.Language == "rust" && (edits[path] != nil || opened) && s.rust.hashes[path] != src.Hash {
				if err := client.Sync(filepath.Join(root, filepath.FromSlash(path)), src.Text); err != nil {
					return err
				}
				s.rust.hashes[path] = src.Hash
			}
		}
		return nil
	}
	needSeed := false
	for _, path := range paths {
		encoding, _ := scipSourceEncoding(documents[path], base.Graph.Sources[path])
		for _, o := range documents[path].Occurrences {
			if scip.SymbolRole_Definition.Matches(o) || defined[o.Symbol] || s.rust.seeded[o.Symbol] {
				continue
			}
			anchor, err := scipAnchor(base.Graph.Sources[path], o, encoding)
			if err != nil {
				return err
			}
			if edits[path].containedBefore(int(anchor.StartByte), int(anchor.EndByte)) {
				if rustOperatorToken(base.Graph.Sources[path].Text[anchor.StartByte:anchor.EndByte]) {
					continue
				}
				needSeed = true
				break
			}
		}
	}
	if needSeed {
		if err := syncSources(base.Graph.Sources); err != nil {
			return err
		}
		for _, path := range paths {
			src, doc, edit := base.Graph.Sources[path], documents[path], edits[path]
			encoding, _ := scipSourceEncoding(doc, src)
			for _, o := range doc.Occurrences {
				if scip.SymbolRole_Definition.Matches(o) || defined[o.Symbol] || s.rust.seeded[o.Symbol] {
					continue
				}
				anchor, err := scipAnchor(src, o, encoding)
				if err != nil {
					return err
				}
				if !edit.containedBefore(int(anchor.StartByte), int(anchor.EndByte)) {
					continue
				}
				// External operator occurrences cannot produce tld references
				// (no captured target fact) or call-expression edges. Prune them
				// rather than trying to reconstruct compiler desugarings.
				if rustOperatorToken(src.Text[anchor.StartByte:anchor.EndByte]) {
					continue
				}
				locations, err := client.Definition(ctx, filepath.Join(root, filepath.FromSlash(path)), lspPosition(src, int(anchor.StartByte), client.Encoding))
				if err != nil {
					return err
				}
				if len(locations) == 0 {
					return fmt.Errorf("ambiguous external Rust identity (%d locations): %s", len(locations), o.Symbol)
				}
				for _, loc := range locations {
					key, err := rustLocationKey(loc)
					if err != nil {
						return err
					}
					if s.rust.external[key] == nil {
						s.rust.external[key] = map[string]bool{}
					}
					s.rust.external[key][o.Symbol] = true
				}
				s.rust.seeded[o.Symbol] = true
			}
		}
	}
	if err := syncSources(g.Sources); err != nil {
		return err
	}
	// All overlays arrive before any diagnostic/definition query. Pull diagnostics
	// run against that revision; readiness also rejects partial Cargo loading.
	for _, path := range paths {
		if err := client.CheckDocument(ctx, filepath.Join(root, filepath.FromSlash(path))); err != nil {
			return err
		}
	}
	if err := client.WaitRustReady(ctx); err != nil {
		return err
	}
	locals := map[string][]bodySpan{}
	for _, path := range paths {
		edit, doc := edits[path], documents[path]
		encoding, _ := scipSourceEncoding(doc, edit.before)
		var kept []*scip.Occurrence
		for _, o := range doc.Occurrences {
			anchor, err := scipAnchor(edit.before, o, encoding)
			if err != nil {
				return err
			}
			if edit.containedBefore(int(anchor.StartByte), int(anchor.EndByte)) {
				continue
			}
			fresh := proto.Clone(o).(*scip.Occurrence)
			var ok bool
			fresh.Range, ok = edit.remap(o.Range, encoding)
			if !ok {
				return fmt.Errorf("cannot remap Rust occurrence")
			}
			fresh.EnclosingRange, ok = edit.remap(o.EnclosingRange, encoding)
			if !ok {
				return fmt.Errorf("cannot remap Rust declaration extent")
			}
			kept = append(kept, fresh)
		}
		doc.Occurrences, doc.PositionEncoding = kept, scip.PositionEncoding_UTF8CodeUnitOffsetFromLineStart
		for _, site := range edit.references {
			locations, err := client.Definition(ctx, filepath.Join(root, filepath.FromSlash(path)), lspPosition(edit.after, site.start, client.Encoding))
			if err != nil {
				return err
			}
			if len(locations) == 0 {
				if site.operator {
					continue
				}
				return fmt.Errorf("unresolved Rust reference requires SCIP: %s", edit.after.Text[site.start:site.end])
			}
			var targets map[string]bool
			for _, loc := range locations {
				candidates := map[string]bool{}
				full, err := lsp.Path(loc.URI)
				if err != nil {
					return err
				}
				rel, err := filepath.Rel(root, full)
				if err != nil {
					return err
				}
				rel = filepath.ToSlash(rel)
				if src := g.Sources[rel]; src != nil {
					offset, err := src.Offset(loc.Range.Start.Line, loc.Range.Start.Character, client.Encoding)
					if err != nil {
						return err
					}
					symbol := definitions[locationKey(rel, offset)]
					if symbol == "" {
						if rustLocalDefinition(src, offset, locals) {
							continue
						}
						return fmt.Errorf("unmapped Rust definition %s:%d", rel, offset)
					}
					candidates[symbol] = true
				} else {
					if site.operator {
						continue
					}
					key, err := rustLocationKey(loc)
					if err != nil {
						return err
					}
					if len(s.rust.external[key]) == 0 {
						return fmt.Errorf("new external Rust target requires SCIP: %s", full)
					}
					for symbol := range s.rust.external[key] {
						candidates[symbol] = true
					}
				}
				// Macro-generated external methods can return both definition
				// and expansion sites. Intersect identities across all sites so
				// shared macro definitions cannot mix i32/i64/etc. impls.
				if targets == nil {
					targets = candidates
				} else {
					for symbol := range targets {
						if !candidates[symbol] {
							delete(targets, symbol)
						}
					}
				}
			}
			if targets != nil && len(targets) != 1 {
				return fmt.Errorf("ambiguous Rust reference identity")
			}
			symbols := make([]string, 0, len(targets))
			for symbol := range targets {
				symbols = append(symbols, symbol)
			}
			slices.Sort(symbols)
			for _, symbol := range symbols {
				doc.Occurrences = append(doc.Occurrences, &scip.Occurrence{Symbol: symbol, Range: byteRange(edit.after, site.start, site.end)})
			}
		}
	}
	return writeSCIPDocuments(ctx, artifact, strings.TrimPrefix(base.Snapshot.ToolVersions["rust-analyzer"], "rust-analyzer "), docs)
}

func rustLocalDefinition(src *graph.Source, offset int, cache map[string][]bodySpan) bool {
	if spans, ok := cache[src.Path]; ok {
		for _, span := range spans {
			if offset >= span.start && offset < span.end {
				return true
			}
		}
		return false
	}
	lang := parserLanguage("rust")
	tree := parseDeclarationTree(lang, src.Text)
	if tree == nil {
		return false
	}
	defer tree.Release()
	cache[src.Path] = nil
	var walk func(*tsNode)
	walk = func(n *tsNode) {
		if n == nil {
			return
		}
		var local *tsNode
		switch n.Kind() {
		case "parameter", "let_declaration", "for_expression", "let_condition", "match_arm":
			local = n.ChildByFieldName("pattern")
		case "self_parameter", "closure_parameters", "type_parameter", "lifetime_parameter":
			local = n
		}
		if local != nil {
			cache[src.Path] = append(cache[src.Path], bodySpan{int(local.StartByte()), int(local.EndByte())})
		}
		for i := 0; i < n.NamedChildCount(); i++ {
			walk(n.NamedChild(i))
		}
	}
	walk(wrapNode(tree.RootNode(), lang))
	return rustLocalDefinition(src, offset, cache)
}
