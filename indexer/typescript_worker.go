package indexer

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	pb "buf.build/gen/go/tldiagramcom/diagram/protocolbuffers/go/codeindex/v1"
	"github.com/mertcikla/codeindex/graph"
	"github.com/mertcikla/codeindex/tools"
	scip "github.com/scip-code/scip/bindings/go/scip"
	"google.golang.org/protobuf/proto"
)

//go:embed typescript_worker.cjs
var typeScriptWorkerSource string

type typeScriptCompiler struct {
	mu          sync.Mutex
	ctx         context.Context
	root        string
	cfgTool     string
	projectDir  string
	cmd         *exec.Cmd
	input       io.WriteCloser
	output      *bufio.Scanner
	ready       chan struct{}
	stderr      bytes.Buffer
	err         error
	stale       bool
	fingerprint string
	data        []byte
	documents   []*scip.Document
}
type compilerReply struct {
	Ready bool
	Data  []byte
	Error string
}
type compilerRefresh struct{ documents []*scip.Document }

func (s *WatchSession) startTypeScriptCompiler() {
	s.compiler = &typeScriptCompiler{ctx: s.ctx, root: s.root, cfgTool: s.cfg.Tools.SCIPTypeScript, ready: make(chan struct{})}
	c := s.compiler
	go func() {
		defer close(c.ready)
		c.mu.Lock()
		defer c.mu.Unlock()
		projects, err := DiscoverProjects(c.ctx, c.root, nil, nil)
		if err != nil {
			c.err = err
			return
		}
		for _, pr := range projects {
			if languageFamily(pr.Language) == familyWeb {
				c.projectDir = filepath.Join(c.root, filepath.FromSlash(pr.Root))
				c.err = c.start()
				return
			}
		}
		c.err = fmt.Errorf("no TypeScript project")
	}()
}
func (c *typeScriptCompiler) start() error {
	tool, err := tools.ResolveName(c.cfgTool)
	if err != nil {
		return err
	}
	tool, err = filepath.EvalSymlinks(tool)
	if err != nil {
		return err
	}
	// Private producer APIs are gated to the supported version and installation.
	if toolVersion(c.ctx, tool, []string{"--version"}) != "0.4.0" {
		return fmt.Errorf("unsupported TypeScript producer")
	}
	c.cmd = exec.CommandContext(c.ctx, "node", "-e", typeScriptWorkerSource, filepath.Dir(tool), c.projectDir)
	c.stderr.Reset()
	c.cmd.Stderr = &c.stderr
	c.input, err = c.cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := c.cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err = c.cmd.Start(); err != nil {
		return err
	}
	c.output = bufio.NewScanner(stdout)
	c.output.Buffer(make([]byte, 64<<10), 64<<20)
	reply, err := c.read()
	if err != nil {
		return err
	}
	if !reply.Ready {
		return fmt.Errorf("TypeScript compiler did not start")
	}
	return nil
}
func (c *typeScriptCompiler) read() (compilerReply, error) {
	var r compilerReply
	if !c.output.Scan() {
		if c.cmd != nil {
			_ = c.cmd.Wait()
		}
		return r, fmt.Errorf("TypeScript compiler disconnected: %v %s", c.output.Err(), c.stderr.String())
	}
	if err := json.Unmarshal(c.output.Bytes(), &r); err != nil {
		return r, err
	}
	if r.Error != "" {
		return r, fmt.Errorf("%s", r.Error)
	}
	return r, nil
}
func (c *typeScriptCompiler) close() {
	if c == nil {
		return
	}
	<-c.ready
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stop()
}
func (c *typeScriptCompiler) stop() {
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
		_ = c.cmd.Wait()
		c.cmd = nil
	}
	c.documents = nil
	c.data = nil
}

