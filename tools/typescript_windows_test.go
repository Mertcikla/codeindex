package tools

import (
	"testing"

	"github.com/mertcikla/codeindex/lsp"
)

func TestMatchesWindowsTypeScriptHealthDefinition(t *testing.T) {
	for _, tt := range []struct{ path, uri string }{
		{`C:\Program Files\π # %\__codeindex_health__.ts`, "file:///c%3A/Program%20Files/%CF%80%20%23%20%25/__codeindex_health__.ts"},
		{`\\server\share\space #\__codeindex_health__.ts`, "file://server/share/space%20%23/__codeindex_health__.ts"},
	} {
		loc := lsp.Location{URI: tt.uri, Range: lsp.Range{Start: lsp.Position{Character: 9}}}
		if !matchesTypeScriptHealthDefinition(tt.path, loc) {
			t.Errorf("did not match Windows definition %q for %q", tt.uri, tt.path)
		}
	}
}
