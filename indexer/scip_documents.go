package indexer

import (
	"context"
	"fmt"
	"io"
	"os"

	pb "buf.build/gen/go/tldiagramcom/diagram/protocolbuffers/go/codeindex/v1"
	"github.com/mertcikla/codeindex/graph"
	scip "github.com/scip-code/scip/bindings/go/scip"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func readSCIPDocuments(ctx context.Context, g *graph.Graph, project *pb.Project, reader io.Reader) ([]*scip.Document, error) {
	var docs []*scip.Document
	visitor := &scip.IndexVisitor{VisitDocument: func(_ context.Context, doc *scip.Document) error {
		if source := g.Sources[scipDocumentPath(project, doc.RelativePath)]; source != nil {
			docs = append(docs, doc)
		}
		return ctx.Err()
	}}
	err := visitor.ParseStreaming(ctx, reader)
	return docs, err
}

// projectVersionRewriter updates only the defining package; dependencies retain their versions.
func projectVersionRewriter(previous, current []*scip.Document, packageName string) (func(string) string, error) {
	moduleVersion := func(docs []*scip.Document) (string, string, error) {
		key, version := "", ""
		for _, doc := range docs {
			for _, info := range doc.Symbols {
				symbol, err := scip.ParseSymbol(info.Symbol)
				if err != nil || symbol.Package == nil {
					continue
				}
				if packageName != "" && symbol.Package.Name != packageName {
					continue
				}
				v := symbol.Package.Version
				symbol.Descriptors, symbol.Package.Version = nil, ""
				k := scip.VerboseSymbolFormatter.FormatSymbol(symbol)
				if key != "" && (key != k || version != v) {
					return "", "", fmt.Errorf("ambiguous project package version")
				}
				key, version = k, v
			}
		}
		return key, version, nil
	}
	oldKey, oldVersion, err := moduleVersion(previous)
	if err != nil {
		return nil, err
	}
	newKey, newVersion, err := moduleVersion(current)
	if err != nil || oldKey == "" || oldKey != newKey {
		return nil, fmt.Errorf("project package identity changed")
	}
	return func(raw string) string {
		if oldVersion == newVersion {
			return raw
		}
		symbol, err := scip.ParseSymbol(raw)
		if err != nil || symbol.Package == nil || symbol.Package.Version != oldVersion {
			return raw
		}
		descriptors := symbol.Descriptors
		symbol.Descriptors, symbol.Package.Version = nil, ""
		if scip.VerboseSymbolFormatter.FormatSymbol(symbol) != oldKey {
			return raw
		}
		symbol.Descriptors, symbol.Package.Version = descriptors, newVersion
		return scip.VerboseSymbolFormatter.FormatSymbol(symbol)
	}, nil
}

func writeSCIPDocuments(ctx context.Context, artifact, version string, documents []*scip.Document) error {
	output, err := os.Create(artifact + ".merged")
	if err != nil {
		return err
	}
	defer func() { _ = output.Close(); _ = os.Remove(output.Name()) }()
	write := func(field protowire.Number, message proto.Message) error {
		data, err := proto.Marshal(message)
		if err != nil {
			return err
		}
		header := protowire.AppendTag(nil, field, protowire.BytesType)
		header = protowire.AppendVarint(header, uint64(len(data)))
		if _, err := output.Write(header); err != nil {
			return err
		}
		_, err = output.Write(data)
		return err
	}
	if err := write(1, &scip.Metadata{ToolInfo: &scip.ToolInfo{Version: version}}); err != nil {
		return err
	}
	for _, doc := range documents {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := write(2, doc); err != nil {
			return err
		}
	}
	if err := output.Close(); err != nil {
		return err
	}
	return os.Rename(output.Name(), artifact)
}
