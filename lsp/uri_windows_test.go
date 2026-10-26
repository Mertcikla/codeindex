package lsp

import "testing"

func TestWindowsFileURI(t *testing.T) {
	for _, tt := range []struct{ path, uri string }{
		{`C:\Program Files\π #%.go`, "file:///C:/Program%20Files/%CF%80%20%23%25.go"},
		{`\\server\share\space #.go`, "file://server/share/space%20%23.go"},
	} {
		if got := URI(tt.path); got != tt.uri {
			t.Errorf("URI(%q) = %q, want %q", tt.path, got, tt.uri)
		}
		if got, err := Path(tt.uri); err != nil || got != tt.path {
			t.Errorf("Path(%q) = %q, %v; want %q", tt.uri, got, err, tt.path)
		}
	}
}
