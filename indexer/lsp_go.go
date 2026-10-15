package indexer

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf16"

	pb "buf.build/gen/go/tldiagramcom/diagram/protocolbuffers/go/codeindex/v1"
	"github.com/mertcikla/codeindex/graph"
	"github.com/mertcikla/codeindex/lsp"
	scip "github.com/scip-code/scip/bindings/go/scip"
	"google.golang.org/protobuf/proto"
)

type bodySpan struct{ start, end int }
type goEdit struct {
	before, after        *graph.Source
	oldBodies, newBodies []bodySpan
	changed              []bool
	set                  *token.FileSet
	file                 *ast.File
}

// goBodyEdit deliberately preserves all text outside function bodies, including
// docs, imports and directives. This lets the adapter retain exactly the
// declaration metadata consumed by tld without approximating Go type strings.
func goBodyEdit(before, after *graph.Source) (*goEdit, error) {
	if bytes.Contains(before.Text, []byte("//line ")) || bytes.Contains(before.Text, []byte("/*line ")) {
		return nil, fmt.Errorf("source line directives require SCIP")
	}
	if !goBodyOnlyChange(before.Text, after.Text) {
		return nil, fmt.Errorf("declaration shape changed")
	}
	parse := func(src *graph.Source) (*token.FileSet, *ast.File, []bodySpan, error) {
		set := token.NewFileSet()
		file, err := parser.ParseFile(set, src.Path, src.Text, parser.ParseComments)
		if err != nil {
			return nil, nil, nil, err
		}
		var bodies []bodySpan
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
				bodies = append(bodies, bodySpan{set.Position(fn.Body.Pos()).Offset, set.Position(fn.Body.End()).Offset})
			}
		}
		return set, file, bodies, nil
	}
	_, _, old, err := parse(before)
	if err != nil {
		return nil, err
	}
	set, file, current, err := parse(after)
	if err != nil || len(old) != len(current) {
		return nil, fmt.Errorf("function membership changed")
	}
	a, b := 0, 0
	changed := make([]bool, len(old))
	for i, span := range old {
		if !bytes.Equal(before.Text[a:span.start], after.Text[b:current[i].start]) {
			return nil, fmt.Errorf("text outside function bodies changed")
		}
		changed[i] = !bytes.Equal(before.Text[span.start:span.end], after.Text[current[i].start:current[i].end])
		a, b = span.end, current[i].end
	}
	if !bytes.Equal(before.Text[a:], after.Text[b:]) {
		return nil, fmt.Errorf("text outside function bodies changed")
	}
	return &goEdit{before: before, after: after, oldBodies: old, newBodies: current, changed: changed, set: set, file: file}, nil
}

func (e *goEdit) offset(old int) (int, bool) {
	delta := 0
	for i, b := range e.oldBodies {
		if old <= b.start {
			return old + delta, true
		}
		if old < b.end {
			if !e.changed[i] {
				return old + delta, true
			}
			return 0, false
		}
		delta = e.newBodies[i].end - b.end
	}
	return old + delta, true
}

func byteRange(src *graph.Source, start, end int) []int32 {
	sl, sc := src.Position(start)
	el, ec := src.Position(end)
	if sl == el {
		return []int32{int32(sl), int32(sc), int32(ec)}
	}
	return []int32{int32(sl), int32(sc), int32(el), int32(ec)}
}

func (e *goEdit) remap(rng []int32, encoding string) ([]int32, bool) {
	if len(rng) == 0 {
		return nil, true
	}
	a, err := scipAnchor(e.before, &scip.Occurrence{Range: rng}, encoding)
	if err != nil {
		return nil, false
	}
	start, ok := e.offset(int(a.StartByte))
	if !ok {
		return nil, false
	}
	end, ok := e.offset(int(a.EndByte))
	if !ok {
		return nil, false
	}
	return byteRange(e.after, start, end), true
}

func lspPosition(src *graph.Source, offset int, encoding string) lsp.Position {
	line, column := src.Position(offset)
	if encoding == "utf-16" {
		start := offset - int(column)
		column = uint32(len(utf16.Encode([]rune(string(src.Text[start:offset])))))
	}
	return lsp.Position{Line: int(line), Character: int(column)}
}

