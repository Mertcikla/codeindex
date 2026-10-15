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

type typeScriptSession struct {
	ctx    context.Context
	root   string
	cfg    config.Config
	ready  chan struct{}
	mu     sync.Mutex
	client *lsp.Client
	err    error
	stale  bool
	hashes map[string]string
	// External identities are keyed by authoritative definition location and
	// constructor context. They are learned from the baseline, not guessed
	// from display names, which collide across modules and overloads.
	external map[string]map[string]bool
	seeded   map[string]bool
}

func newTypeScriptSession(ctx context.Context, cfg config.Config, root string) *typeScriptSession {
	s := &typeScriptSession{ctx: ctx, root: root, cfg: cfg, ready: make(chan struct{}), hashes: map[string]string{}, external: map[string]map[string]bool{}, seeded: map[string]bool{}}
	go func() {
		defer close(s.ready)
		projects, err := DiscoverProjects(ctx, root, nil, nil)
		if err != nil {
			s.err = err
			return
		}
		for _, pr := range projects {
			if languageFamily(pr.Language) == familyWeb {
				s.client, s.err = s.start()
				if s.err == nil {
					s.warm(filepath.Join(root, filepath.FromSlash(pr.Root)))
				}
				return
			}
		}
		s.err = fmt.Errorf("no TypeScript project in checkout")
	}()
	return s
}

func (s *typeScriptSession) warm(projectDir string) {
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
		if !isTestSource(path) && (sourceLanguage(path) == "typescript" || sourceLanguage(path) == "tsx") {
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
	var ignored any
	// Loading a configured project happens while the initial SCIP run proceeds.
	// Failure only disables warm-up; a refresh still verifies its own project.
	_ = s.client.TypeScriptRequest(ctx, "projectInfo", map[string]any{"file": first, "needFileNameList": false}, &ignored)
}

// Resolve the same SDK that Node resolves from scip-typescript's entry point.
// A wrapper with no accessible SDK is unsupported; using a different compiler
// would give different stdlib declaration sets and could silently lose edges.
func typeScriptSDK(cfg config.Config) (string, error) {
	exe, err := tools.ResolveName(cfg.Tools.SCIPTypeScript)
	if err != nil {
		return "", err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return "", err
	}
	for dir := filepath.Dir(exe); ; dir = filepath.Dir(dir) {
		candidate := filepath.Join(dir, "node_modules", "typescript", "lib", "tsserver.js")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	return "", fmt.Errorf("cannot resolve scip-typescript's TypeScript SDK from %s", exe)
}

func (s *typeScriptSession) start() (*lsp.Client, error) {
	exe, err := tools.ResolveName(s.cfg.Tools.TypeScriptLanguageServer)
	if err != nil {
		return nil, err
	}
	sdk, err := typeScriptSDK(s.cfg)
	if err != nil {
		return nil, err
	}
	return lsp.StartTypeScript(s.ctx, exe, s.root, sdk)
}

func (s *typeScriptSession) lock(ctx context.Context, root string) (*lsp.Client, func(), error) {
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
		return nil, nil, fmt.Errorf("TypeScript LSP session unavailable: %v", s.err)
	}
	return s.client, s.mu.Unlock, nil
}

func (s *typeScriptSession) invalidate() { s.mu.Lock(); s.stale = true; s.mu.Unlock() }
func (s *typeScriptSession) close() {
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
