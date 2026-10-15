package indexer

import (
	"fmt"
	"path/filepath"
	"strings"

	pb "buf.build/gen/go/tldiagramcom/diagram/protocolbuffers/go/codeindex/v1"
	"github.com/mertcikla/codeindex/graph"
)

type typeScriptReference struct {
	start, end  int
	constructor bool
}
type typeScriptEdit struct {
	*bodyEdit
	references []typeScriptReference
}

func typeScriptBodyEdit(before, after *graph.Source) (*typeScriptEdit, error) {
	if after.Language != "typescript" && after.Language != "tsx" {
		return nil, fmt.Errorf("typed TypeScript functions are required")
	}
	parse := func(src *graph.Source) ([]bodySpan, []bool, error) {
		lang := parserLanguage(src.Language)
		tree := parseDeclarationTree(lang, src.Text)
		if tree == nil {
			return nil, nil, fmt.Errorf("cannot parse TypeScript")
		}
		defer tree.Release()
		if tree.RootNode().HasErrorOrMissing() {
			return nil, nil, fmt.Errorf("invalid TypeScript syntax")
		}
		var bodies []bodySpan
		var supported []bool
		var walk func(*tsNode)
		walk = func(n *tsNode) {
			if n == nil {
				return
			}
			switch n.Kind() {
			case "function_declaration", "generator_function_declaration", "method_definition", "arrow_function", "function_expression", "generator_function":
				if body := n.ChildByFieldName("body"); body != nil {
					bodies = append(bodies, bodySpan{int(body.StartByte()), int(body.EndByte())})
					// Inferred return types can change callers and symbol metadata
					// outside the edited file. Explicit annotations keep that API
					// fixed; constructors/accessors remain on SCIP.
					name := declarationName(n, src.Text)
					supported = append(supported, n.ChildByFieldName("return_type") != nil && name != "constructor")
					return
				}
			}
			for i := 0; i < n.NamedChildCount(); i++ {
				walk(n.NamedChild(i))
			}
		}
		walk(wrapNode(tree.RootNode(), lang))
		return bodies, supported, nil
	}
	old, oldSupported, err := parse(before)
	if err != nil {
		return nil, err
	}
	current, supported, err := parse(after)
	if err != nil {
		return nil, err
	}
	edit, err := newBodyEdit(before, after, old, current)
	if err != nil {
		return nil, err
	}
	for i, changed := range edit.changed {
		if changed && (!supported[i] || !oldSupported[i]) {
			return nil, fmt.Errorf("inferred API or constructor change requires SCIP")
		}
	}
	lang := parserLanguage(after.Language)
	tree := parseDeclarationTree(lang, after.Text)
	if tree == nil {
		return nil, fmt.Errorf("cannot parse TypeScript references")
	}
	defer tree.Release()
	out := &typeScriptEdit{bodyEdit: edit}
	var unsupported bool
	var walk func(*tsNode)
	walk = func(n *tsNode) {
		if n == nil {
			return
		}
		start, end := int(n.StartByte()), int(n.EndByte())
		if !edit.containsChanged(start, end) {
			for i := 0; i < n.NamedChildCount(); i++ {
				walk(n.NamedChild(i))
			}
			return
		}
		switch n.Kind() {
		case "object", "object_pattern", "object_type", "class", "class_declaration", "type_alias_declaration", "interface_declaration":
			// These can create counter-dependent global SCIP descriptors or
			// new implementation relationships from inside a function body.
			unsupported = true
			return
		case "identifier", "property_identifier", "type_identifier", "shorthand_property_identifier", "shorthand_property_identifier_pattern", "string":
			ctor := false
			for parent := n.Parent(); parent != nil; parent = parent.Parent() {
				if parent.Kind() == "new_expression" {
					if target := parent.ChildByFieldName("constructor"); target != nil && start >= int(target.StartByte()) && end <= int(target.EndByte()) {
						ctor = true
					}
					break
				}
				if parent.Kind() != "member_expression" && parent.Kind() != "nested_type_identifier" {
					break
				}
			}
			out.references = append(out.references, typeScriptReference{start, end, ctor})
			return
		}
		for i := 0; i < n.NamedChildCount(); i++ {
			walk(n.NamedChild(i))
		}
	}
	walk(wrapNode(tree.RootNode(), lang))
	if unsupported {
		return nil, fmt.Errorf("body introduces object/type identities requiring SCIP")
	}
	return out, nil
}

func (e *bodyEdit) containsChanged(start, end int) bool {
	for i, b := range e.newBodies {
		if e.changed[i] && start >= b.start && end <= b.end {
			return true
		}
	}
	return false
}
func (e *bodyEdit) containedBefore(start, end int) bool {
	for i, b := range e.oldBodies {
		if e.changed[i] && start >= b.start && end <= b.end {
			return true
		}
	}
	return false
}

func typeScriptEdits(project *pb.Project, snap *pb.Snapshot, g *graph.Graph, base *IncrementalBase) (map[string]*typeScriptEdit, error) {
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
	out := map[string]*typeScriptEdit{}
	for _, f := range snap.Manifest.Files {
		old := previous[f.Path]
		if old == nil || old.Mode != f.Mode {
			return nil, fmt.Errorf("file membership/mode changed")
		}
		if old.Hash == f.Hash {
			continue
		}
		before, after := base.Graph.Sources[f.Path], g.Sources[f.Path]
		if before == nil || after == nil {
			return nil, fmt.Errorf("build or dependency inputs changed: %s", f.Path)
		}
		if project.Root != "." && project.Root != "" && !strings.HasPrefix(f.Path, strings.TrimSuffix(project.Root, "/")+"/") {
			return nil, fmt.Errorf("change outside TypeScript project")
		}
		if !filepath.IsLocal(f.Path) {
			return nil, fmt.Errorf("invalid source path")
		}
		edit, err := typeScriptBodyEdit(before, after)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.Path, err)
		}
		out[f.Path] = edit
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no changed TypeScript documents")
	}
	return out, nil
}
