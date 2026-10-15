package indexer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	pb "buf.build/gen/go/tldiagramcom/diagram/protocolbuffers/go/codeindex/v1"
	"github.com/mertcikla/codeindex/graph"
	"github.com/mertcikla/codeindex/tools"
	scip "github.com/scip-code/scip/bindings/go/scip"
	"google.golang.org/protobuf/proto"
)

// Dynamic Python edits can change inferred types in untouched callers. Limit
// refreshes to one file and fully annotated top-level function bodies, with no
// imports, nested definitions, decorators or writes outside local variables.
// The merge additionally requires identical global bindings and symbol metadata.
func pythonChangedTarget(ctx context.Context, pr *pb.Project, snap *pb.Snapshot, g *graph.Graph, base *IncrementalBase, tmp string) string {
	if base == nil || base.Snapshot.ConfigHash != snap.ConfigHash || len(base.Snapshot.Warnings) > 0 || !base.Snapshot.GetManifest().GetComplete() || !snap.GetManifest().GetComplete() {
		return ""
	}
	projects := 0
	for _, p := range snap.Projects {
		if languageFamily(p.Language) == familyPython {
			projects++
		}
	}
	if projects != 1 || len(snap.Manifest.Files) != len(base.Snapshot.Manifest.Files) {
		return ""
	}
	old := map[string]*pb.SourceFile{}
	for _, f := range base.Snapshot.Manifest.Files {
		old[f.Path] = f
	}
	changed := ""
	for _, f := range snap.Manifest.Files {
		before := old[f.Path]
		if before == nil || before.Mode != f.Mode {
			return ""
		}
		if before.Hash == f.Hash {
			continue
		}
		if changed != "" || f.Path == pr.ConfigPath || !strings.HasSuffix(f.Path, ".py") || g.Sources[f.Path] == nil || base.Graph.Sources[f.Path] == nil {
			return ""
		}
		changed = f.Path
	}
	if changed == "" {
		return ""
	}
	target, err := filepath.Rel(pr.Root, changed)
	if err != nil || !filepath.IsLocal(target) {
		return ""
	}
	input, err := json.Marshal([]string{string(base.Graph.Sources[changed].Text), string(g.Sources[changed].Text)})
	if err != nil {
		return ""
	}
	filename := filepath.Join(tmp, "python-body-change.json")
	if err := os.WriteFile(filename, input, 0600); err != nil {
		return ""
	}
	defer func() { _ = os.Remove(filename) }()
	output, err := tools.Run(ctx, "python3", []string{"-I", "-c", pythonBodyChangeScript, filename}, "")
	if err != nil || strings.TrimSpace(string(output)) != "true" {
		return ""
	}
	return filepath.ToSlash(target)
}

const pythonBodyChangeScript = `
import ast, json, sys

def eligible(before, after):
    left, right = (ast.parse(s, type_comments=True) for s in (before, after))
    if len(left.body) != len(right.body): return False
    changed = False
    for a, b in zip(left.body, right.body):
        if ast.dump(a) == ast.dump(b): continue
        if type(a) is not type(b) or not isinstance(a, (ast.FunctionDef, ast.AsyncFunctionDef)): return False
        for fn in (a, b):
            args = fn.args.posonlyargs + fn.args.args + fn.args.kwonlyargs
            args += [v for v in (fn.args.vararg, fn.args.kwarg) if v is not None]
            if fn.returns is None or fn.decorator_list or any(v.annotation is None for v in args): return False
            for stmt in fn.body:
                for node in ast.walk(stmt):
                    if isinstance(node, (ast.Global, ast.Nonlocal, ast.Import, ast.ImportFrom, ast.ClassDef, ast.FunctionDef, ast.AsyncFunctionDef, ast.Yield, ast.YieldFrom)): return False
                    if isinstance(node, (ast.Attribute, ast.Subscript)) and isinstance(node.ctx, (ast.Store, ast.Del)): return False
        a.body, b.body = [ast.Pass()], [ast.Pass()]
        if ast.dump(a) != ast.dump(b): return False
        changed = True
    return changed and ast.dump(left) == ast.dump(right)

try:
    with open(sys.argv[1]) as f: before, after = json.load(f)
    print(json.dumps(eligible(before, after)))
except (SyntaxError, ValueError, TypeError):
    print('false')
`