// refreshGoLSP emits the same reduced SCIP document format used by our artifact
// cache. Only tld-consumed semantics matter: local symbols, syntax coloring,
// diagnostics, unused external symbol metadata and redundant text are omitted.
// API edits stay on SCIP until a broader adapter passes full graph parity.
func (s *WatchSession) refreshGoLSP(ctx context.Context, root string, project *pb.Project, snap *pb.Snapshot, g *graph.Graph, base *IncrementalBase, cached graph.ProjectArtifact, artifact string) error {
	if len(goChangedPackages(project, snap, g, base, filepath.Join(root, filepath.FromSlash(project.Root)))) == 0 {
		return fmt.Errorf("change requires full semantic indexing")
	}
	edits := map[string]*goEdit{}
	for path, src := range g.Sources {
		old := base.Graph.Sources[path]
		if src.Language != "go" || old == nil || old.Hash == src.Hash {
			continue
		}
		edit, err := goBodyEdit(old, src)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		edits[path] = edit
	}
	if len(edits) == 0 {
		return fmt.Errorf("no changed Go documents")
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
	externals, err := newGoExternalSymbols(docs)
	if err != nil {
		return err
	}
	client, unlock, err := s.lock(ctx, root)
	if err != nil {
		return err
	}
	defer unlock()
	// Update all prior overlays, including files reverted after an unsuccessful
	// batch. A stale opened overlay overrides disk even when the base is fresh.
	for path := range s.hashes {
		if g.Sources[path] == nil {
			return fmt.Errorf("opened source removed; session needs restart")
		}
	}
	for path, src := range g.Sources {
		_, opened := s.hashes[path]
		if src.Language == "go" && (edits[path] != nil || opened) && s.hashes[path] != src.Hash {
			if err := client.Sync(filepath.Join(root, filepath.FromSlash(path)), src.Text); err != nil {
				return err
			}
			s.hashes[path] = src.Hash
		}
	}
	paths := make([]string, 0, len(edits))
	for path := range edits {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	for _, path := range paths {
		if err := client.CheckDocument(ctx, filepath.Join(root, filepath.FromSlash(path))); err != nil {
			return err
		}
	}
	// A definition location is the bridge between LSP identity and SCIP's
	// canonical symbol key. Rebuild this map for every batch after remapping
	// declaration offsets, so references within and across edited files agree.
	definitions := map[string]string{}
	documents := map[string]*scip.Document{}
	locationKey := func(path string, offset int) string { return fmt.Sprintf("%s:%d", path, offset) }
	for _, doc := range docs {
		path := scipDocumentPath(project, doc.RelativePath)
		src := base.Graph.Sources[path]
		if src == nil {
			return fmt.Errorf("missing baseline source %s", path)
		}
		encoding, _ := scipPositionEncoding(doc)
		edit := edits[path]
		if edit != nil {
			var kept []*scip.Occurrence
			for _, occurrence := range doc.Occurrences {
				fresh := proto.Clone(occurrence).(*scip.Occurrence)
				var ok bool
				fresh.Range, ok = edit.remap(occurrence.Range, encoding)
				if !ok {
					if scip.SymbolRole_Definition.Matches(occurrence) && !strings.HasPrefix(occurrence.Symbol, "local ") {
						return fmt.Errorf("global definition inside edited body")
					}
					continue
				}
				fresh.EnclosingRange, ok = edit.remap(occurrence.EnclosingRange, encoding)
				if !ok {
					return fmt.Errorf("cannot remap definition extent")
				}
				kept = append(kept, fresh)
			}
			doc.Occurrences, doc.PositionEncoding = kept, scip.PositionEncoding_UTF8CodeUnitOffsetFromLineStart
			src, encoding = edit.after, "utf-8"
		}
		documents[path] = doc
		for _, occurrence := range doc.Occurrences {
			if !scip.SymbolRole_Definition.Matches(occurrence) || strings.HasPrefix(occurrence.Symbol, "local ") {
				continue
			}
			anchor, err := scipAnchor(src, occurrence, encoding)
			if err != nil {
				return fmt.Errorf("invalid baseline definition in %s", path)
			}
			definitions[locationKey(path, int(anchor.StartByte))] = occurrence.Symbol
		}
	}
	for _, path := range paths {
		edit, doc := edits[path], documents[path]
		if doc == nil {
			return fmt.Errorf("changed source was not indexed by SCIP: %s", path)
		}
		var queryErr error
		for _, decl := range edit.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			bodyChanged := false
			for i, body := range edit.newBodies {
				if body.start == edit.set.Position(fn.Body.Pos()).Offset {
					bodyChanged = edit.changed[i]
					break
				}
			}
			if !bodyChanged {
				continue
			}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				if queryErr != nil {
					return false
				}
				ident, ok := node.(*ast.Ident)
				if !ok || ident.Name == "_" {
					return true
				}
				start, end := edit.set.Position(ident.Pos()).Offset, edit.set.Position(ident.End()).Offset
				locations, err := client.Definition(ctx, filepath.Join(root, filepath.FromSlash(path)), lspPosition(edit.after, start, client.Encoding))
				if err != nil {
					queryErr = err
					return false
				}
				if len(locations) > 1 {
					queryErr = fmt.Errorf("ambiguous definition for %s", ident.Name)
					return false
				}
				for _, loc := range locations {
					full, err := lsp.Path(loc.URI)
					if err != nil {
						queryErr = err
						return false
					}
					rel, err := filepath.Rel(root, full)
					if err != nil || !filepath.IsLocal(rel) {
						symbol, err := externals.symbol(full, loc.Range.Start, client.Encoding)
						if err != nil {
							queryErr = err
							return false
						}
						if symbol != "" {
							doc.Occurrences = append(doc.Occurrences, &scip.Occurrence{Symbol: symbol, Range: byteRange(edit.after, start, end), SymbolRoles: int32(scip.SymbolRole_ReadAccess)})
						}
						continue
					}
					rel = filepath.ToSlash(rel)
					source := g.Sources[rel]
					if source == nil {
						queryErr = fmt.Errorf("definition outside captured sources: %s", rel)
						return false
					}
					offset, err := source.Offset(loc.Range.Start.Line, loc.Range.Start.Character, client.Encoding)
					if err != nil {
						queryErr = err
						return false
					}
					symbol := definitions[locationKey(rel, offset)]
					if symbol == "" {
						continue
					} // document-local variable, parameter or label
					doc.Occurrences = append(doc.Occurrences, &scip.Occurrence{Symbol: symbol, Range: byteRange(edit.after, start, end), SymbolRoles: int32(scip.SymbolRole_ReadAccess)})
				}
				return true
			})
			if queryErr != nil {
				return queryErr
			}
		}
	}
	return writeSCIPDocuments(ctx, artifact, base.Snapshot.ToolVersions["scip-go"], docs)
}
