package indexer

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/mertcikla/codeindex/config"
	"github.com/mertcikla/codeindex/lsp"
	"github.com/mertcikla/codeindex/tools"
)

// WatchSession owns a warm language server for one checkout. It must only be
// attached to working-tree builds, never temporary historical checkouts. Start
// it before the initial SCIP build so package loading happens in parallel.
// Go, TypeScript, Rust and Python have separate sessions and parity gates. One unavailable
// language server must not prevent the other from warming or refreshing.
type WatchSession struct {
	root   string
	cfg    config.Config
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex // serializes batches and Close
	ready  chan struct{}
	client *lsp.Client
	err    error
	stale  bool
	// hashes describes all opened overlays, including unsuccessful batches.
	// Synchronizing against the session rather than only the published base
	// prevents a reverted edit from leaving stale content in the language server.
	hashes map[string]string
	web    *typeScriptSession
	rust   *rustSession
	python *pythonSession
}

func NewWatchSession(ctx context.Context, cfg config.Config, root string) *WatchSession {
	life, cancel := context.WithCancel(ctx)
	abs, err := filepath.Abs(root)
	if err == nil {
		abs, err = filepath.EvalSymlinks(abs)
	}
	s := &WatchSession{root: abs, cfg: cfg, ctx: life, cancel: cancel, ready: make(chan struct{}), hashes: map[string]string{}, err: err}
	s.web = newTypeScriptSession(life, cfg, abs)
	s.rust = newRustSession(life, cfg, abs)
	s.python = newPythonSession(life, cfg, abs)
	go func() {
		defer close(s.ready)
		if s.err != nil {
			return
		}
		projects, err := DiscoverProjects(life, abs, nil, nil)
		if err != nil {
			s.err = err
			return
		}
		hasGo := false
		for _, project := range projects {
			hasGo = hasGo || languageFamily(project.Language) == familyGo
		}
		if !hasGo {
			s.err = fmt.Errorf("no Go project in checkout")
			return
		}
		exe, err := tools.ResolveName(cfg.Tools.Gopls)
		if err != nil {
			s.err = err
			return
		}
		s.client, s.err = lsp.Start(life, exe, abs)
		if s.err != nil {
			return
		}
		// Force workspace loading while SCIP is running. No result is used.
		warm, stop := context.WithTimeout(life, cfg.ToolTimeout())
		defer stop()
		var ignored any
		s.err = s.client.Call(warm, "workspace/symbol", map[string]any{"query": "__codeindex_warmup__"}, &ignored)
	}()
	return s
}

func (s *WatchSession) Close() {
	if s == nil {
		return
	}
	s.cancel()
	s.web.close()
	s.rust.close()
	s.python.close()
	<-s.ready
	s.mu.Lock()

	defer s.mu.Unlock()
	if s.client != nil {
		s.client.Close()
		s.client = nil
	}
}

func (s *WatchSession) lock(ctx context.Context, root string) (*lsp.Client, func(), error) {
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
		s.client, s.err = nil, nil
		s.hashes = map[string]string{}
		exe, err := tools.ResolveName(s.cfg.Tools.Gopls)
		if err == nil {
			s.client, err = lsp.Start(s.ctx, exe, s.root)
		}
		s.err, s.stale = err, false
	}
	if s.err != nil || s.client == nil || root != s.root {
		s.mu.Unlock()
		return nil, nil, fmt.Errorf("Go LSP session unavailable for %s: %v", root, s.err)
	}
	return s.client, s.mu.Unlock, nil
}

// Invalidate drops any editor overlays after a SCIP fallback. In particular,
// module/config edits must reload the server's package graph before it is used
// again. The next batch restarts lazily; historical builds never call this.
func (s *WatchSession) Invalidate() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.stale = true
	s.mu.Unlock()
}