// scip-python changes its document root to --target-only and can emit imported
// files outside that root. Normalize all paths, compact to the importer fields,
// then reject context-sensitive changes rather than publishing a different graph.
func mergePythonArtifact(ctx context.Context, cached []byte, artifact string, pr *pb.Project, g *graph.Graph, target, projectName string) error {
	file, err := os.Open(artifact)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	index := &scip.Index{}
	visitor := &scip.IndexVisitor{
		VisitMetadata: func(_ context.Context, m *scip.Metadata) error { index.Metadata = m; return ctx.Err() },
		VisitDocument: func(_ context.Context, d *scip.Document) error {
			if filepath.IsAbs(d.RelativePath) {
				return fmt.Errorf("absolute Python document path")
			}
			d.RelativePath = path.Clean(path.Join(target, d.RelativePath))
			if g.Sources[scipDocumentPath(pr, d.RelativePath)] != nil {
				index.Documents = append(index.Documents, d)
			}
			return ctx.Err()
		},
	}
	if err := visitor.ParseStreaming(ctx, file); err != nil {
		return err
	}
	if index.Metadata.GetToolInfo().GetVersion() != "0.6.6" {
		return fmt.Errorf("unexpected Python artifact version")
	}
	raw, err := proto.Marshal(index)
	if err != nil {
		return err
	}
	compact, err := compactSCIPArtifact(ctx, g, pr, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	reader, err := scipArtifactReader(compact)
	if err != nil {
		return err
	}
	defer func() { _ = reader.Close() }()
	current, err := readSCIPDocuments(ctx, g, pr, reader)
	if err != nil {
		return err
	}
	oldReader, err := scipArtifactReader(cached)
	if err != nil {
		return err
	}
	defer func() { _ = oldReader.Close() }()
	previous, err := readSCIPDocuments(ctx, g, pr, oldReader)
	if err != nil {
		return err
	}
	rewrite, err := projectVersionRewriter(previous, current, projectName)
	if err != nil {
		return err
	}
	replacements := map[string]*scip.Document{}
	for _, d := range current {
		if replacements[d.RelativePath] != nil {
			return fmt.Errorf("duplicate Python document")
		}
		replacements[d.RelativePath] = d
	}
	bindings := func(d *scip.Document) []string {
		var keys []string
		for _, o := range d.Occurrences {
			keys = append(keys, fmt.Sprintf("%d:%s", o.SymbolRoles, o.Symbol))
		}
		return keys
	}
	seen := false
	for i, doc := range previous {
		for _, o := range doc.Occurrences {
			o.Symbol = rewrite(o.Symbol)
		}
		for _, s := range doc.Symbols {
			s.Symbol = rewrite(s.Symbol)
			for _, r := range s.Relationships {
				r.Symbol = rewrite(r.Symbol)
			}
		}
		fresh := replacements[doc.RelativePath]
		delete(replacements, doc.RelativePath)
		if doc.RelativePath != target {
			if fresh != nil && !proto.Equal(doc, fresh) {
				return fmt.Errorf("Python bindings changed in untouched document %s", doc.RelativePath)
			}
			continue
		}
		if fresh == nil {
			return fmt.Errorf("missing Python target document")
		}
		// Source coordinates can move; global occurrences (including definitions)
		// and signatures/relationships must remain identical. New calls, inferred
		// types or scope-dependent upstream aliases require a full project scan.
		if !slices.Equal(bindings(doc), bindings(fresh)) {
			return fmt.Errorf("Python target symbol bindings changed")
		}
		oldOccurrences, newOccurrences := doc.Occurrences, fresh.Occurrences
		doc.Occurrences, fresh.Occurrences = nil, nil
		equal := proto.Equal(doc, fresh)
		doc.Occurrences, fresh.Occurrences = oldOccurrences, newOccurrences
		if !equal {
			return fmt.Errorf("Python target declaration metadata changed")
		}
		previous[i] = fresh
		seen = true
	}
	if !seen || len(replacements) > 0 {
		return fmt.Errorf("Python target document membership changed")
	}
	return writeSCIPDocuments(ctx, artifact, "0.6.6", previous)
}