func (s *WatchSession) refreshTypeScriptCompiler(ctx context.Context, root string, project *pb.Project, snap *pb.Snapshot, g *graph.Graph, base *IncrementalBase, cached graph.ProjectArtifact) (*compilerRefresh, error) {
	c := s.compiler
	if c == nil || root != c.root {
		return nil, fmt.Errorf("no warm TypeScript compiler")
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.ready:
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if filepath.Join(root, filepath.FromSlash(project.Root)) != c.projectDir {
		return nil, fmt.Errorf("unsupported TypeScript project")
	}
	if c.stale {
		c.stop()
		c.err = c.start()
		c.stale = false
	}
	if c.err != nil {
		return nil, c.err
	}
	// Same complete inventory/configuration/revision; only captured web sources may move.
	before, after := map[string]string{}, map[string]string{}
	if base == nil || base.Snapshot.ConfigHash != snap.ConfigHash || len(base.Snapshot.Warnings) > 0 || !base.Snapshot.GetManifest().GetComplete() || !snap.GetManifest().GetComplete() {
		return nil, fmt.Errorf("no compatible TypeScript baseline")
	}
	old := map[string]*pb.SourceFile{}
	for _, f := range base.Snapshot.Manifest.Files {
		old[f.Path] = f
	}
	if len(old) != len(snap.Manifest.Files) {
		return nil, fmt.Errorf("TypeScript membership changed")
	}
	for _, f := range snap.Manifest.Files {
		prior := old[f.Path]
		if prior == nil || prior.Mode != f.Mode {
			return nil, fmt.Errorf("TypeScript membership changed")
		}
		if prior.Hash == f.Hash {
			continue
		}
		a, b := base.Graph.Sources[f.Path], g.Sources[f.Path]
		if a == nil || b == nil || languageFamily(b.Language) != familyWeb || scipDocumentPath(project, projectRelative(project, f.Path)) != f.Path {
			return nil, fmt.Errorf("TypeScript configuration or non-web inputs changed")
		}
		before[projectRelative(project, f.Path)] = string(a.Text)
		after[projectRelative(project, f.Path)] = string(b.Text)
	}
	if len(after) == 0 {
		return nil, fmt.Errorf("no changed TypeScript sources")
	}
	if c.fingerprint != cached.Fingerprint || !bytes.Equal(c.data, cached.Data) {
		reader, err := scipArtifactReader(cached.Data)
		if err != nil {
			return nil, err
		}
		docs, err := readSCIPDocuments(ctx, base.Graph, project, reader)
		_ = reader.Close()
		if err != nil {
			return nil, err
		}
		c.documents, c.data, c.fingerprint = docs, cached.Data, cached.Fingerprint
	}
	request, _ := json.Marshal(map[string]any{"before": before, "after": after})
	if _, err := fmt.Fprintf(c.input, "%s\n", request); err != nil {
		c.stale = true
		return nil, err
	}
	// A timed out request invalidates the process; never consume its stale reply.
	process := c.cmd.Process
	stopCancel := context.AfterFunc(ctx, func() { _ = process.Kill() })
	defer stopCancel()
	reply, err := c.read()
	if err != nil {
		c.stale = true
		if ctx.Err() != nil {
			c.stale = true
			return nil, ctx.Err()
		}
		return nil, err
	}
	compact, err := compactSCIPArtifact(ctx, g, project, bytes.NewReader(reply.Data))
	if err != nil {
		return nil, err
	}
	reader, err := scipArtifactReader(compact)
	if err != nil {
		return nil, err
	}
	fresh, err := readSCIPDocuments(ctx, g, project, reader)
	_ = reader.Close()
	if err != nil {
		return nil, err
	}
	groups := map[string][]*scip.Document{}
	for _, d := range fresh {
		groups[d.RelativePath] = append(groups[d.RelativePath], d)
	}
	docs := append([]*scip.Document(nil), c.documents...)
	for i, oldDoc := range docs {
		if _, changed := after[oldDoc.RelativePath]; !changed {
			continue
		}
		list := groups[oldDoc.RelativePath]
		if len(list) == 0 {
			return nil, fmt.Errorf("missing TypeScript compiler document")
		}
		d := list[0]
		groups[oldDoc.RelativePath] = list[1:]
		if !sameSCIPDefinitions(oldDoc, d) {
			return nil, fmt.Errorf("TypeScript symbol identities changed")
		}
		docs[i] = d
	}
	for _, list := range groups {
		if len(list) > 0 {
			return nil, fmt.Errorf("TypeScript configuration coverage changed")
		}
	}
	return &compilerRefresh{documents: docs}, nil
}
func projectRelative(project *pb.Project, path string) string {
	rel, err := filepath.Rel(project.Root, path)
	if err != nil {
		return ""
	}
	return filepath.ToSlash(rel)
}
func sameSCIPDefinitions(a, b *scip.Document) bool {
	keys := func(d *scip.Document) map[string]int {
		out := map[string]int{}
		for _, o := range d.Occurrences {
			if scip.SymbolRole_Definition.Matches(o) && o.Symbol != "" && !strings.HasPrefix(o.Symbol, "local ") {
				out[o.Symbol]++
			}
		}
		return out
	}
	x, y := keys(a), keys(b)
	if len(x) != len(y) {
		return false
	}
	for k, n := range x {
		if y[k] != n {
			return false
		}
	}
	return true
}

func cacheCompilerDocuments(ctx context.Context, documents []*scip.Document) ([]byte, error) {
	raw, err := proto.Marshal(&scip.Index{Metadata: &scip.Metadata{ToolInfo: &scip.ToolInfo{Version: "0.4.0"}}, Documents: documents})
	if err != nil {
		return nil, err
	}
	return cacheRefreshedSCIP(ctx, bytes.NewReader(raw))
}
func (c *typeScriptCompiler) remember(fingerprint string, data []byte, docs []*scip.Document) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fingerprint, c.data, c.documents = fingerprint, data, docs
}

func (c *typeScriptCompiler) invalidate() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stale = true
}
