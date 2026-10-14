package indexer

import (
	"encoding/json"
	"path"
	"sort"
	"strings"

	pb "buf.build/gen/go/tldiagramcom/diagram/protocolbuffers/go/codeindex/v1"
	"github.com/mertcikla/codeindex/graph"
	"gopkg.in/yaml.v3"
)

// addPackageExports records public exposure as file references. Entry points
// have separate logical identities; equivalent conditions share an edge and
// retain their individual source witnesses. Recompute after file facts on every
// build, since either the manifest or a target may have changed.
func addPackageExports(g *graph.Graph) {
	files := map[string]*pb.CodeFact{}
	for _, f := range g.Facts {
		if f.Kind == pb.FactKind_FACT_KIND_FILE && f.Anchor != nil {
			files[f.Anchor.Path] = f
		}
	}
	paths := make([]string, 0, len(g.Sources))
	for p := range g.Sources {
		if path.Base(p) == "package.json" || path.Base(p) == "jsr.json" {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	for _, p := range paths {
		src := g.Sources[p]
		if files[p] == nil || !json.Valid(src.Text) {
			continue
		}
		var doc yaml.Node
		if yaml.Unmarshal(src.Text, &doc) != nil || len(doc.Content) != 1 {
			continue
		}
		exports := exportField(doc.Content[0], "exports")
		if exports == nil {
			continue
		}
		entries := map[string]*yaml.Node{".": exports}
		if exports.Kind == yaml.MappingNode {
			subpaths, conditions := false, false
			for i := 0; i+1 < len(exports.Content); i += 2 {
				if strings.HasPrefix(exports.Content[i].Value, ".") {
					subpaths = true
				} else {
					conditions = true
				}
			}
			if subpaths && conditions {
				continue
			} // Invalid npm mixed-key object.
			if subpaths {
				entries = map[string]*yaml.Node{}
				for i := 0; i+1 < len(exports.Content); i += 2 {
					entries[exports.Content[i].Value] = exports.Content[i+1]
				}
			}
		}
		names := make([]string, 0, len(entries))
		for name := range entries {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if name != "." && !strings.HasPrefix(name, "./") || strings.Contains(name, "*") {
				continue
			}
			entry := entries[name]
			edges := map[string]*pb.EdgeFact{}
			var visit func(*yaml.Node, []string)
			visit = func(n *yaml.Node, conditions []string) {
				switch n.Kind {
				case yaml.MappingNode:
					for i := 0; i+1 < len(n.Content); i += 2 {
						visit(n.Content[i+1], append(append([]string(nil), conditions...), n.Content[i].Value))
					}
				case yaml.SequenceNode:
					// Fallback arrays depend on runtime resolution. Do not claim all
					// alternatives are exposed simultaneously.
					return
				case yaml.ScalarNode:
					if n.Tag != "!!str" {
						return
					}
					target := exportLocalPath(path.Dir(p), n.Value)
					if target == "" {
						return
					}
					resolved, via := exportImplementationTarget(g, target)
					to := files[resolved]
					if to == nil || resolved == p {
						return
					}
					start, _ := src.OffsetForLineColumn(n.Line, n.Column, 0, 0)
					end := start
					// JSON strings may contain escapes; scan the actual token for an
					// exact byte span rather than using the decoded value length.
					if start < len(src.Text) && src.Text[start] == '"' {
						for end = start + 1; end < len(src.Text); end++ {
							if src.Text[end] == '\\' {
								end++
								continue
							}
							if src.Text[end] == '"' {
								end++
								break
							}
						}
					}
					anchor := src.Anchor(start, end)
					metadata, _ := json.Marshal(struct {
						EntryPoint string   `json:"entryPoint"`
						Conditions []string `json:"conditions"`
						Target     string   `json:"target"`
						SourceMap  string   `json:"sourceMap,omitempty"`
					}{name, conditions, n.Value, via})
					evidence := &pb.Evidence{Producer: "package-exports", Version: "1", OriginalId: string(metadata), Anchor: anchor, Derivation: "public package export"}
					edge := edges[resolved]
					if edge == nil {
						edge = g.AddEdgeFact(pb.EdgeKind_EDGE_KIND_REFERENCES, files[p].Id, to.Id, "", anchor, evidence)
						edge.LogicalKey += "|public-export|" + name
						edges[resolved] = edge
					} else {
						edge.Evidence = append(edge.Evidence, evidence)
					}
					if via != "" {
						mapping := g.Sources[via]
						edge.Evidence = append(edge.Evidence, &pb.Evidence{
							Producer: "package-exports", Version: "1", OriginalId: string(metadata),
							Anchor:     mapping.Anchor(0, len(mapping.Text)),
							Derivation: "single-source source map resolves public export target",
						})
					}
				}
			}
			visit(entry, nil)
		}
	}
}

func exportField(n *yaml.Node, name string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == name {
			return n.Content[i+1]
		}
	}
	return nil
}

// Resolve only package-relative paths that stay inside the repository. Export
// wildcards need concrete expansion semantics and are deliberately left out.
func exportLocalPath(dir, target string) string {
	if !strings.HasPrefix(target, "./") || strings.ContainsAny(target, "*\\?#") {
		return ""
	}
	p := path.Clean(path.Join(dir, target))
	if p == ".." || strings.HasPrefix(p, "../") {
		return ""
	}
	return p
}

func exportImplementationTarget(g *graph.Graph, target string) (string, string) {
	src := g.Sources[target]
	if src == nil || languageFamily(src.Language) != familyWeb {
		return "", ""
	}
	// A captured output with an explicit sourceMappingURL and one source is
	// verifiable. Never infer dist -> src or .js -> .ts from names alone.
	marker := "//# sourceMappingURL="
	pos := strings.LastIndex(string(src.Text), marker)
	if pos >= 0 {
		url := strings.TrimSpace(strings.SplitN(string(src.Text[pos+len(marker):]), "\n", 2)[0])
		if strings.ContainsAny(url, ":\\?#") || strings.HasPrefix(url, "/") {
			return "", ""
		}
		mapPath := exportLocalPath(path.Dir(target), "./"+url)
		if mapPath == "" {
			return "", ""
		}
		mapping := g.Sources[mapPath]
		if mapping == nil {
			return "", ""
		}
		var m struct {
			Version    int      `json:"version"`
			File       string   `json:"file"`
			SourceRoot string   `json:"sourceRoot"`
			Sources    []string `json:"sources"`
		}
		if json.Unmarshal(mapping.Text, &m) != nil || m.Version != 3 || len(m.Sources) != 1 || strings.Contains(m.SourceRoot, ":") || strings.Contains(m.Sources[0], ":") {
			return "", ""
		}
		if m.File != "" && path.Clean(path.Join(path.Dir(mapPath), m.File)) != target {
			return "", ""
		}
		p := path.Clean(path.Join(path.Dir(mapPath), m.SourceRoot, m.Sources[0]))
		if strings.HasPrefix(p, "../") || p == ".." || path.IsAbs(m.SourceRoot) || path.IsAbs(m.Sources[0]) {
			return "", ""
		}
		original := g.Sources[p]
		if original == nil || languageFamily(original.Language) != familyWeb || p == target {
			return "", ""
		}
		return p, mapPath
	}
	// The manifest explicitly targets this captured file. Keep that exact
	// target (including build output) when no map is available; do not claim
	// it resolves to an inferred TypeScript implementation.
	return target, ""
}
