package indexer

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/mertcikla/codeindex/config"
	"github.com/mertcikla/codeindex/graph"
	"github.com/mertcikla/codeindex/lsp"
	"github.com/mertcikla/codeindex/parser"
	"github.com/mertcikla/codeindex/tools"
)

type pythonSession struct {
	ctx        context.Context
	root       string
	projectDir string
	cfg        config.Config
	ready      chan struct{}
	mu         sync.Mutex
	client     *lsp.Client
	err        error
	stale      bool
	hashes     map[string]string
	// External identities are learned from authoritative baseline definition
	// locations rather than display names, which collide across Python modules.
	external map[string]map[string]bool
	seeded   map[string]bool
	ignored  map[string]bool
	builtins map[string]map[string]bool
}

func newPythonSession(ctx context.Context, cfg config.Config, root string) *pythonSession {
	s := &pythonSession{ctx: ctx, root: root, cfg: cfg, ready: make(chan struct{}), hashes: map[string]string{}, external: map[string]map[string]bool{}, seeded: map[string]bool{}, ignored: map[string]bool{}, builtins: map[string]map[string]bool{}}
	go func() {
		defer close(s.ready)
		projects, err := DiscoverProjects(ctx, root, nil, nil)
		if err != nil {
			s.err = err
			return
		}
		bestDepth := 0
		for _, pr := range projects {
			if languageFamily(pr.Language) != familyPython {
				continue
			}
			dir := filepath.Join(root, filepath.FromSlash(pr.Root))
			rel, _ := filepath.Rel(root, dir)
			depth := 0
			if rel != "." {
				depth = strings.Count(filepath.ToSlash(rel), "/") + 1
			}
			if s.projectDir == "" || depth < bestDepth {
				s.projectDir = dir
				bestDepth = depth
			}
		}
		if s.projectDir != "" {
			s.client, s.err = s.start()
			if s.err == nil {
				s.warm(s.projectDir)
			}
			return
		}
		s.err = fmt.Errorf("no Python project in checkout")
	}()
	return s
}

func (s *pythonSession) warm(projectDir string) {
	var first string
	_ = filepath.WalkDir(projectDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || parser.SkipDirs[entry.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !isTestSource(path) && sourceLanguage(path) == "python" {
			first = path
			return filepath.SkipAll
		}
		return nil
	})
	if first == "" {
		return
	}
	text, err := os.ReadFile(first)
	if err != nil {
		return
	}
	if err := s.client.Sync(first, text); err != nil {
		return
	}
	rel, err := filepath.Rel(s.root, first)
	if err != nil {
		return
	}
	s.hashes[filepath.ToSlash(rel)] = graph.Hash(text)
	ctx, stop := context.WithTimeout(s.ctx, s.cfg.ToolTimeout())
	defer stop()
	_ = s.client.CheckPythonDocument(ctx, first)
}

func (s *pythonSession) start() (*lsp.Client, error) {
	exe, err := tools.ResolveName(s.cfg.Tools.Pyright)
	if err != nil {
		return nil, err
	}
	return lsp.StartPyright(s.ctx, exe, s.projectDir)
}

func (s *pythonSession) lock(ctx context.Context, root string) (*lsp.Client, func(), error) {
	select {
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	case <-s.ready:
	}
	s.mu.Lock()
	if s.stale && root == s.root {
		if s.client != nil {
			s.client.Close()
		}
		s.hashes = map[string]string{}
		s.external = map[string]map[string]bool{}
		s.seeded = map[string]bool{}
		s.ignored = map[string]bool{}
		s.builtins = map[string]map[string]bool{}
		s.client, s.err = s.start()
		s.stale = false
	}
	if s.err != nil || s.client == nil || root != s.root {
		s.mu.Unlock()
		return nil, nil, fmt.Errorf("Python LSP session unavailable: %v", s.err)
	}
	return s.client, s.mu.Unlock, nil
}

func (s *pythonSession) invalidate() { s.mu.Lock(); s.stale = true; s.mu.Unlock() }
func (s *pythonSession) close() {
	if s == nil {
		return
	}
	<-s.ready
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client != nil {
		s.client.Close()
		s.client = nil
	}
}
