package app

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sizedPNG encodes an opaque width x height gradient.
func sizedPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x), G: uint8(y), B: 0x40, A: 0xff})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

// getBytes fetches a minted relative URL from the live transport.
func getBytes(t *testing.T, base, relative string) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.Get(base + relative)
	if err != nil {
		t.Fatalf("GET %s: %v", relative, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, body
}

// The whole path: the bound method on the computer that owns the file, the
// single-use relative URL, and the file's own bytes over the byte route.
func TestGetLocalImageServesTheFileOverHTTP(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	payload := sizedPNG(t, 641, 480)
	path := filepath.Join(workspace, "shot.png")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("write image: %v", err)
	}
	app, base := forgeAttachmentApp(t)

	got, err := app.GetLocalImage(path, workspace, 0)
	if err != nil {
		t.Fatalf("GetLocalImage: %v", err)
	}
	if !strings.HasPrefix(got.URL, "/attachments/image/") || !strings.Contains(got.URL, "?ticket=") {
		t.Fatalf("URL = %q, want a relative ticketed /attachments/image/ URL", got.URL)
	}
	want := LocalImage{
		URL: got.URL, MimeType: "image/png", Width: 641, Height: 480,
		OriginalWidth: 641, OriginalHeight: 480, OriginalBytes: int64(len(payload)),
	}
	if got != want {
		t.Fatalf("GetLocalImage = %+v, want %+v", got, want)
	}
	resp, body := getBytes(t, base, got.URL)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/png" || !bytes.Equal(body, payload) {
		t.Fatalf("GET = %d %q with %d bytes, want 200 image/png with the file", resp.StatusCode, resp.Header.Get("Content-Type"), len(body))
	}
	if resp, _ := getBytes(t, base, got.URL); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a spent ticket answered %d, want 404", resp.StatusCode)
	}
}

func TestGetLocalImageServesADerivedTier(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	payload := sizedPNG(t, 641, 480)
	if err := os.WriteFile(filepath.Join(workspace, "shot.png"), payload, 0o600); err != nil {
		t.Fatalf("write image: %v", err)
	}
	app, base := forgeAttachmentApp(t)

	got, err := app.GetLocalImage("shot.png", workspace, 300)
	if err != nil {
		t.Fatalf("GetLocalImage: %v", err)
	}
	if !got.Derived || got.MimeType != "image/png" || got.Width != 320 || got.Height != 240 ||
		got.OriginalWidth != 641 || got.OriginalHeight != 480 || got.OriginalBytes != int64(len(payload)) {
		t.Fatalf("GetLocalImage = %+v, want a 320x240 derivative of the 641x480 file", got)
	}
	resp, body := getBytes(t, base, got.URL)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(body))
	if err != nil || cfg.Width != 320 || cfg.Height != 240 {
		t.Fatalf("served %v %dx%d, want a 320x240 png", err, cfg.Width, cfg.Height)
	}
}

// undecodablePNG is a width x height PNG whose header reads and whose image
// data does not decode: one byte inside it flipped.
func undecodablePNG(t *testing.T, width, height int) []byte {
	t.Helper()
	data := sizedPNG(t, width, height)
	data[len(data)/2] ^= 0xFF
	if _, err := png.DecodeConfig(bytes.NewReader(data)); err != nil {
		t.Fatalf("the header no longer reads: %v", err)
	}
	if _, err := png.Decode(bytes.NewReader(data)); err == nil {
		t.Fatal("the pixels still decode")
	}
	return data
}

