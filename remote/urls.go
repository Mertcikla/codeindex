package remote

import (
	"context"
	"net/url"
	"os/exec"
	"strings"
)

// NormalizeRemote converts common Git remote forms (https, http, ssh, git and
// scp-like git@host:path) into a canonical browser URL such as
// https://github.com/owner/repo. Credentials are stripped. ok is false when
// raw does not identify a remote repository.
//
// This is the canonical implementation shared by every consumer of remote
// identity (repository identity resolution, remote keys stored by callers, and
// remote reference parsing); callers must delegate here rather than re-derive
// the normalization so keys stay comparable.
func NormalizeRemote(raw string) (string, bool) {
	cleaned := strings.TrimSpace(raw)
	if cleaned == "" || strings.HasPrefix(cleaned, "/") || strings.HasPrefix(cleaned, "~") {
		return "", false
	}
	if looksLikeWindowsPath(cleaned) {
		return "", false
	}
	cleaned = strings.TrimSuffix(cleaned, "/")
	cleaned = strings.TrimSuffix(cleaned, ".git")

	var host, path string
	switch {
	case strings.Contains(cleaned, "://"):
		parsed, err := url.Parse(cleaned)
		if err != nil || parsed.Hostname() == "" {
			return "", false
		}
		switch strings.ToLower(parsed.Scheme) {
		case "http", "https", "ssh", "git":
		default:
			return "", false
		}
		host = parsed.Hostname()
		path = parsed.Path
	case strings.Contains(cleaned, "@") && strings.Contains(cleaned, ":"):
		at := strings.Index(cleaned, "@")
		colon := strings.Index(cleaned[at:], ":") + at
		host = cleaned[at+1 : colon]
		path = cleaned[colon+1:]
	case strings.Contains(cleaned, ":"):
		colon := strings.Index(cleaned, ":")
		host = cleaned[:colon]
		path = cleaned[colon+1:]
	default:
		slash := strings.Index(cleaned, "/")
		if slash < 0 {
			return "", false
		}
		host = cleaned[:slash]
		path = cleaned[slash+1:]
	}
	host = strings.ToLower(strings.TrimSpace(host))
	path = strings.Trim(strings.TrimSpace(path), "/")
	if host == "" || path == "" || (!strings.Contains(host, ".") && host != "localhost") {
		return "", false
	}
	if strings.ContainsAny(host, " \t") || strings.ContainsAny(path, " \t") {
		return "", false
	}
	return "https://" + host + "/" + path, true
}

// RemoteKey returns a normalized host/path identity for a remote URL or a
// legacy element repo value, including GitHub owner/repo shorthand. It returns
// "" when value cannot be interpreted as a repository identity. Matching is
// case-insensitive.
func RemoteKey(value string) string {
	cleaned := strings.TrimSpace(value)
	if cleaned == "" || strings.HasPrefix(cleaned, "/") || strings.HasPrefix(cleaned, "~") || looksLikeWindowsPath(cleaned) {
		return ""
	}
	if normalized, ok := NormalizeRemote(cleaned); ok {
		return strings.ToLower(strings.TrimPrefix(normalized, "https://"))
	}
	parts := strings.Split(strings.Trim(cleaned, "/"), "/")
	if len(parts) == 2 && parts[0] != "" && parts[1] != "" &&
		!strings.Contains(parts[0], ".") && !strings.Contains(parts[0], ":") {
		return "github.com/" + strings.ToLower(parts[0]) + "/" + strings.ToLower(parts[1])
	}
	return ""
}

// GitRemoteURL reports the sanitized browser URL of the repository's origin
// remote, or "" when there is no usable origin.
func GitRemoteURL(ctx context.Context, root string) string {
	if strings.TrimSpace(root) == "" {
		return ""
	}
	out, err := exec.CommandContext(ctx, "git", "-C", root, "remote", "get-url", "origin").Output()
	if err != nil {
		return ""
	}
	normalized, ok := NormalizeRemote(strings.TrimSpace(string(out)))
	if !ok {
		return ""
	}
	return normalized
}
