package externalurl

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The frontend renders links from its copy of the deny-list; the opener
// refuses from this one. A scheme present in one and absent from the other
// is a link that renders but will not open, or opens but never renders.
func TestDeniedSchemesMatchFrontend(t *testing.T) {
	path := filepath.Join("..", "..", "frontend", "src", "lib", "markdown", "render", "elements", "urlSchemes.ts")
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read frontend scheme list: %v", err)
	}
	start := strings.Index(string(source), "DENIED_LINK_SCHEMES")
	end := strings.Index(string(source)[start:], "]);")
	if start < 0 || end < 0 {
		t.Fatalf("DENIED_LINK_SCHEMES literal not found in %s", path)
	}
	literal := string(source)[start : start+end]
	var frontend []string
	for _, match := range regexp.MustCompile(`'([a-z][a-z0-9+.-]*)'`).FindAllStringSubmatch(literal, -1) {
		frontend = append(frontend, match[1])
	}
	var backend []string
	for scheme := range deniedSchemes {
		backend = append(backend, scheme)
	}
	sort.Strings(frontend)
	sort.Strings(backend)
	if strings.Join(frontend, ",") != strings.Join(backend, ",") {
		t.Fatalf("deny-lists differ\nfrontend: %v\nbackend:  %v", frontend, backend)
	}
}

func TestSchemeOpenable(t *testing.T) {
	for _, scheme := range []string{"http", "https", "mailto", "tel", "vscode", "obsidian", "slack", "ssh", "MAILTO"} {
		if !SchemeOpenable(scheme) {
			t.Errorf("SchemeOpenable(%q) = false, want true", scheme)
		}
	}
	for _, scheme := range []string{"", "c", "C", "file", "FILE", "javascript", "data", "ms-msdt", "JavaScript"} {
		if SchemeOpenable(scheme) {
			t.Errorf("SchemeOpenable(%q) = true, want false", scheme)
		}
	}
}