// A tier of a file Go cannot decode answers the file itself.
func TestGetLocalImageServesTheFileWhenItsPixelsDoNotDecode(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	payload := undecodablePNG(t, 641, 480)
	if err := os.WriteFile(filepath.Join(workspace, "shot.png"), payload, 0o600); err != nil {
		t.Fatalf("write image: %v", err)
	}
	app, base := forgeAttachmentApp(t)

	got, err := app.GetLocalImage("shot.png", workspace, 300)
	if err != nil {
		t.Fatalf("GetLocalImage: %v", err)
	}
	if got.Derived || got.Width != 641 || got.Height != 480 || got.OriginalBytes != int64(len(payload)) {
		t.Fatalf("GetLocalImage = %+v, want the 641x480 file itself", got)
	}
	if resp, body := getBytes(t, base, got.URL); resp.StatusCode != http.StatusOK || !bytes.Equal(body, payload) {
		t.Fatalf("GET = %d with %d bytes, want the file", resp.StatusCode, len(body))
	}
}

func TestGetLocalImageReportsTheReasonAndNeedsATransport(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	app, _ := forgeAttachmentApp(t)
	_, err := app.GetLocalImage(filepath.Join(workspace, "missing.png"), workspace, 0)
	if err == nil || !strings.HasPrefix(err.Error(), "load local image: file not found: ") {
		t.Fatalf("GetLocalImage(missing) = %v, want the reason the chip shows", err)
	}

	path := filepath.Join(workspace, "shot.png")
	if err := os.WriteFile(path, sizedPNG(t, 8, 8), 0o600); err != nil {
		t.Fatalf("write image: %v", err)
	}
	if _, err := (&App{}).GetLocalImage(path, workspace, 0); err == nil {
		t.Fatal("GetLocalImage answered a URL with no transport to serve it")
	}
}

// The route's 404 for an id nothing resolved comes from the adapter.
func TestOpenLocalImageMissesAnUnknownID(t *testing.T) {
	t.Parallel()
	if _, err := (attachmentTransfer{app: &App{}}).OpenLocalImage("never-resolved"); err == nil {
		t.Fatal("OpenLocalImage answered for an id nothing resolved")
	}
}

// The save lands in the user's Downloads on this computer as the original
// file, never over a file already there.
func TestSaveLocalImageWritesTheOriginalWithoutOverwriting(t *testing.T) {
	workspace := t.TempDir()
	payload := sizedPNG(t, 641, 480)
	path := filepath.Join(workspace, "shot.png")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("write image: %v", err)
	}
	home := t.TempDir()
	downloads := filepath.Join(home, "Downloads")
	if err := os.MkdirAll(downloads, 0o755); err != nil {
		t.Fatalf("create Downloads: %v", err)
	}
	t.Setenv("HOME", home)
	app := &App{configDir: t.TempDir()}

	first, err := app.SaveLocalImage(path, workspace)
	if err != nil {
		t.Fatalf("SaveLocalImage: %v", err)
	}
	if first != filepath.Join(downloads, "shot.png") {
		t.Fatalf("saved to %q, want the user's Downloads directory", first)
	}
	if saved, err := os.ReadFile(first); err != nil || !bytes.Equal(saved, payload) {
		t.Fatalf("saved file = %v (%d bytes), want the original %d bytes", err, len(saved), len(payload))
	}
	second, err := app.SaveLocalImage("shot.png", workspace)
	if err != nil {
		t.Fatalf("second SaveLocalImage: %v", err)
	}
	if second != filepath.Join(downloads, "shot (2).png") {
		t.Fatalf("second save went to %q, want the (2) suffix", second)
	}
	if saved, err := os.ReadFile(first); err != nil || !bytes.Equal(saved, payload) {
		t.Fatal("the second save changed the first file")
	}
}

func TestSaveLocalImageRefusesWhatIsNotAnImage(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	path := filepath.Join(workspace, "notes.png")
	if err := os.WriteFile(path, []byte("not an image"), 0o600); err != nil {
		t.Fatalf("write text: %v", err)
	}
	app := &App{configDir: t.TempDir(), downloadsIsolated: true}
	_, err := app.SaveLocalImage(path, workspace)
	if err == nil || !strings.HasPrefix(err.Error(), "load local image: not an image: ") {
		t.Fatalf("SaveLocalImage(text) = %v, want the not-an-image refusal", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(app.configDir, "downloads")); len(entries) != 0 {
		t.Fatalf("a refused save wrote %d files", len(entries))
	}
}
