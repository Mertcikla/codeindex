package jsonstore

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// AllocateIDs writes the sequence atomically while Open holds a process lock.
func (s *Store) AllocateIDs(_ context.Context, count int) (int64, error) {
	if count <= 0 {
		return 0, fmt.Errorf("ID allocation count must be positive")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.dir, "sequence")
	var last int64
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return 0, err
	}
	if err == nil {
		last, err = strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
		if err != nil {
			return 0, err
		}
	}
	if last < 0 || last > math.MaxInt64-int64(count) {
		return 0, fmt.Errorf("codeindex IDs exhausted")
	}
	tmp, err := os.CreateTemp(s.dir, "sequence-*")
	if err != nil {
		return 0, err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.WriteString(strconv.FormatInt(last+int64(count), 10)); err != nil {
		_ = tmp.Close()
		return 0, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return 0, err
	}
	return last + 1, nil
}

type repositoryDoc struct {
	ID        string `json:"id"`
	Root      string `json:"root"`
	RemoteURL string `json:"remote_url,omitempty"`
	RemoteKey string `json:"remote_key,omitempty"`
	Managed   bool   `json:"managed,omitempty"`
}

func (s *Store) RepositoryByRoot(_ context.Context, root string) (string, bool, error) {
	return s.findRepository(func(r repositoryDoc) bool { return r.Root == root })
}
func (s *Store) findRepository(matches func(repositoryDoc) bool) (string, bool, error) {
	paths, err := filepath.Glob(filepath.Join(s.dir, "*", "repository.json"))
	if err != nil {
		return "", false, err
	}
	for _, path := range paths {
		var repo repositoryDoc
		if err := readJSON(path, &repo); err != nil {
			return "", false, err
		}
		if matches(repo) {
			return repo.ID, true, nil
		}
	}
	return "", false, nil
}
