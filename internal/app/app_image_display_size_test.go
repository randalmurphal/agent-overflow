package app

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"agent-overflow/internal/testutil"
)

// The images whose size only the header reader knows: an svg with only a
// viewBox (fluid, its ratio's terms) and a real AVIF (its primary item's
// pixels, internal/attachment/testdata).
const displaySizeFluidSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 640 360"><rect width="640" height="360"/></svg>`

func displaySizeAVIF(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("internal/attachment/testdata/solid-5x7.avif")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// displaySizeCases are what each mint answers for those images, at the
// original and at a tier: both are served as themselves.
func displaySizeCases(t *testing.T) []struct {
	name  string
	data  []byte
	mime  string
	w, h  int
	fluid bool
} {
	return []struct {
		name  string
		data  []byte
		mime  string
		w, h  int
		fluid bool
	}{
		{"fluid svg", []byte(displaySizeFluidSVG), "image/svg+xml", 16, 9, true},
		{"avif", displaySizeAVIF(t), "image/avif", 5, 7, false},
	}
}

func TestGetGitBlobImageAnswersFluidAndAVIFSizes(t *testing.T) {
	t.Parallel()
	app, base := reviewDiffFilesApp(t)
	repo := testutil.InitGitRepo(t)
	cases := displaySizeCases(t)
	for _, tc := range cases {
		writeRepoBytes(t, repo, tc.name, tc.data)
	}
	testutil.RunGit(t, repo, "add", "-A")
	testutil.RunGit(t, repo, "commit", "-qm", "images")
	ref := testWorkspaceRef(t, app, repo)
	for _, tc := range cases {
		oid := revParseIn(t, repo, "HEAD:"+tc.name)
		for _, maxWidth := range []int{0, 320} {
			got, err := app.GetGitBlobImage(context.Background(), ref, oid, maxWidth)
			if err != nil {
				t.Fatalf("%s at %d: %v", tc.name, maxWidth, err)
			}
			want := LocalImage{URL: got.URL, MimeType: tc.mime, Width: tc.w, Height: tc.h, OriginalWidth: tc.w, OriginalHeight: tc.h, OriginalBytes: int64(len(tc.data)), Fluid: tc.fluid}
			if got != want {
				t.Fatalf("%s at %d = %+v, want %+v", tc.name, maxWidth, got, want)
			}
			if resp, body := getBytes(t, base, got.URL); resp.StatusCode != http.StatusOK || !bytes.Equal(body, tc.data) {
				t.Fatalf("%s: GET = %d with %d bytes, want the blob", tc.name, resp.StatusCode, len(body))
			}
		}
	}
}

func TestGetLocalImageAnswersFluidAndAVIFSizes(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	app, base := forgeAttachmentApp(t)
	for _, tc := range displaySizeCases(t) {
		path := filepath.Join(workspace, tc.name)
		if err := os.WriteFile(path, tc.data, 0o600); err != nil {
			t.Fatal(err)
		}
		for _, maxWidth := range []int{0, 320} {
			got, err := app.GetLocalImage(path, workspace, maxWidth)
			if err != nil {
				t.Fatalf("%s at %d: %v", tc.name, maxWidth, err)
			}
			want := LocalImage{URL: got.URL, MimeType: tc.mime, Width: tc.w, Height: tc.h, OriginalWidth: tc.w, OriginalHeight: tc.h, OriginalBytes: int64(len(tc.data)), Fluid: tc.fluid}
			if got != want {
				t.Fatalf("%s at %d = %+v, want %+v", tc.name, maxWidth, got, want)
			}
			if resp, body := getBytes(t, base, got.URL); resp.StatusCode != http.StatusOK || !bytes.Equal(body, tc.data) {
				t.Fatalf("%s: GET = %d with %d bytes, want the file", tc.name, resp.StatusCode, len(body))
			}
		}
	}
}

func TestFetchForgeAttachmentAnswersFluidAndAVIFSizes(t *testing.T) {
	t.Parallel()
	for _, tc := range displaySizeCases(t) {
		core, _ := gitlabUploads(t, constantPayload(tc.data))
		app, base := forgeAttachmentApp(t)
		app.git = core
		for _, maxWidth := range []int{0, 320} {
			got, err := app.FetchForgeAttachment(t.Context(), testGitLabPR, testUploadHref(), maxWidth)
			if err != nil {
				t.Fatalf("%s at %d: %v", tc.name, maxWidth, err)
			}
			want := ForgeAttachment{URL: got.URL, MimeType: tc.mime, Kind: "image", SizeBytes: int64(len(tc.data)), Filename: "hero.png", Width: tc.w, Height: tc.h, OriginalWidth: tc.w, OriginalHeight: tc.h, Fluid: tc.fluid}
			if got != want {
				t.Fatalf("%s at %d = %+v, want %+v", tc.name, maxWidth, got, want)
			}
			if resp, body := getBytes(t, base, got.URL); resp.StatusCode != http.StatusOK || !bytes.Equal(body, tc.data) {
				t.Fatalf("%s: GET = %d with %d bytes, want the attachment", tc.name, resp.StatusCode, len(body))
			}
		}
	}
}
