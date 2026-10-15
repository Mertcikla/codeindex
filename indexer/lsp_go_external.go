package indexer

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mertcikla/codeindex/graph"
	"github.com/mertcikla/codeindex/lsp"
	scip "github.com/scip-code/scip/bindings/go/scip"
)

// External calls are part of tld's graph even when no target fact is captured.
// LSP supplies the authoritative definition site; AST structure supplies only
// its SCIP descriptor. Package/version identities come from the baseline, not
// guesses about installed toolchain versions or module-cache directory names.
type goExternalSymbols struct {
	packages   map[string]*scip.Package
	namespaces map[string]*scip.Package
	files      map[string]*externalGoFile
}
type externalGoFile struct {
	source      *graph.Source
	namespace   string
	descriptors map[int][]*scip.Descriptor
}

func newGoExternalSymbols(docs []*scip.Document) (*goExternalSymbols, error) {
	r := &goExternalSymbols{packages: map[string]*scip.Package{}, namespaces: map[string]*scip.Package{}, files: map[string]*externalGoFile{}}
	seen := map[string]bool{}
	for _, doc := range docs {
		for _, occurrence := range doc.Occurrences {
			raw := occurrence.Symbol
			if seen[raw] || raw == "" || strings.HasPrefix(raw, "local ") {
				continue
			}
			seen[raw] = true
			symbol, err := scip.ParseSymbol(raw)
			if err != nil || symbol.Scheme != "scip-go" || symbol.Package == nil || len(symbol.Descriptors) == 0 {
				continue
			}
			pkg := symbol.Package
			if old := r.packages[pkg.Name]; old != nil && old.Version != pkg.Version {
				return nil, fmt.Errorf("ambiguous module %s", pkg.Name)
			}
			r.packages[pkg.Name] = pkg
			r.namespaces[symbol.Descriptors[0].Name] = pkg
		}
	}
	return r, nil
}

func (r *goExternalSymbols) symbol(path string, pos lsp.Position, encoding string) (string, error) {
	file := r.files[path]
	if file == nil {
		text, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		namespace, err := externalGoNamespace(filepath.Dir(path))
		if err != nil {
			return "", err
		}
		file = &externalGoFile{source: &graph.Source{Text: text}, namespace: namespace, descriptors: map[int][]*scip.Descriptor{}}
		set := token.NewFileSet()
		parsed, err := parser.ParseFile(set, path, text, 0)
		if err != nil {
			return "", err
		}
		define := func(ident *ast.Ident, descriptors ...*scip.Descriptor) {
			file.descriptors[set.Position(ident.Pos()).Offset] = descriptors
		}
		for _, decl := range parsed.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				var descriptors []*scip.Descriptor
				if d.Recv != nil && len(d.Recv.List) > 0 {
					name := goReceiverName(d.Recv.List[0].Type)
					if name == "" {
						continue
					}
					descriptors = append(descriptors, &scip.Descriptor{Name: name, Suffix: scip.Descriptor_Type})
				}
				define(d.Name, append(descriptors, &scip.Descriptor{Name: d.Name.Name, Suffix: scip.Descriptor_Method})...)
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch spec := spec.(type) {
					case *ast.TypeSpec:
						owner := &scip.Descriptor{Name: spec.Name.Name, Suffix: scip.Descriptor_Type}
						define(spec.Name, owner)
						var fields *ast.FieldList
						suffix := scip.Descriptor_Term
						switch typ := spec.Type.(type) {
						case *ast.StructType:
							fields = typ.Fields
						case *ast.InterfaceType:
							fields, suffix = typ.Methods, scip.Descriptor_Method
						}
						if fields != nil {
							for _, field := range fields.List {
								for _, name := range field.Names {
									define(name, owner, &scip.Descriptor{Name: name.Name, Suffix: suffix})
								}
							}
						}
					case *ast.ValueSpec:
						for _, name := range spec.Names {
							define(name, &scip.Descriptor{Name: name.Name, Suffix: scip.Descriptor_Term})
						}
					}
				}
			}
		}
		// A package qualifier is not a symbol reference in scip-go's file
		// visitor. Keep an explicit empty entry so it is not mistaken for an
		// unsupported declaration.
		define(parsed.Name)
		r.files[path] = file
	}
	if file.namespace == "builtin" || file.namespace == "unsafe" {
		return "", nil
	}
	offset, err := file.source.Offset(pos.Line, pos.Character, encoding)
	if err != nil {
		return "", err
	}
	descriptors, found := file.descriptors[offset]
	if !found {
		return "", fmt.Errorf("unsupported external Go declaration at %s:%d", path, offset)
	}
	if len(descriptors) == 0 {
		return "", nil
	}
	pkg := r.namespaces[file.namespace]
	if pkg == nil {
		// A newly referenced transitive package can share a known module.
		for name, candidate := range r.packages {
			if strings.HasPrefix(file.namespace, name+"/") && (pkg == nil || len(name) > len(pkg.Name)) {
				pkg = candidate
			}
		}
	}
	if pkg == nil {
		return "", fmt.Errorf("no baseline package identity for %s", file.namespace)
	}
	return scip.VerboseSymbolFormatter.FormatSymbol(&scip.Symbol{Scheme: "scip-go", Package: pkg, Descriptors: append([]*scip.Descriptor{{Name: file.namespace, Suffix: scip.Descriptor_Namespace}}, descriptors...)}), nil
}

func goReceiverName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.StarExpr:
		return goReceiverName(e.X)
	case *ast.IndexExpr:
		return goReceiverName(e.X)
	case *ast.IndexListExpr:
		return goReceiverName(e.X)
	case *ast.ParenExpr:
		return goReceiverName(e.X)
	}
	return ""
}

func externalGoNamespace(dir string) (string, error) {
	for root := dir; ; root = filepath.Dir(root) {
		data, err := os.ReadFile(filepath.Join(root, "go.mod"))
		if err == nil {
			for line := range strings.SplitSeq(string(data), "\n") {
				fields := strings.Fields(line)
				if len(fields) < 2 || fields[0] != "module" {
					continue
				}
				module := fields[1]
				if quoted, err := strconv.Unquote(module); err == nil {
					module = quoted
				}
				rel, err := filepath.Rel(root, dir)
				if err != nil {
					return "", err
				}
				if module == "std" {
					return filepath.ToSlash(rel), nil
				}
				if rel == "." {
					return module, nil
				}
				return module + "/" + filepath.ToSlash(rel), nil
			}
		}
		if filepath.Dir(root) == root {
			break
		}
	}
	return "", fmt.Errorf("cannot resolve external Go namespace for %s", dir)
}
