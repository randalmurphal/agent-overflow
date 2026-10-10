package localimage

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/attachment"
)

func pngBytes(t *testing.T, width, height int) []byte {
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

func writeFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func readAll(t *testing.T, s *Service, contentID string) (Content, []byte) {
	t.Helper()
	content, err := s.Open(contentID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer content.Content.Close()
	data, err := io.ReadAll(content.Content)
	if err != nil {
		t.Fatalf("read content: %v", err)
	}
	return content, data
}

func TestResolveServesTheFileItself(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	payload := pngBytes(t, 8, 6)
	path := writeFile(t, workspace, "diagram.png", payload)
	s := New()

	got, err := s.Resolve(path, workspace, 0)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := Image{
		ContentID: got.ContentID, MimeType: "image/png", Width: 8, Height: 6,
		OriginalWidth: 8, OriginalHeight: 6, OriginalBytes: int64(len(payload)),
	}
	if got != want || got.ContentID == "" {
		t.Fatalf("Resolve = %+v, want %+v", got, want)
	}
	content, data := readAll(t, s, got.ContentID)
	if !bytes.Equal(data, payload) || content.MimeType != "image/png" {
		t.Fatalf("Open served %d bytes as %q, want the file as image/png", len(data), content.MimeType)
	}
	// The file itself, so the route answers Range and nothing is held in
	// memory for it.
	if _, isFile := content.Content.(*os.File); !isFile {
		t.Fatalf("an original is served from %T, want the open file", content.Content)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !content.ModTime.Equal(info.ModTime()) {
		t.Fatalf("ModTime = %v, want the file's %v", content.ModTime, info.ModTime())
	}
}

func TestResolveServesATierDerivative(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	// Over one and a half times the 480 tier, so both tiers derive.
	payload := pngBytes(t, 721, 540)
	path := writeFile(t, workspace, "shot.png", payload)
	s := New()

	got, err := s.Resolve(path, workspace, 300)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !got.Derived || got.Width != 320 || got.Height != 240 || got.MimeType != "image/png" ||
		got.OriginalWidth != 721 || got.OriginalHeight != 540 || got.OriginalBytes != int64(len(payload)) {
		t.Fatalf("Resolve = %+v, want a 320x240 png derivative of the 721x540 file", got)
	}
	content, data := readAll(t, s, got.ContentID)
	cfg, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width != 320 || cfg.Height != 240 {
		t.Fatalf("served %v %dx%d, want a 320x240 png", err, cfg.Width, cfg.Height)
	}
	if content.ModTime.IsZero() {
		t.Fatal("a derivative carries no ModTime")
	}

	// Another tier is another derivative; the original is a third entry.
	wider, err := s.Resolve(path, workspace, 400)
	if err != nil {
		t.Fatalf("Resolve 400: %v", err)
	}
	original, err := s.Resolve(path, workspace, 0)
	if err != nil {
		t.Fatalf("Resolve 0: %v", err)
	}
	if wider.Width != 480 || wider.ContentID == got.ContentID || original.Derived || original.ContentID == got.ContentID {
		t.Fatalf("tiers share entries: 320 %q, 480 %+v, original %+v", got.ContentID, wider, original)
	}
}

// A file over the derivative pixel cap whose asked tier's derivative is over
// it too is served at the next tier down, under the cache key of the tier
// asked for: asking again answers the same entry without deriving again.
func TestResolveStepsDownOverThePixelCap(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	// 16.9 MP; 480x35027 is over the cap, 320x23351 is not.
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, 481, 35100))); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	path := writeFile(t, workspace, "tall.png", buf.Bytes())
	s := New()

	got, err := s.Resolve(path, workspace, 400)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !got.Derived || got.Width != 320 || got.Height != 23351 || got.OriginalWidth != 481 || got.OriginalHeight != 35100 {
		t.Fatalf("Resolve = %+v, want a 320x23351 derivative of the 481x35100 file", got)
	}
	_, data := readAll(t, s, got.ContentID)
	if cfg, err := png.DecodeConfig(bytes.NewReader(data)); err != nil || cfg.Width != 320 || cfg.Height != 23351 {
		t.Fatalf("served %v %dx%d, want a 320x23351 png", err, cfg.Width, cfg.Height)
	}
	// The same tier answers the held entry. The bytes behind the same size
	// and mtime are no longer an image, so a second derivation would fail.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), buf.Len()), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatalf("restore mtime: %v", err)
	}
	for _, maxWidth := range []int{400, 480} {
		again, err := s.Resolve(path, workspace, maxWidth)
		if err != nil || again != got {
			t.Fatalf("Resolve at %d = %+v, %v; want the held %+v", maxWidth, again, err, got)
		}
	}
}

