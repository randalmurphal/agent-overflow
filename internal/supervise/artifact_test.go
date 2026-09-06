package supervise

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type artifactEntry struct {
	name, data string
	mode       os.FileMode
}

func testArtifactZIP(t *testing.T, entries []artifactEntry) (string, string) {
	t.Helper()
	var data bytes.Buffer
	w := zip.NewWriter(&data)
	for _, entry := range entries {
		h := &zip.FileHeader{Name: entry.name, Method: zip.Deflate}
		h.SetMode(entry.mode)
		f, err := w.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(entry.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "download")
	if err := os.WriteFile(file, data.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data.Bytes())
	return file, hex.EncodeToString(digest[:])
}

func testBundleEntries(binary string) []artifactEntry {
	return []artifactEntry{
		{appBundleName + "/Contents/MacOS/agent-overflow", binary, 0o755},
		{appBundleName + "/Contents/Info.plist", "unchanged bundle metadata", 0o644},
		{appBundleName + "/Contents/_CodeSignature/CodeResources", "unchanged signature", 0o644},
		{appBundleName + "/Contents/Resources/theme.json", "resource bytes", 0o644},
	}
}

func TestServiceArtifactPreservesWholeBundleAndPublishesAnImmutableVersion(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("macOS executable modes require a Unix host")
	}
	file, digest := testArtifactZIP(t, testBundleEntries("test executable"))
	a, err := PrepareArtifact(context.Background(), file, "agent-overflow-darwin-arm64.zip", digest)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	layout, _ := NewLayout(t.TempDir())
	if err := a.Stage(layout, "2.0.0"); err != nil {
		t.Fatal(err)
	}
	a.Close()
	version, _ := layout.VersionDir("2.0.0")
	for _, entry := range testBundleEntries("test executable") {
		got, err := os.ReadFile(filepath.Join(version, filepath.FromSlash(entry.name)))
		if err != nil || string(got) != entry.data {
			t.Fatalf("lost %s: %q %v", entry.name, got, err)
		}
	}
	launcher, _ := layout.VersionBinary("2.0.0")
	contents, err := os.ReadFile(launcher)
	if err != nil || !strings.Contains(string(contents), "exec ") {
		t.Fatal("missing legacy supervisor entry point", err)
	}
	// Retrying the same verified release is harmless; a different build cannot
	// overwrite a version that might be a supervisor's rollback target.
	again, err := PrepareArtifact(context.Background(), file, "agent-overflow-darwin-arm64.zip", digest)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if err := again.Stage(layout, "2.0.0"); err != nil {
		t.Fatal(err)
	}
	otherFile, otherDigest := testArtifactZIP(t, testBundleEntries("different executable"))
	other, err := PrepareArtifact(context.Background(), otherFile, "agent-overflow-darwin-arm64.zip", otherDigest)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := other.Stage(layout, "2.0.0"); err == nil {
		t.Fatal("overwrote immutable version")
	}
	got, _ := os.ReadFile(a.Binary)
	if string(got) != "test executable" {
		t.Fatal("changed previous version")
	}
}

func TestServiceArtifactPreservesInternalFrameworkLinks(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("macOS bundle symlinks require a Unix host")
	}
	entries := append(testBundleEntries("test executable"),
		artifactEntry{appBundleName + "/Contents/Frameworks/F.framework/Versions/A/F", "framework", 0o755},
		artifactEntry{appBundleName + "/Contents/Frameworks/F.framework/Versions/Current", "A", os.ModeSymlink | 0o777},
		artifactEntry{appBundleName + "/Contents/Frameworks/F.framework/F", "Versions/Current/F", os.ModeSymlink | 0o777})
	file, digest := testArtifactZIP(t, entries)
	a, err := PrepareArtifact(context.Background(), file, "agent-overflow-darwin-arm64.zip", digest)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	got, err := os.ReadFile(filepath.Join(a.dir, appBundleName, "Contents", "Frameworks", "F.framework", "F"))
	if err != nil || string(got) != "framework" {
		t.Fatal("lost framework link", err)
	}
}

func TestServiceArtifactRejectsUnsafeArchivesAndCleansExtraction(t *testing.T) {
	cases := map[string][]artifactEntry{
		"traversal":     {{appBundleName + "/../outside", "x", 0o644}},
		"absolute":      {{"/outside", "x", 0o644}},
		"separator":     {{appBundleName + "/Contents\\outside", "x", 0o644}},
		"duplicate":     {{appBundleName + "/x", "one", 0o644}, {appBundleName + "/x", "two", 0o644}},
		"special":       {{appBundleName + "/pipe", "", os.ModeNamedPipe | 0o600}},
		"link escape":   {{appBundleName + "/Contents/link", "../../outside", os.ModeSymlink | 0o777}},
		"link absolute": {{appBundleName + "/Contents/link", "/tmp", os.ModeSymlink | 0o777}},
		"link cycle":    {{appBundleName + "/Contents/a", "b", os.ModeSymlink | 0o777}, {appBundleName + "/Contents/b", "a", os.ModeSymlink | 0o777}},
		"no executable": {{appBundleName + "/Contents/MacOS/agent-overflow", "x", 0o600}},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			file, digest := testArtifactZIP(t, entries)
			if a, err := PrepareArtifact(context.Background(), file, "agent-overflow-darwin-arm64.zip", digest); err == nil {
				a.Close()
				t.Fatal("accepted unsafe archive")
			}
			left, _ := filepath.Glob(filepath.Join(filepath.Dir(file), "artifact-*"))
			if len(left) != 0 {
				t.Fatal("retained failed extraction", left)
			}
		})
	}
}

func TestServiceArtifactBoundsExpansionAndHonorsCancellation(t *testing.T) {
	file, digest := testArtifactZIP(t, testBundleEntries("test executable"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := PrepareArtifact(ctx, file, "agent-overflow-darwin-arm64.zip", digest); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(file)
	central := bytes.Index(data, []byte{'P', 'K', 1, 2})
	if central < 0 {
		t.Fatal("fixture missing central directory")
	}
	binary.LittleEndian.PutUint32(data[central+24:], uint32(maxArtifactBytes+1))
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareArtifact(context.Background(), file, "agent-overflow-darwin-arm64.zip", digest); err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatal("ignored declared expansion budget", err)
	}
}
