package indexer

import (
	"bytes"
	"context"
	"encoding/json"
	"go/ast"
	"go/format"
	"go/parser"
	"go/scanner"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	pb "buf.build/gen/go/tldiagramcom/diagram/protocolbuffers/go/codeindex/v1"
	"github.com/mertcikla/codeindex/graph"
	"github.com/mertcikla/codeindex/tools"
)

// Package refresh is deliberately limited to body-only edits in one indexed
// module. Declarations, imports, named types, build inputs and source membership
// must stay unchanged; otherwise callers and implementations may need reindexing.
func goChangedPackages(project *pb.Project, snap *pb.Snapshot, g *graph.Graph, base *IncrementalBase) []string {
	if base == nil || base.Snapshot.ConfigHash != snap.ConfigHash || len(base.Snapshot.Warnings) != 0 ||
		!base.Snapshot.GetManifest().GetComplete() || !snap.GetManifest().GetComplete() {
		return nil
	}
	goProjects := 0
	for _, p := range snap.Projects {
		if languageFamily(p.Language) == familyGo {
			goProjects++
		}
	}
	if goProjects != 1 || filepath.Base(project.ConfigPath) != "go.mod" ||
		(os.Getenv("GOPACKAGESDRIVER") != "" && os.Getenv("GOPACKAGESDRIVER") != "off") {
		return nil
	}
	previous := map[string]*pb.SourceFile{}
	for _, file := range base.Snapshot.Manifest.Files {
		previous[file.Path] = file
	}
	if len(previous) != len(snap.Manifest.Files) {
		return nil
	}
	dirs := map[string]bool{}
	for _, file := range snap.Manifest.Files {
		old := previous[file.Path]
		if old == nil || old.Mode != file.Mode {
			return nil
		}
		if old.Hash == file.Hash {
			continue
		}
		if strings.HasSuffix(file.Path, "_test.go") {
			continue
		}
		before, after := base.Graph.Sources[file.Path], g.Sources[file.Path]
		if before == nil || after == nil || after.Language != "go" || !goBodyOnlyChange(before.Text, after.Text) {
			return nil
		}
		rel := file.Path
		if prefix := strings.TrimSuffix(project.Root, "/"); prefix != "" && prefix != "." {
			if !strings.HasPrefix(rel, prefix+"/") {
				return nil
			}
			rel = strings.TrimPrefix(rel, prefix+"/")
		}
		if !filepath.IsLocal(rel) {
			return nil
		}
		dirs[filepath.ToSlash(filepath.Dir(rel))] = true
	}
	patterns := make([]string, 0, len(dirs))
	for dir := range dirs {
		if dir == "." {
			patterns = append(patterns, ".")
		} else {
			patterns = append(patterns, "./"+dir)
		}
	}
	slices.Sort(patterns)
	return patterns
}

func goBodyOnlyChange(before, after []byte) bool {
	shape := func(src []byte) ([]byte, error) {
		set := token.NewFileSet()
		file, err := parser.ParseFile(set, "source.go", src, parser.ParseComments)
		if err != nil {
			return nil, err
		}
		var directives []string
		for _, group := range file.Comments {
			for _, comment := range group.List {
				if strings.HasPrefix(comment.Text, "//go:") || strings.HasPrefix(comment.Text, "// +build") || strings.HasPrefix(comment.Text, "//line ") {
					directives = append(directives, comment.Text)
				}
			}
		}
		var localTypes bool
		for _, declaration := range file.Decls {
			if fn, ok := declaration.(*ast.FuncDecl); ok && fn.Body != nil {
				ast.Inspect(fn.Body, func(node ast.Node) bool {
					if _, ok := node.(*ast.TypeSpec); ok {
						localTypes = true
					}
					return true
				})
				fn.Body = nil
			}
		}
		for _, imp := range file.Imports {
			if imp.Path.Value == `"C"` {
				localTypes = true
			}
		}
		if localTypes {
			return nil, io.ErrUnexpectedEOF
		}
		// Clear comments so edits inside function bodies do not leave
		// detached comment positions in the structural representation.
		file.Comments = nil
		ast.Inspect(file, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.FuncDecl:
				n.Doc = nil
			case *ast.GenDecl:
				n.Doc = nil
			case *ast.TypeSpec:
				n.Doc, n.Comment = nil, nil
			case *ast.ValueSpec:
				n.Doc, n.Comment = nil, nil
			case *ast.ImportSpec:
				n.Doc, n.Comment = nil, nil
			case *ast.Field:
				n.Doc, n.Comment = nil, nil
			}
			return true
		})
		var output bytes.Buffer
		if err := format.Node(&output, set, file); err != nil {
			return nil, err
		}
		// format.Node preserves blank lines using original token positions.
		// A larger body can therefore change whitespace between declarations
		// even after removing the bodies. Compare tokens, not layout.
		var normalized bytes.Buffer
		var scan scanner.Scanner
		tokens := token.NewFileSet().AddFile("shape.go", -1, output.Len())
		scan.Init(tokens, output.Bytes(), nil, 0)
		for {
			_, tok, lit := scan.Scan()
			if tok == token.EOF {
				break
			}
			if tok != token.SEMICOLON {
				normalized.WriteString(tok.String())
				normalized.WriteByte(0)
				normalized.WriteString(lit)
				normalized.WriteByte(0)
			}
		}
		return append(normalized.Bytes(), []byte(strings.Join(directives, "\n"))...), nil
	}
	a, err := shape(before)
	if err != nil {
		return false
	}
	b, err := shape(after)
	return err == nil && bytes.Equal(a, b)
}

// Reject ill-typed edits before using cached facts for untouched packages.
func goPackagesBuildable(ctx context.Context, dir string, patterns []string) bool {
	output, err := tools.Run(ctx, "go", append([]string{"list", "-e", "-export", "-json"}, patterns...), dir)
	if err != nil {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	count := 0
	for {
		var pkg struct {
			Error      json.RawMessage
			DepsErrors []json.RawMessage
		}
		if err := decoder.Decode(&pkg); err != nil {
			return err == io.EOF && count > 0
		}
		if len(pkg.Error) != 0 || len(pkg.DepsErrors) != 0 {
			return false
		}
		count++
	}
}
