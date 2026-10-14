package indexer

import (
	"bytes"
	"compress/zlib"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	pb "buf.build/gen/go/tldiagramcom/diagram/protocolbuffers/go/codeindex/v1"
	"github.com/mertcikla/codeindex/graph"
	scip "github.com/scip-code/scip/bindings/go/scip"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// This is a private cache, not an exported SCIP index. If the importer starts
// consuming additional fields, bump this version and relationshipVersion so
// project fingerprints invalidate older, insufficient caches.
const scipArtifactMagic = "SCIP\x00"
const scipArtifactVersion byte = 1

// compactSCIPArtifact retains only documents and fields consumed by our
// importer. Streaming bounds memory to one document plus the compressed output.
// Original artifacts stay untouched, including caller-supplied files.
func compactSCIPArtifact(ctx context.Context, g *graph.Graph, project *pb.Project, raw io.Reader) ([]byte, error) {
	var output bytes.Buffer
	output.WriteString(scipArtifactMagic)
	output.WriteByte(scipArtifactVersion)
	compressed := zlib.NewWriter(&output)
	defer func() { _ = compressed.Close() }()
	write := func(field protowire.Number, msg proto.Message) error {
		data, err := proto.MarshalOptions{Deterministic: true}.Marshal(msg)
		if err != nil {
			return err
		}
		header := protowire.AppendTag(nil, field, protowire.BytesType)
		header = protowire.AppendVarint(header, uint64(len(data)))
		if _, err := compressed.Write(header); err != nil {
			return err
		}
		_, err = compressed.Write(data)
		return err
	}
	visitor := &scip.IndexVisitor{
		VisitMetadata: func(_ context.Context, metadata *scip.Metadata) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if metadata.GetToolInfo() == nil {
				return fmt.Errorf("SCIP metadata lacks tool info")
			}
			return write(1, &scip.Metadata{ToolInfo: &scip.ToolInfo{Version: metadata.GetToolInfo().GetVersion()}})
		},
		VisitDocument: func(_ context.Context, document *scip.Document) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			path := scipDocumentPath(project, document.GetRelativePath())
			source := g.Sources[path]
			if path == "" || source == nil {
				return nil
			}
			// Embedded text is redundant with captured sources, but validate it
			// before dropping it so cache construction cannot hide mismatches.
			if document.GetText() != "" && graph.Hash([]byte(document.GetText())) != source.Hash {
				return fmt.Errorf("SCIP source mismatch: %s", path)
			}
			retained := &scip.Document{RelativePath: document.RelativePath, PositionEncoding: document.PositionEncoding}
			encoding, declared := scipPositionEncoding(document)
			for _, occurrence := range document.Occurrences {
				if occurrence.GetSymbol() == "" || strings.HasPrefix(occurrence.GetSymbol(), "local ") {
					continue
				}
				if _, ok := scipAnchorResilient(source, occurrence, encoding, declared); !ok {
					continue
				}
				kept := proto.Clone(occurrence).(*scip.Occurrence)
				kept.OverrideDocumentation = nil
				kept.SyntaxKind = scip.SyntaxKind_UnspecifiedSyntaxKind
				kept.Diagnostics = nil
				kept.ProtoReflect().SetUnknown(nil)
				retained.Occurrences = append(retained.Occurrences, kept)
			}
			for _, info := range document.Symbols {
				if info.GetSymbol() == "" || strings.HasPrefix(info.GetSymbol(), "local ") {
					continue
				}
				kept := &scip.SymbolInformation{
					Symbol: info.Symbol, Kind: info.Kind, DisplayName: info.DisplayName,
					Documentation: info.Documentation,
				}
				if text := info.GetSignatureDocumentation().GetText(); text != "" {
					kept.SignatureDocumentation = &scip.Signature{Text: text}
				}
				for _, relationship := range info.Relationships {
					if relationship.GetSymbol() != "" && (relationship.GetIsImplementation() || relationship.GetIsTypeDefinition()) {
						kept.Relationships = append(kept.Relationships, &scip.Relationship{
							Symbol: relationship.Symbol, IsImplementation: relationship.IsImplementation, IsTypeDefinition: relationship.IsTypeDefinition,
						})
					}
				}
				retained.Symbols = append(retained.Symbols, kept)
			}
			return write(2, retained)
		},
		// ExternalSymbols are never visited by importSCIPReader.
	}
	if err := visitor.ParseStreaming(ctx, raw); err != nil {
		return nil, fmt.Errorf("compact SCIP artifact: %w", err)
	}
	if err := compressed.Close(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

// scipArtifactReader accepts older raw caches too. Caller-provided SCIP files
// continue through importSCIP directly and always use the original wire format.
func scipArtifactReader(data []byte) (io.ReadCloser, error) {
	if !bytes.HasPrefix(data, []byte(scipArtifactMagic)) {
		return io.NopCloser(bytes.NewReader(data)), nil
	}
	if len(data) <= len(scipArtifactMagic) || data[len(scipArtifactMagic)] != scipArtifactVersion {
		return nil, fmt.Errorf("unsupported SCIP artifact cache version")
	}
	r, err := zlib.NewReader(bytes.NewReader(data[len(scipArtifactMagic)+1:]))
	if err != nil {
		return nil, fmt.Errorf("open SCIP artifact cache: %w", err)
	}
	return r, nil
}

func scipDocumentPath(project *pb.Project, relativePath string) string {
	rel := filepath.ToSlash(filepath.Clean(relativePath))
	if rel == "." || strings.HasPrefix(rel, "../") || filepath.IsAbs(rel) {
		return ""
	}
	path := rel
	if root := strings.Trim(project.Root, "/"); root != "" && root != "." {
		path = root + "/" + rel
	}
	if isTestSource(path) {
		return ""
	}
	return path
}

func scipPositionEncoding(d *scip.Document) (string, bool) {
	switch d.GetPositionEncoding() {
	case scip.PositionEncoding_UTF8CodeUnitOffsetFromLineStart:
		return "utf-8", true
	case scip.PositionEncoding_UTF16CodeUnitOffsetFromLineStart:
		return "utf-16", true
	case scip.PositionEncoding_UTF32CodeUnitOffsetFromLineStart:
		return "utf-32", true
	default:
		return "utf-8", false
	}
}
