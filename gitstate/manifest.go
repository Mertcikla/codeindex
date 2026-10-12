package gitstate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	pb "buf.build/gen/go/tldiagramcom/diagram/protocolbuffers/go/codeindex/v1"
)

// FileStamp identifies a file's content without hashing it: size, modification
// time, and mode. A content change that preserves all three is treated as
// unchanged, matching how git decides whether the index needs a rehash.
type FileStamp struct {
	Size       int64
	MtimeNanos int64
	Mode       string
	Hash       string
}

// ManifestCache carries content hashes from a previous manifest pass so a
// post-index verification pass only re-reads files whose stamp moved.
type ManifestCache map[string]FileStamp

// Manifest captures every tracked or nonignored untracked file, independent of
// language, size, binary detection, and extraction exclusions. Symlinks are
// hashed as links, never followed. A missing Git checkout is explicit.
func Manifest(ctx context.Context, root string) (*pb.SnapshotManifest, error) {
	manifest, _, err := ManifestCached(ctx, root, nil)
	return manifest, err
}

// ManifestCached is Manifest with hash reuse from an earlier pass. It returns
// the cache to pass to the next call; only regular files participate.
func ManifestCached(ctx context.Context, root string, cache ManifestCache) (*pb.SnapshotManifest, ManifestCache, error) {
	if _, err := Run(ctx, root, "rev-parse", "--git-dir"); err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return &pb.SnapshotManifest{Reason: "not a Git checkout"}, nil, nil
	}
	raw, err := Run(ctx, root, "ls-files", "--cached", "--others", "--exclude-standard", "-z", "--", ".")
	if err != nil {
		return nil, nil, err
	}
	// Gitlinks may be uninitialized; in that case their immutable index
	// identity is authoritative, never the parent checkout's HEAD.
	staged, err := Run(ctx, root, "ls-files", "--stage", "-z")
	if err != nil {
		return nil, nil, err
	}
	gitlinks := map[string]string{}
	for _, record := range strings.Split(staged, "\x00") {
		metadata, path, ok := strings.Cut(record, "\t")
		fields := strings.Fields(metadata)
		if ok && len(fields) == 3 && fields[0] == "160000" {
			gitlinks[path] = fields[1]
		}
	}
	manifest := &pb.SnapshotManifest{Complete: true}
	next := make(ManifestCache, len(cache))
	seen := map[string]bool{}
	for _, path := range strings.Split(raw, "\x00") {
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		absolute := filepath.Join(root, filepath.FromSlash(path))
		info, err := os.Lstat(absolute)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		entry := &pb.SourceFile{Path: path, Mode: "100644"}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			entry.Mode = "120000"
			target, err := os.Readlink(absolute)
			if err != nil {
				return nil, nil, err
			}
			hash := sha256.Sum256([]byte(target))
			entry.Hash = hex.EncodeToString(hash[:])
		case info.Mode().IsRegular():
			if info.Mode()&0111 != 0 {
				entry.Mode = "100755"
			}
			stamp := info.ModTime().UnixNano()
			if cached, ok := cache[path]; ok && cached.Hash != "" && cached.Size == info.Size() && cached.MtimeNanos == stamp && cached.Mode == entry.Mode {
				entry.Hash = cached.Hash
			} else {
				file, err := os.Open(absolute)
				if err != nil {
					return nil, nil, err
				}
				hash := sha256.New()
				_, readErr := io.Copy(hash, file)
				closeErr := file.Close()
				if err := errors.Join(readErr, closeErr); err != nil {
					return nil, nil, err
				}
				entry.Hash = hex.EncodeToString(hash.Sum(nil))
			}
			next[path] = FileStamp{Size: info.Size(), MtimeNanos: stamp, Mode: entry.Mode, Hash: entry.Hash}
		case info.IsDir():
			entry.Mode = "160000"
			revision := gitlinks[path]
			top, topErr := Run(ctx, absolute, "rev-parse", "--show-toplevel")
			if topErr == nil && filepath.Clean(strings.TrimSpace(top)) == filepath.Clean(absolute) {
				revision, err = Resolve(ctx, absolute, "HEAD")
				if err != nil {
					return nil, nil, fmt.Errorf("capture submodule %s: %w", path, err)
				}
			}
			if revision == "" {
				return nil, nil, fmt.Errorf("missing Git identity for directory %s", path)
			}
			entry.Hash = revision
		default:
			return nil, nil, fmt.Errorf("unsupported Git file type: %s", path)
		}
		manifest.Files = append(manifest.Files, entry)
	}
	sort.Slice(manifest.Files, func(i, j int) bool { return manifest.Files[i].Path < manifest.Files[j].Path })
	return manifest, next, nil
}
