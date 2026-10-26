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

func tsLocationKey(loc lsp.Location, constructor bool) (string, error) {
	path, err := lsp.Path(loc.URI)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s:%d:%d:%t", filepath.Clean(path), loc.Range.Start.Line, loc.Range.Start.Character, constructor), nil
}

func tsConstructor(raw string, known map[string]bool) string {
	symbol, err := scip.ParseSymbol(raw)
	if err != nil || len(symbol.Descriptors) == 0 || symbol.Descriptors[len(symbol.Descriptors)-1].Suffix != scip.Descriptor_Type {
		return raw
	}
	symbol.Descriptors = append(symbol.Descriptors, &scip.Descriptor{Name: "<constructor>", Suffix: scip.Descriptor_Method})
	constructor := scip.VerboseSymbolFormatter.FormatSymbol(symbol)
	if known[constructor] {
		return constructor
	}
	return raw
}

// These producer versions pass the Docker LSP/SCIP graph parity fixture. The
// private compiler worker retains its separate, stricter version gate.
func typeScriptLSPProducer(version string) bool {
	return version == "0.4.0" || version == "0.5.2"
}

func (s *WatchSession) refreshTypeScriptLSP(ctx context.Context, root string, project *pb.Project, snap *pb.Snapshot, g *graph.Graph, base *IncrementalBase, cached graph.ProjectArtifact, artifact string) error {
	edits, err := typeScriptEdits(project, snap, g, base)
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
	definitions := map[string][]string{}
	definedSymbols := map[string]bool{}
	known := map[string]bool{}
	documents := map[string]*scip.Document{}
	locationKey := func(path string, offset int) string { return fmt.Sprintf("%s:%d", path, offset) }
	for _, doc := range docs {
		path := scipDocumentPath(project, doc.RelativePath)
		if documents[path] != nil {
			return fmt.Errorf("multiple compiler configurations for %s", path)
		}
		documents[path] = doc
		encoding, _ := scipSourceEncoding(doc, base.Graph.Sources[path])
		for _, o := range doc.Occurrences {
			known[o.Symbol] = true
			if edits[path] != nil {
				parsed, err := scip.ParseSymbol(o.Symbol)
				if err != nil {
					return err
				}
				for _, descriptor := range parsed.Descriptors {
					if descriptor.Suffix == scip.Descriptor_Meta && tsSymbolFile(parsed) == doc.RelativePath {
						return fmt.Errorf("counter-dependent SCIP identity in edited file")
					}
				}
			}
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
					return fmt.Errorf("global definition inside edited body")
				}
				var ok bool
				offset, ok = edit.offset(offset)
				if !ok {
					return fmt.Errorf("cannot remap TypeScript definition")
				}
			}
			key := locationKey(path, offset)
			definitions[key] = append(definitions[key], o.Symbol)
			definedSymbols[o.Symbol] = true
		}
	}
	for _, path := range paths {
		if documents[path] == nil {
			return fmt.Errorf("changed file has no SCIP document: %s", path)
		}
	}
	client, unlock, err := s.web.lock(ctx, root)
	if err != nil {
		return err
	}
	defer unlock()
	// Learn external keys from authoritative baseline occurrences. Restoring all
	// edited overlays first keeps cross-file inference consistent during this
	// identity-only phase. No baseline graph is published or mutated.
	syncSources := func(sources map[string]*graph.Source) error {
		for path := range s.web.hashes {
			if sources[path] == nil {
				return fmt.Errorf("opened TypeScript source removed")
			}
		}
		for path, src := range sources {
			_, opened := s.web.hashes[path]
			if languageFamily(src.Language) == familyWeb && (edits[path] != nil || opened) && s.web.hashes[path] != src.Hash {
				if err := client.Sync(filepath.Join(root, filepath.FromSlash(path)), src.Text); err != nil {
					return err
				}
				s.web.hashes[path] = src.Hash
			}
		}
		return nil
	}
	needSeed := false
	for _, path := range paths {
		doc := documents[path]
		encoding, _ := scipSourceEncoding(doc, base.Graph.Sources[path])
		for _, o := range doc.Occurrences {
			if scip.SymbolRole_Definition.Matches(o) || definedSymbols[o.Symbol] || s.web.seeded[o.Symbol] {
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
	if needSeed {
		if err := syncSources(base.Graph.Sources); err != nil {
			return err
		}
	}
	localBounds := map[string][]bodySpan{}
	for _, path := range paths {
		src, edit, doc := base.Graph.Sources[path], edits[path], documents[path]
		encoding, _ := scipSourceEncoding(doc, base.Graph.Sources[path])
		for _, o := range doc.Occurrences {
			if scip.SymbolRole_Definition.Matches(o) || definedSymbols[o.Symbol] || s.web.seeded[o.Symbol] {
				continue
			}
			anchor, err := scipAnchor(src, o, encoding)
			if err != nil {
				return err
			}
			if !edit.containedBefore(int(anchor.StartByte), int(anchor.EndByte)) {
				continue
			}
			position := lspPosition(src, int(anchor.StartByte), client.Encoding)
			locations, err := client.Definition(ctx, filepath.Join(root, filepath.FromSlash(path)), position)
			if err != nil {
				return err
			}
			parsed, err := scip.ParseSymbol(o.Symbol)
			if err != nil {
				return err
			}
			var namespace []string
			constructor := false
			for _, d := range parsed.Descriptors {
				if d.Suffix == scip.Descriptor_Namespace {
					namespace = append(namespace, d.Name)
				}
				if d.Suffix == scip.Descriptor_Method && d.Name == "<constructor>" {
					constructor = true
				}
			}
			matched := false
			for _, loc := range locations {
				full, err := lsp.Path(loc.URI)
				if err != nil {
					return err
				}
				if len(locations) > 1 && len(namespace) > 0 && !strings.HasSuffix(filepath.ToSlash(full), "/"+strings.Join(namespace, "/")) {
					continue
				}
				key, err := tsLocationKey(loc, constructor)
				if err != nil {
					return err
				}
				if s.web.external[key] == nil {
					s.web.external[key] = map[string]bool{}
				}
				s.web.external[key][o.Symbol] = true
				matched = true
			}
			if !matched {
				return fmt.Errorf("cannot bind baseline external symbol %s", o.Symbol)
			}
			s.web.seeded[o.Symbol] = true
		}
	}
	if err := syncSources(g.Sources); err != nil {
		return err
	}
	for _, path := range paths {
		full := filepath.Join(root, filepath.FromSlash(path))
		var info struct {
			ConfigFileName string `json:"configFileName"`
		}
		if err := client.TypeScriptRequest(ctx, "projectInfo", map[string]any{"file": full, "needFileNameList": false}, &info); err != nil {
			return err
		}
		matched := false
		for _, name := range webProjectConfigs(filepath.Join(root, filepath.FromSlash(project.Root))) {
			if filepath.Clean(info.ConfigFileName) == filepath.Clean(filepath.Join(root, filepath.FromSlash(project.Root), name)) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("LSP selected an unindexed TypeScript configuration: %s", info.ConfigFileName)
		}
		if err := client.CheckTypeScriptDocument(ctx, full); err != nil {
			return err
		}
	}
	for _, path := range paths {
		edit, doc := edits[path], documents[path]
		encoding, _ := scipSourceEncoding(doc, base.Graph.Sources[path])
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
				return fmt.Errorf("cannot remap TypeScript occurrence")
			}
			fresh.EnclosingRange, ok = edit.remap(o.EnclosingRange, encoding)
			if !ok {
				return fmt.Errorf("cannot remap TypeScript declaration extent")
			}
			kept = append(kept, fresh)
		}
		doc.Occurrences, doc.PositionEncoding = kept, scip.PositionEncoding_UTF8CodeUnitOffsetFromLineStart
		for _, site := range edit.references {
			position := lspPosition(edit.after, site.start, client.Encoding)
			locations, err := client.Definition(ctx, filepath.Join(root, filepath.FromSlash(path)), position)
			if err != nil {
				return err
			}
			targets := map[string]bool{}
			for _, loc := range locations {
				full, err := lsp.Path(loc.URI)
				if err != nil {
					return err
				}
				rel, err := filepath.Rel(root, full)
				if err != nil {
					return err
				}
				rel = filepath.ToSlash(rel)
				var symbols []string
				if src := g.Sources[rel]; src != nil {
					offset, err := src.Offset(loc.Range.Start.Line, loc.Range.Start.Character, client.Encoding)
					if err != nil {
						return err
					}
					symbols = definitions[locationKey(rel, offset)]
					if len(symbols) == 0 {
						// A definition inside a function block is document-local in
						// SCIP. Everything else must bind to the captured baseline.
						if tsLocalDefinition(src, offset, localBounds) {
							continue
						}
						return fmt.Errorf("unmapped TypeScript definition %s:%d", rel, offset)
					}
					for _, symbol := range symbols {
						if site.constructor {
							symbol = tsConstructor(symbol, known)
						}
						targets[symbol] = true
					}
				} else {
					key, err := tsLocationKey(loc, site.constructor)
					if err != nil {
						return err
					}
					if len(s.web.external[key]) == 0 {
						return fmt.Errorf("new external TypeScript target requires SCIP: %s", full)
					}
					for symbol := range s.web.external[key] {
						targets[symbol] = true
					}
				}
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
	return writeSCIPDocuments(ctx, artifact, base.Snapshot.ToolVersions["scip-typescript"], docs)
}

func tsSymbolFile(symbol *scip.Symbol) string {
	var parts []string
	for _, d := range symbol.Descriptors {
		if d.Suffix != scip.Descriptor_Namespace {
			break
		}
		parts = append(parts, d.Name)
		switch filepath.Ext(d.Name) {
		case ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".mts", ".cts":
			return strings.Join(parts, "/")
		}
	}
	return ""
}

func tsLocalDefinition(src *graph.Source, offset int, cache map[string][]bodySpan) bool {
	if spans, ok := cache[src.Path]; ok {
		for _, span := range spans {
			if offset >= span.start && offset < span.end {
				return true
			}
		}
		return false
	}
	lang := parserLanguage(src.Language)
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
		if n.Kind() == "statement_block" || n.Kind() == "arrow_function" || n.Kind() == "function_expression" {
			cache[src.Path] = append(cache[src.Path], bodySpan{int(n.StartByte()), int(n.EndByte())})
			return
		}
		for i := 0; i < n.NamedChildCount(); i++ {
			walk(n.NamedChild(i))
		}
	}
	walk(wrapNode(tree.RootNode(), lang))
	return tsLocalDefinition(src, offset, cache)
}
