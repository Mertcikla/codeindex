package indexer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	pb "buf.build/gen/go/tldiagramcom/diagram/protocolbuffers/go/codeindex/v1"
	"github.com/mertcikla/codeindex/graph"
	"github.com/mertcikla/codeindex/lsp"
	scip "github.com/scip-code/scip/bindings/go/scip"
	"google.golang.org/protobuf/proto"
)

const pyrightLSPVersion = "1.1.409"

func pythonLocationKey(loc lsp.Location) (string, error) {
	path, err := lsp.Path(loc.URI)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s:%d:%d", filepath.Clean(path), loc.Range.Start.Line, loc.Range.Start.Character), nil
}

// refreshPythonLSP retains SCIP declaration identities and metadata, replacing
// only changed-body references through Pyright definition locations.
func (s *WatchSession) refreshPythonLSP(ctx context.Context, root string, project *pb.Project, snap *pb.Snapshot, g *graph.Graph, base *IncrementalBase, cached graph.ProjectArtifact, artifact string) error {
	edits, err := pythonEdits(project, snap, g, base)
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
	capturedDefinitions := map[string]bool{}
	locationKey := func(path string, offset int) string { return fmt.Sprintf("%s:%d", path, offset) }
	for _, doc := range docs {
		path := scipDocumentPath(project, doc.RelativePath)
		if documents[path] != nil {
			return fmt.Errorf("multiple Python configurations for %s", path)
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
			capturedDefinitions[locationKey(path, offset)] = true
			if edit := edits[path]; edit != nil {
				if edit.containedBefore(int(anchor.StartByte), int(anchor.EndByte)) {
					return fmt.Errorf("global Python definition inside edited body")
				}
				var ok bool
				offset, ok = edit.offset(offset)
				if !ok {
					return fmt.Errorf("cannot remap Python definition")
				}
			}
			key := locationKey(path, offset)
			if previous := definitions[key]; previous != "" && previous != o.Symbol {
				return fmt.Errorf("ambiguous Python definition at %s", key)
			}
			definitions[key] = o.Symbol
			defined[o.Symbol] = true
		}
	}
	for _, path := range paths {
		if documents[path] == nil {
			return fmt.Errorf("changed Python file has no SCIP document: %s", path)
		}
		for _, offset := range edits[path].definitionOffsets {
			if !capturedDefinitions[locationKey(path, offset)] {
				return fmt.Errorf("changed Python function was not indexed by SCIP")
			}
		}
	}
	client, unlock, err := s.python.lock(ctx, root)
	if err != nil {
		return err
	}
	defer unlock()
	if filepath.Clean(filepath.Join(root, filepath.FromSlash(project.Root))) != s.python.projectDir {
		return fmt.Errorf("Python project requires a separate Pyright configuration")
	}
	if client.PyrightVersion() != pyrightLSPVersion {
		return fmt.Errorf("unvalidated Pyright version %s", client.PyrightVersion())
	}
	syncSources := func(sources map[string]*graph.Source) error {
		for path := range s.python.hashes {
			if sources[path] == nil {
				return fmt.Errorf("opened Python source removed")
			}
		}
		for path, src := range sources {
			_, opened := s.python.hashes[path]
			if src.Language == "python" && (edits[path] != nil || opened) && s.python.hashes[path] != src.Hash {
				if err := client.Sync(filepath.Join(root, filepath.FromSlash(path)), src.Text); err != nil {
					return err
				}
				s.python.hashes[path] = src.Hash
			}
		}
		return nil
	}
	needSeed := false
	for _, path := range paths {
		encoding, _ := scipSourceEncoding(documents[path], base.Graph.Sources[path])
		for _, o := range documents[path].Occurrences {
			if scip.SymbolRole_Definition.Matches(o) || defined[o.Symbol] || s.python.seeded[o.Symbol] {
				continue
			}
			anchor, err := scipAnchor(base.Graph.Sources[path], o, encoding)
			if err != nil {
				return err
			}
			if edits[path].containedBefore(int(anchor.StartByte), int(anchor.EndByte)) {
				needSeed = true
				break
			}
		}
	}
	if err := syncSources(base.Graph.Sources); err != nil {
		return err
	}
	// Identity learning must see the baseline revision, including after a
	// reverted/failed overlay. Establish the same analysis barrier as publishing.
	for _, path := range paths {
		if err := client.CheckPythonDocument(ctx, filepath.Join(root, filepath.FromSlash(path))); err != nil {
			return err
		}
	}
	if needSeed {
		for _, path := range paths {
			src, doc, edit := base.Graph.Sources[path], documents[path], edits[path]
			encoding, _ := scipSourceEncoding(doc, src)
			for _, o := range doc.Occurrences {
				if scip.SymbolRole_Definition.Matches(o) || defined[o.Symbol] || s.python.seeded[o.Symbol] {
					continue
				}
				anchor, err := scipAnchor(src, o, encoding)
				if err != nil {
					return err
				}
				if !edit.containedBefore(int(anchor.StartByte), int(anchor.EndByte)) {
					continue
				}
				locations, err := client.Definition(ctx, filepath.Join(root, filepath.FromSlash(path)), lspPosition(src, int(anchor.StartByte), client.Encoding))
				if err != nil {
					return err
				}
				if len(locations) == 0 {
					return fmt.Errorf("ambiguous external Python identity (%d locations): %s", len(locations), o.Symbol)
				}
				for _, loc := range locations {
					key, err := pythonLocationKey(loc)
					if err != nil {
						return err
					}
					if s.python.external[key] == nil {
						s.python.external[key] = map[string]bool{}
					}
					s.python.external[key][o.Symbol] = true
				}
				s.python.seeded[o.Symbol] = true
			}
		}
	}
	// SCIP-Python deliberately treats non-overloaded builtin functions as
	// document-local intrinsics. Learn that omission from baseline sites and
	// prune the corresponding LSP results, rather than publishing extra edges.
	for _, path := range paths {
		src, doc, edit := base.Graph.Sources[path], documents[path], edits[path]
		encoding, _ := scipSourceEncoding(doc, src)
		globalSites := map[int]bool{}
		for _, o := range doc.Occurrences {
			anchor, err := scipAnchor(src, o, encoding)
			if err != nil {
				return err
			}
			globalSites[int(anchor.StartByte)] = true
		}
		for _, site := range edit.beforeReferences {
			if globalSites[site.start] {
				continue
			}
			locations, err := client.Definition(ctx, filepath.Join(root, filepath.FromSlash(path)), lspPosition(src, site.start, client.Encoding))
			if err != nil {
				return err
			}
			for _, loc := range locations {
				key, err := pythonLocationKey(loc)
				if err != nil {
					return err
				}
				if s.python.ignored[key] {
					continue
				}
				if pythonBuiltinFunction(loc, client.Encoding, s.python.builtins) {
					s.python.ignored[key] = true
				}
			}
		}
	}
	if err := syncSources(g.Sources); err != nil {
		return err
	}
	// All overlays arrive before any diagnostic/definition query. Pull diagnostics
	// run against that revision before publishing any regenerated occurrences.
	for _, path := range paths {
		if err := client.CheckPythonDocument(ctx, filepath.Join(root, filepath.FromSlash(path))); err != nil {
			return err
		}
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
				return fmt.Errorf("cannot remap Python occurrence")
			}
			fresh.EnclosingRange, ok = edit.remap(o.EnclosingRange, encoding)
			if !ok {
				return fmt.Errorf("cannot remap Python declaration extent")
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
				return fmt.Errorf("unresolved Python reference requires SCIP: %s", edit.after.Text[site.start:site.end])
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
						if pythonLocalDefinition(src, offset, locals) {
							continue
						}
						return fmt.Errorf("unmapped Python definition %s:%d", rel, offset)
					}
					candidates[symbol] = true
				} else {
					key, err := pythonLocationKey(loc)
					if err != nil {
						return err
					}
					if s.python.ignored[key] {
						continue
					}
					if len(s.python.external[key]) == 0 {
						return fmt.Errorf("new external Python target %s requires SCIP: %s:%d", edit.after.Text[site.start:site.end], full, loc.Range.Start.Line+1)
					}
					for symbol := range s.python.external[key] {
						candidates[symbol] = true
					}
				}
				// All returned definition locations must agree on one captured identity.
				// Ambiguous overloads and aliases require SCIP.
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
				return fmt.Errorf("ambiguous Python reference identity")
			}
			symbols := make([]string, 0, len(targets))
			for symbol := range targets {
				symbols = append(symbols, symbol)
			}
			slices.Sort(symbols)
			for _, symbol := range symbols {
				parsed, err := scip.ParseSymbol(symbol)
				if err != nil {
					return err
				}
				if n := len(parsed.Descriptors); n > 0 && pythonDynamicName(parsed.Descriptors[n-1].Name) {
					return fmt.Errorf("dynamic Python target requires SCIP")
				}
				doc.Occurrences = append(doc.Occurrences, &scip.Occurrence{Symbol: symbol, Range: byteRange(edit.after, site.start, site.end)})
			}
		}
	}
	return writeSCIPDocuments(ctx, artifact, base.Snapshot.ToolVersions["scip-python"], docs)
}