// corruptPixels flips a byte inside a PNG's image data. The header still
// reads; the decode fails.
func corruptPixels(t *testing.T, src []byte) []byte {
	t.Helper()
	out := bytes.Clone(src)
	out[len(out)/2] ^= 0xFF
	if _, err := png.DecodeConfig(bytes.NewReader(out)); err != nil {
		t.Fatalf("the corrupted header no longer reads: %v", err)
	}
	if _, err := png.Decode(bytes.NewReader(out)); err == nil {
		t.Fatal("the corrupted pixels still decode")
	}
	return out
}

// A file Go cannot decode behind a valid header is served as it is when a
// tier is asked for, as it was before derivatives existed.
func TestResolveServesTheFileWhenItsPixelsDoNotDecode(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	payload := corruptPixels(t, pngBytes(t, 641, 480))
	path := writeFile(t, workspace, "shot.png", payload)
	s := New()

	got, err := s.Resolve(path, workspace, 300)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Derived || got.MimeType != "image/png" || got.Width != 641 || got.Height != 480 || got.OriginalBytes != int64(len(payload)) {
		t.Fatalf("Resolve = %+v, want the 641x480 file itself", got)
	}
	if _, data := readAll(t, s, got.ContentID); !bytes.Equal(data, payload) {
		t.Fatal("the route would not serve the file's bytes")
	}
}

// A held derivative answers without reading the file again: identity is
// path, size and mtime, so same-size bytes written under a restored mtime are
// still the version that was derived.
func TestResolveAnswersAHeldDerivativeWithoutReading(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	payload := pngBytes(t, 641, 480)
	path := writeFile(t, workspace, "shot.png", payload)
	s := New()
	first, err := s.Resolve(path, workspace, 320)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), len(payload)), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatalf("restore mtime: %v", err)
	}
	second, err := s.Resolve(path, workspace, 320)
	if err != nil {
		t.Fatalf("second Resolve read the file again: %v", err)
	}
	if second != first {
		t.Fatalf("second Resolve = %+v, want the held %+v", second, first)
	}

	// A new size or a new mtime is a new version, read and validated again.
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), len(payload)+1), 0o600); err != nil {
		t.Fatalf("resize: %v", err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatalf("restore mtime: %v", err)
	}
	if _, err := s.Resolve(path, workspace, 320); err == nil || !strings.HasPrefix(err.Error(), "load local image: not an image: ") {
		t.Fatalf("Resolve after a size change = %v, want the new bytes refused", err)
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), len(payload)), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if err := os.Chtimes(path, info.ModTime().Add(time.Second), info.ModTime().Add(time.Second)); err != nil {
		t.Fatalf("touch: %v", err)
	}
	if _, err := s.Resolve(path, workspace, 320); err == nil || !strings.HasPrefix(err.Error(), "load local image: not an image: ") {
		t.Fatalf("Resolve after an mtime change = %v, want the new bytes refused", err)
	}
}

func TestOpenRefusesAnOriginalThatChanged(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	path := writeFile(t, workspace, "shot.png", pngBytes(t, 8, 8))
	s := New()
	got, err := s.Resolve(path, workspace, 0)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if err := os.Chtimes(path, info.ModTime().Add(time.Second), info.ModTime().Add(time.Second)); err != nil {
		t.Fatalf("touch: %v", err)
	}
	if _, err := s.Open(got.ContentID); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("Open after an mtime change = %v, want a refusal", err)
	}
	if err := os.WriteFile(path, append(pngBytes(t, 8, 8), 0), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatalf("restore mtime: %v", err)
	}
	if _, err := s.Open(got.ContentID); err == nil {
		t.Fatal("Open served an original whose size changed")
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := s.Open(got.ContentID); err == nil {
		t.Fatal("Open served an original that was deleted")
	}
	if _, err := s.Open("never-minted"); err == nil {
		t.Fatal("Open answered an id nothing resolved")
	}
}

