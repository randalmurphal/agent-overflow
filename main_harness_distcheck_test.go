package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func writeDistFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for path, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func embedFixture(files map[string]string) fstest.MapFS {
	m := fstest.MapFS{}
	for path, content := range files {
		m["frontend/dist/"+path] = &fstest.MapFile{Data: []byte(content)}
	}
	return m
}

// TestCompareEmbeddedDist: the tripwire must flag content drift, an
// added file, and a renamed file — a stale embed after a `vite build`
// once cost a full profiling round (minified names from an old bundle).
func TestCompareEmbeddedDist(t *testing.T) {
	base := map[string]string{
		"index.html":        "<html>app</html>",
		"assets/app-a1.js":  "console.log(1)",
		"assets/app-a1.css": "body{}",
	}

	t.Run("identical trees match", func(t *testing.T) {
		verdict, warning := compareEmbeddedDist(embedFixture(base), writeDistFixture(t, base))
		if verdict != assetsFreshnessMatch || warning != "" {
			t.Fatalf("verdict = %q warning = %q, want match/empty", verdict, warning)
		}
	})

	stale := func(t *testing.T, disk map[string]string) {
		t.Helper()
		verdict, warning := compareEmbeddedDist(embedFixture(base), writeDistFixture(t, disk))
		if verdict != assetsFreshnessStale {
			t.Fatalf("verdict = %q, want stale", verdict)
		}
		if !strings.Contains(warning, "STALE") || !strings.Contains(warning, "make harness-build") {
			t.Fatalf("warning does not name the problem and the fix: %q", warning)
		}
	}
	t.Run("changed content is stale", func(t *testing.T) {
		disk := map[string]string{}
		for k, v := range base {
			disk[k] = v
		}
		disk["assets/app-a1.js"] = "console.log(2)"
		stale(t, disk)
	})
	t.Run("added file is stale", func(t *testing.T) {
		disk := map[string]string{}
		for k, v := range base {
			disk[k] = v
		}
		disk["assets/chunk-b2.js"] = "new chunk"
		stale(t, disk)
	})
	t.Run("renamed file is stale", func(t *testing.T) {
		disk := map[string]string{
			"index.html":        base["index.html"],
			"assets/app-b2.js":  base["assets/app-a1.js"],
			"assets/app-a1.css": base["assets/app-a1.css"],
		}
		stale(t, disk)
	})
}

// TestCheckEmbeddedDistFreshnessDevServer: when the instance serves
// dev-server assets the embed is not what anyone measures, so the
// staleness verdict must step aside for the existing dev-asset warning.
func TestCheckEmbeddedDistFreshnessDevServer(t *testing.T) {
	t.Setenv("FRONTEND_DEVSERVER_URL", "http://localhost:5173")
	verdict, warning := checkEmbeddedDistFreshness(embedFixture(map[string]string{"index.html": "x"}))
	if verdict != assetsFreshnessDev || warning != "" {
		t.Fatalf("verdict = %q warning = %q, want dev-server/empty", verdict, warning)
	}
}
