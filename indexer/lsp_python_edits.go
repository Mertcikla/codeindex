package indexer

import (
	"fmt"
	"path/filepath"
	"strings"

	pb "buf.build/gen/go/tldiagramcom/diagram/protocolbuffers/go/codeindex/v1"
	"github.com/mertcikla/codeindex/graph"
)

type pythonReference struct{ start, end int }
type pythonEdit struct {
	*bodyEdit
	references        []pythonReference
	beforeReferences  []pythonReference
	definitionOffsets []int
}

func pythonBodyEdit(before, after *graph.Source) (*pythonEdit, error) {
	parse := func(src *graph.Source) ([]bodySpan, []bool, []string, error) {
		lang := parserLanguage("python")
		tree := parseDeclarationTree(lang, src.Text)
		if tree == nil {
			return nil, nil, nil, fmt.Errorf("cannot parse Python")
		}
		defer tree.Release()
		if tree.RootNode().HasErrorOrMissing() {
			return nil, nil, nil, fmt.Errorf("invalid Python syntax")
		}
		var bodies []bodySpan
		var supported []bool
		var docs []string
		var walk func(*tsNode)
		walk = func(n *tsNode) {
			if n == nil {
				return
			}

			if n.Kind() == "function_definition" {
				body := n.ChildByFieldName("body")
				if body == nil {
					return
				}
				ok := n.ChildByFieldName("return_type") != nil && (n.Parent() == nil || n.Parent().Kind() != "decorated_definition") && !strings.HasPrefix(strings.TrimSpace(string(src.Text[n.StartByte():body.StartByte()])), "async ")
				method := false
				for p := n.Parent(); p != nil; p = p.Parent() {
					if p.Kind() == "class_definition" {
						method = true
						break
					}
				}
				if params := n.ChildByFieldName("parameters"); params != nil {
					for i := 0; i < params.NamedChildCount(); i++ {
						param := params.NamedChild(i)
						switch param.Kind() {
						case "typed_parameter", "typed_default_parameter", "positional_separator", "keyword_separator":
						default:
							text := string(src.Text[param.StartByte():param.EndByte()])
							if !method || i != 0 || (text != "self" && text != "cls") {
								ok = false
							}
						}
					}
				}
				doc := ""
				var first *tsNode
				for i := 0; i < body.NamedChildCount(); i++ {
					if child := body.NamedChild(i); child.Kind() != "comment" {
						first = child
						break
					}
				}
				if first != nil && first.Kind() == "expression_statement" {
					first = first.NamedChild(0)
				}
				if first != nil && (first.Kind() == "string" || first.Kind() == "concatenated_string") {
					doc = string(src.Text[first.StartByte():first.EndByte()])
				}
				bodies = append(bodies, bodySpan{int(body.StartByte()), int(body.EndByte())})
				supported = append(supported, ok)
				docs = append(docs, doc)
				return
			}
			for i := 0; i < n.NamedChildCount(); i++ {
				walk(n.NamedChild(i))
			}
		}
		walk(wrapNode(tree.RootNode(), lang))
		return bodies, supported, docs, nil
	}
	old, oldSupported, oldDocs, err := parse(before)
	if err != nil {
		return nil, err
	}
	current, supported, docs, err := parse(after)
	if err != nil {
		return nil, err
	}
	edit, err := newBodyEdit(before, after, old, current)
	if err != nil {
		return nil, err
	}
	for i, changed := range edit.changed {
		if changed && (!supported[i] || !oldSupported[i] || oldDocs[i] != docs[i]) {
			return nil, fmt.Errorf("inferred/decorated API, async function or docstring change requires SCIP")
		}
	}
	out := &pythonEdit{bodyEdit: edit}
	for _, src := range []*graph.Source{before, after} {
		lang := parserLanguage("python")
		tree := parseDeclarationTree(lang, src.Text)
		var unsupported bool
		var walk func(*tsNode)
		walk = func(n *tsNode) {
			if n == nil {
				return
			}
			if src == before && n.Kind() == "function_definition" {
				body, name := n.ChildByFieldName("body"), n.ChildByFieldName("name")
				if body != nil && name != nil {
					for i, b := range edit.oldBodies {
						if edit.changed[i] && int(body.StartByte()) == b.start {
							out.definitionOffsets = append(out.definitionOffsets, int(name.StartByte()))
							break
						}
					}
				}
			}
			start, end := int(n.StartByte()), int(n.EndByte())
			inside := edit.containsChanged(start, end)
			if src == before {
				inside = edit.containedBefore(start, end)
			}
			if inside {
				switch n.Kind() {
				case "function_definition", "class_definition", "decorated_definition", "global_statement", "nonlocal_statement", "import_statement", "import_from_statement", "future_import_statement", "delete_statement", "yield", "type_alias_statement":
					unsupported = true
					return
				case "assignment", "augmented_assignment", "named_expression":
					if n.ChildByFieldName("type") != nil {
						unsupported = true
						return
					}
					var check func(*tsNode)
					check = func(target *tsNode) {
						if target == nil {
							return
						}
						if target.Kind() == "attribute" || target.Kind() == "subscript" {
							unsupported = true
						}
						for i := 0; i < target.NamedChildCount(); i++ {
							check(target.NamedChild(i))
						}
					}
					check(n.ChildByFieldName("left"))
					check(n.ChildByFieldName("name"))
				case "call":
					callee := n.ChildByFieldName("function")
					if callee != nil {
						if callee.Kind() == "attribute" {
							callee = callee.ChildByFieldName("attribute")
						}
						if callee != nil {
							if pythonDynamicName(string(src.Text[callee.StartByte():callee.EndByte()])) {
								unsupported = true
								return
							}
						}
					}
				case "identifier":
					if src == after {
						out.references = append(out.references, pythonReference{start, end})
					} else {
						out.beforeReferences = append(out.beforeReferences, pythonReference{start, end})
					}
					return
				}
			}
			for i := 0; i < n.NamedChildCount(); i++ {
				walk(n.NamedChild(i))
			}
		}
		walk(wrapNode(tree.RootNode(), lang))
		tree.Release()
		if unsupported {
			return nil, fmt.Errorf("nested declaration, import, annotation or nonlocal/dynamic write requires SCIP")
		}
	}
	return out, nil
}

