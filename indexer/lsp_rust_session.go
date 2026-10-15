package indexer

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/mertcikla/codeindex/config"
	"github.com/mertcikla/codeindex/graph"
	"github.com/mertcikla/codeindex/lsp"
	"github.com/mertcikla/codeindex/parser"
	"github.com/mertcikla/codeindex/tools"
)

type rustSession struct {
	ctx    context.Context
	root   string
	cfg    config.Config
	ready  chan struct{}
	mu     sync.Mutex
	client *lsp.Client
	err    error
	stale  bool
	hashes map[string]string
	// External identities are learned from authoritative baseline definition
	// locations rather than display names, which collide across crates and impls.
	external map[string]map[string]bool
	seeded   map[string]bool
}

func newRustSession(ctx context.Context, cfg config.Config, root string) *rustSession {
	s := &rustSession{ctx: ctx, root: root, cfg: cfg, ready: make(chan struct{}), hashes: map[string]string{}, external: map[string]map[string]bool{}, seeded: map[string]bool{}}
	go func() {
		defer close(s.ready)
		projects, err := DiscoverProjects(ctx, root, nil, nil)
		if err != nil {
			s.err = err
			return
		}
		for _, pr := range projects {
			if languageFamily(pr.Language) == familyRust {
				s.client, s.err = s.start()
				if s.err == nil {
					s.warm(filepath.Join(root, filepath.FromSlash(pr.Root)))
				}
				return
			}
		}
		s.err = fmt.Errorf("no Rust project in checkout")
	}()
	return s
}

func (s *rustSession) warm(projectDir string) {
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
		if !isTestSource(path) && sourceLanguage(path) == "rust" {
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
	_ = s.client.WaitRustReady(ctx)
}

func (s *rustSession) start() (*lsp.Client, error) {
	exe, err := tools.ResolveName(s.cfg.Tools.RustAnalyzer)
	if err != nil {
		return nil, err
	}
	return lsp.StartRust(s.ctx, exe, s.root)
}

func (s *rustSession) lock(ctx context.Context, root string) (*lsp.Client, func(), error) {
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
		s.client, s.err = s.start()
		s.stale = false
	}
	if s.err != nil || s.client == nil || root != s.root {
		s.mu.Unlock()
		return nil, nil, fmt.Errorf("Rust LSP session unavailable: %v", s.err)
	}
	return s.client, s.mu.Unlock, nil
}

func (s *rustSession) invalidate() { s.mu.Lock(); s.stale = true; s.mu.Unlock() }
func (s *rustSession) close() {
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
