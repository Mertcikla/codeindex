package indexer

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"

	pb "buf.build/gen/go/tldiagramcom/diagram/protocolbuffers/go/codeindex/v1"
	"github.com/mertcikla/codeindex/graph"
)

type rustReference struct {
	start, end int
	operator   bool
}
type rustEdit struct {
	*bodyEdit
	references        []rustReference
	operatorSensitive bool
}

func rustBodyEdit(before, after *graph.Source) (*rustEdit, error) {
	parse := func(src *graph.Source) ([]bodySpan, []bool, error) {
		lang := parserLanguage("rust")
		tree := parseDeclarationTree(lang, src.Text)
		if tree == nil {
			return nil, nil, fmt.Errorf("cannot parse Rust")
		}
		defer tree.Release()
		if tree.RootNode().HasErrorOrMissing() {
			return nil, nil, fmt.Errorf("invalid Rust syntax")
		}
		var bodies []bodySpan
		var unsupported []bool
		var walk func(*tsNode)
		walk = func(n *tsNode) {
			if n == nil {
				return
			}
			if n.Kind() == "function_item" {
				if body := n.ChildByFieldName("body"); body != nil {
					bodies = append(bodies, bodySpan{int(body.StartByte()), int(body.EndByte())})
					bad := false
					for i := 0; i < n.NamedChildCount(); i++ {
						child := n.NamedChild(i)
						if child.Kind() == "function_modifiers" {
							text := src.Text[child.StartByte():child.EndByte()]
							bad = bad || bytes.Contains(text, []byte("const")) || bytes.Contains(text, []byte("async"))
						}
					}
					if result := n.ChildByFieldName("return_type"); result != nil && result.Kind() == "abstract_type" {
						bad = true
					}
					unsupported = append(unsupported, bad)
					return
				}
			}
			for i := 0; i < n.NamedChildCount(); i++ {
				walk(n.NamedChild(i))
			}
		}
		walk(wrapNode(tree.RootNode(), lang))
		return bodies, unsupported, nil
	}
	old, oldUnsupported, err := parse(before)
	if err != nil {
		return nil, err
	}
	current, unsupported, err := parse(after)
	if err != nil {
		return nil, err
	}
	edit, err := newBodyEdit(before, after, old, current)
	if err != nil {
		return nil, err
	}
	// Const evaluation and hidden return types can change metadata in callers.
	for i, changed := range edit.changed {
		if changed && (unsupported[i] || oldUnsupported[i]) {
			return nil, fmt.Errorf("const, async or opaque return change requires SCIP")
		}
	}
	out := &rustEdit{bodyEdit: edit}
	// Both old and new bodies must avoid macro expansions, cfg attributes and
	// nested items: they can introduce global definitions and relationships or
	// change their counter identities outside the edited text.
	for _, src := range []*graph.Source{before, after} {
		lang := parserLanguage("rust")
		tree := parseDeclarationTree(lang, src.Text)
		var unsupported bool
		var walk func(*tsNode)
		walk = func(n *tsNode) {
			if n == nil {
				return
			}
			start, end := int(n.StartByte()), int(n.EndByte())
			inside := edit.containsChanged(start, end)
			if src == before {
				inside = edit.containedBefore(start, end)
			}
			if inside {
				switch n.Kind() {
				case "binary_expression", "unary_expression", "index_expression", "compound_assignment_expr", "try_expression", "await_expression":
					if src == after {
						out.operatorSensitive = true
					}
				}
				if src == after {
					if op := n.ChildByFieldName("operator"); op != nil && rustOperatorToken(src.Text[op.StartByte():op.EndByte()]) {
						out.references = append(out.references, rustReference{int(op.StartByte()), int(op.EndByte()), true})
					}
				}
				switch n.Kind() {
				case "async_block", "const_block", "gen_block", "macro_invocation", "attribute_item", "inner_attribute_item", "function_item", "struct_item", "enum_item", "union_item", "impl_item", "trait_item", "mod_item", "type_item", "const_item", "static_item", "use_declaration", "macro_definition":
					unsupported = true
					return
				case "identifier", "type_identifier", "field_identifier", "shorthand_field_identifier", "self", "super", "crate", "lifetime":
					if src == after {
						out.references = append(out.references, rustReference{start, end, false})
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
			return nil, fmt.Errorf("macro, attribute or nested item requires SCIP")
		}
	}
	return out, nil
}

func rustOperatorToken(text []byte) bool {
	switch string(text) {
	case "+", "-", "*", "/", "%", "<<", ">>", "&", "|", "^", "==", "!=", "<", ">", "<=", ">=", "+=", "-=", "*=", "/=", "%=", "<<=", ">>=", "&=", "|=", "^=", "!", "[", "]", "?":
		return true
	}
	return false
}

func rustEdits(project *pb.Project, snap *pb.Snapshot, g *graph.Graph, base *IncrementalBase) (map[string]*rustEdit, error) {
	if base == nil || base.Snapshot.ConfigHash != snap.ConfigHash || len(base.Snapshot.Warnings) > 0 || !base.Snapshot.GetManifest().GetComplete() || !snap.GetManifest().GetComplete() {
		return nil, fmt.Errorf("no compatible complete baseline")
	}
	previous := map[string]*pb.SourceFile{}
	for _, f := range base.Snapshot.Manifest.Files {
		previous[f.Path] = f
	}
	if len(previous) != len(snap.Manifest.Files) {
		return nil, fmt.Errorf("file membership changed")
	}
	out := map[string]*rustEdit{}
	for _, f := range snap.Manifest.Files {
		old := previous[f.Path]
		if old == nil || old.Mode != f.Mode {
			return nil, fmt.Errorf("file membership/mode changed")
		}
		if !filepath.IsLocal(f.Path) {
			return nil, fmt.Errorf("invalid Rust source path")
		}
		if old.Hash == f.Hash {
			continue
		}
		before, after := base.Graph.Sources[f.Path], g.Sources[f.Path]
		if before == nil || after == nil || before.Language != "rust" || after.Language != "rust" {
			return nil, fmt.Errorf("Cargo/build inputs changed: %s", f.Path)
		}
		if project.Root != "." && project.Root != "" && !strings.HasPrefix(f.Path, strings.TrimSuffix(project.Root, "/")+"/") {
			return nil, fmt.Errorf("change outside Rust project")
		}
		edit, err := rustBodyEdit(before, after)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.Path, err)
		}
		out[f.Path] = edit
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no changed Rust documents")
	}
	return out, nil
}