func pythonDynamicName(name string) bool {
	switch name {
	case "eval", "exec", "globals", "locals", "vars", "getattr", "setattr", "delattr", "type", "cast", "TypeVar", "NamedTuple", "TypedDict", "__import__":
		return true
	}
	return false
}

func pythonEdits(project *pb.Project, snap *pb.Snapshot, g *graph.Graph, base *IncrementalBase) (map[string]*pythonEdit, error) {
	if base == nil || base.Snapshot.ConfigHash != snap.ConfigHash || len(base.Snapshot.Warnings) > 0 || !base.Snapshot.GetManifest().GetComplete() || !snap.GetManifest().GetComplete() {
		return nil, fmt.Errorf("no compatible complete baseline")
	}
	if base.Snapshot.GitRevision != snap.GitRevision {
		return nil, fmt.Errorf("Python package revision changed; requires SCIP")
	}
	previous := map[string]*pb.SourceFile{}
	for _, f := range base.Snapshot.Manifest.Files {
		previous[f.Path] = f
	}
	if len(previous) != len(snap.Manifest.Files) {
		return nil, fmt.Errorf("file membership changed")
	}
	out := map[string]*pythonEdit{}
	for _, f := range snap.Manifest.Files {
		old := previous[f.Path]
		if old == nil || old.Mode != f.Mode || !filepath.IsLocal(f.Path) {
			return nil, fmt.Errorf("file membership/mode changed")
		}
		if old.Hash == f.Hash {
			continue
		}
		before, after := base.Graph.Sources[f.Path], g.Sources[f.Path]
		if before == nil || after == nil || after.Language != "python" || strings.HasSuffix(f.Path, ".pyi") {
			return nil, fmt.Errorf("Python configuration/dependency/stub inputs changed: %s", f.Path)
		}
		if project.Root != "." && project.Root != "" && !strings.HasPrefix(f.Path, strings.TrimSuffix(project.Root, "/")+"/") {
			return nil, fmt.Errorf("change outside Python project")
		}
		edit, err := pythonBodyEdit(before, after)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.Path, err)
		}
		out[f.Path] = edit
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no changed Python documents")
	}
	return out, nil
}