func pythonBuiltinFunction(loc lsp.Location, encoding string, cache map[string]map[string]bool) bool {
	path, err := lsp.Path(loc.URI)
	if err != nil || !strings.HasSuffix(filepath.ToSlash(path), "/typeshed-fallback/stdlib/builtins.pyi") {
		return false
	}
	if functions, ok := cache[path]; ok {
		key, _ := pythonLocationKey(loc)
		return functions[key]
	}
	cache[path] = map[string]bool{}
	text, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	src := &graph.Source{Path: path, Language: "python", Text: text}
	lang := parserLanguage("python")
	tree := parseDeclarationTree(lang, text)
	if tree == nil {
		return false
	}
	defer tree.Release()
	if tree.RootNode().HasErrorOrMissing() {
		return false
	}
	var walk func(*tsNode)
	walk = func(n *tsNode) {
		if n == nil || n.Kind() == "class_definition" || n.Kind() == "decorated_definition" {
			return
		}
		if n.Kind() == "function_definition" {
			if name := n.ChildByFieldName("name"); name != nil {
				if pythonDynamicName(string(text[name.StartByte():name.EndByte()])) {
					return
				}
				key, _ := pythonLocationKey(lsp.Location{URI: loc.URI, Range: lsp.Range{Start: lspPosition(src, int(name.StartByte()), encoding)}})
				cache[path][key] = true
			}
			return
		}
		for i := 0; i < n.NamedChildCount(); i++ {
			walk(n.NamedChild(i))
		}
	}
	walk(wrapNode(tree.RootNode(), lang))
	key, _ := pythonLocationKey(loc)
	return cache[path][key]
}

func pythonLocalDefinition(src *graph.Source, offset int, cache map[string][]bodySpan) bool {
	if spans, ok := cache[src.Path]; ok {
		for _, span := range spans {
			if offset >= span.start && offset < span.end {
				return true
			}
		}
		return false
	}
	lang := parserLanguage("python")
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
		if n.Kind() == "function_definition" {
			if body := n.ChildByFieldName("body"); body != nil {
				cache[src.Path] = append(cache[src.Path], bodySpan{int(body.StartByte()), int(body.EndByte())})
			}
			return
		}
		for i := 0; i < n.NamedChildCount(); i++ {
			walk(n.NamedChild(i))
		}
	}
	walk(wrapNode(tree.RootNode(), lang))
	return pythonLocalDefinition(src, offset, cache)
}