func TestResolveServesTheFileWhenNoDerivativeApplies(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	svg := []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg" width="4000" height="4"></svg>`)
	svgPath := writeFile(t, workspace, "diagram.svg", svg)
	smallPath := writeFile(t, workspace, "small.png", pngBytes(t, 300, 10))
	s := New()

	got, err := s.Resolve(svgPath, workspace, 320)
	if err != nil {
		t.Fatalf("Resolve svg: %v", err)
	}
	if got.Derived || got.MimeType != "image/svg+xml" || got.Width != 0 || got.OriginalWidth != 0 {
		t.Fatalf("svg = %+v, want the file with an unknown size", got)
	}
	if _, data := readAll(t, s, got.ContentID); !bytes.Equal(data, svg) {
		t.Fatalf("svg served %q", data)
	}
	small, err := s.Resolve(smallPath, workspace, 320)
	if err != nil {
		t.Fatalf("Resolve small: %v", err)
	}
	if small.Derived || small.Width != 300 || small.Height != 10 {
		t.Fatalf("small = %+v, want the 300x10 file itself", small)
	}
}

func TestResolveReadsAnExistingImageOutsideTheWorkspace(t *testing.T) {
	t.Parallel()
	path := writeFile(t, t.TempDir(), "diagram.png", pngBytes(t, 8, 8))
	if _, err := New().Resolve(path, t.TempDir(), 0); err != nil {
		t.Fatalf("Resolve outside the workspace: %v", err)
	}
}

// Every refusal names a short reason between the method prefix and the
// cause, which is what the rendered chip shows beside the alt text. Saving
// gives the same refusals.
func TestResolveNamesTheReason(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	textPath := writeFile(t, workspace, "notes.txt", []byte("not an image"))
	large := append(pngBytes(t, 8, 8), bytes.Repeat([]byte("x"), int(attachment.DisplayImageMaxBytes))...)
	largePath := writeFile(t, workspace, "large.png", large)
	// A well-formed signature and IHDR declaring 60000x60000 pixels, with
	// the CRC the decoder verifies before it reads the size.
	ihdr := append([]byte("IHDR"), 0, 0, 0xEA, 0x60, 0, 0, 0xEA, 0x60, 8, 6, 0, 0, 0)
	bomb := append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0, 0, 0, 13}, ihdr...)
	bomb = binary.BigEndian.AppendUint32(bomb, crc32.ChecksumIEEE(ihdr))
	bombPath := writeFile(t, workspace, "bomb.png", bomb)
	cases := []struct {
		name   string
		path   string
		reason string
	}{
		{"missing", filepath.Join(workspace, "missing.png"), "load local image: file not found: "},
		{"missing outside the workspace", filepath.Join(t.TempDir(), "gone.png"), "load local image: file not found: "},
		{"directory", workspace, "load local image: not a file: "},
		{"text", textPath, "load local image: not an image: "},
		{"oversized", largePath, "load local image: larger than 25 MiB: "},
		{"pixel bomb", bombPath, "load local image: too many pixels: "},
		{"traversal", filepath.Join(workspace, "a") + "/../b.png", "load local image: path not allowed: "},
		{"network share", `\\server\share\b.png`, "load local image: path not allowed: "},
	}
	for _, tc := range cases {
		for _, maxWidth := range []int{0, 320} {
			_, err := New().Resolve(tc.path, workspace, maxWidth)
			if err == nil || !strings.HasPrefix(err.Error(), tc.reason) {
				t.Fatalf("%s at %d: Resolve error = %v, want prefix %q", tc.name, maxWidth, err, tc.reason)
			}
		}
		if _, err := ReadOriginal(tc.path, workspace); err == nil || !strings.HasPrefix(err.Error(), tc.reason) {
			t.Fatalf("%s: ReadOriginal error = %v, want prefix %q", tc.name, err, tc.reason)
		}
	}
}

func TestReadOriginalReturnsTheValidatedFile(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	payload := pngBytes(t, 641, 480)
	path := writeFile(t, workspace, "shot.png", payload)
	got, err := ReadOriginal("shot.png", workspace)
	if err != nil {
		t.Fatalf("ReadOriginal: %v", err)
	}
	if got.Path != path || got.MimeType != "image/png" || !bytes.Equal(got.Data, payload) {
		t.Fatalf("ReadOriginal = (%s, %s, %d bytes), want the file", got.Path, got.MimeType, len(got.Data))
	}
}
