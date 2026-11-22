package server

import "testing"

func TestSanitizeFilename(t *testing.T) {
	tests := map[string]string{
		"report.pdf":             "report.pdf",
		`C:\Users\me\report.pdf`: "report.pdf",
		"../../etc/passwd":       "passwd",
		"we\"ird\nname.pdf":      "weirdname.pdf",
		"":                       "document.pdf",
		"..":                     "..",
	}
	for in, want := range tests {
		if got := sanitizeFilename(in); got != want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}
