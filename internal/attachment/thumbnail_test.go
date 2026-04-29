package attachment

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

// realPNG returns a 64x32 PNG with a recognisable color so the thumb decoder
// can verify dimensions + that re-encoding survived. Bigger than the
// thumb max so the resize branch is exercised; staying small to keep the
// test fast.
func realPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 64, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 4), G: uint8(y * 8), B: 0x40, A: 0xff})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode test png: %v", err)
	}
	return buf.Bytes()
}

func realJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 80, 60))
	for y := 0; y < 60; y++ {
		for x := 0; x < 80; x++ {
			img.Set(x, y, color.RGBA{R: 0x80, G: uint8(x), B: uint8(y * 3), A: 0xff})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatalf("encode test jpeg: %v", err)
	}
	return buf.Bytes()
}

func TestThumbnailGeneratesAndCachesForPNG(t *testing.T) {
	attStore, meta := newTestStores(t)
	seedThread(t, meta, "thread-thumb-png")

	// Upload a real PNG so Thumbnail can read it from disk.
	src := realPNG(t)
	record, err := attStore.Upload("thread-thumb-png", "shot.png", "image/png", base64.StdEncoding.EncodeToString(src), 1)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	// First call: cache miss → generates and persists.
	got, mime, err := attStore.Thumbnail(record.ThreadID, record.ID)
	if err != nil {
		t.Fatalf("Thumbnail (first call): %v", err)
	}
	if mime != "image/png" {
		t.Fatalf("PNG input should produce PNG output, got %q", mime)
	}
	if len(got) == 0 {
		t.Fatal("Thumbnail returned empty bytes")
	}
	// Decode to verify it's a valid image and within the bounding box.
	thumbImg, err := png.Decode(bytes.NewReader(got))
	if err != nil {
		t.Fatalf("decode generated thumb: %v", err)
	}
	w, h := thumbImg.Bounds().Dx(), thumbImg.Bounds().Dy()
	if w > thumbMaxDim || h > thumbMaxDim {
		t.Fatalf("thumb %dx%d exceeds max %d", w, h, thumbMaxDim)
	}
	// Source aspect 64:32 = 2:1. Output should preserve that. With 64x32
	// already under the 256 max-dim the scaler returns the source as-is.
	if w != 64 || h != 32 {
		t.Fatalf("thumb should preserve 64x32 source dims (no upscale), got %dx%d", w, h)
	}

	// Second call: cache hit → returns same bytes without regenerating.
	cached, _, err := attStore.Thumbnail(record.ThreadID, record.ID)
	if err != nil {
		t.Fatalf("Thumbnail (cached call): %v", err)
	}
	if !bytes.Equal(got, cached) {
		t.Fatal("expected cached thumbnail bytes to be identical to the first call")
	}

	// Verify the cache row was actually written to SQLite.
	rowData, rowMime, hit, err := meta.GetAttachmentThumbnail(record.ID)
	if err != nil {
		t.Fatalf("GetAttachmentThumbnail: %v", err)
	}
	if !hit {
		t.Fatal("expected DB to carry cached thumbnail after first generation")
	}
	if !bytes.Equal(rowData, got) {
		t.Fatal("DB-cached bytes don't match returned bytes")
	}
	if rowMime != mime {
		t.Fatalf("DB-cached mime %q != returned mime %q", rowMime, mime)
	}
}

func TestThumbnailJPEGInputProducesJPEGOutput(t *testing.T) {
	attStore, meta := newTestStores(t)
	seedThread(t, meta, "thread-thumb-jpeg")
	src := realJPEG(t)
	record, err := attStore.Upload("thread-thumb-jpeg", "photo.jpg", "image/jpeg", base64.StdEncoding.EncodeToString(src), 1)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	_, mime, err := attStore.Thumbnail(record.ThreadID, record.ID)
	if err != nil {
		t.Fatalf("Thumbnail: %v", err)
	}
	if mime != "image/jpeg" {
		t.Fatalf("JPEG input should produce JPEG output, got %q", mime)
	}
}

func TestThumbnailRejectsCrossThreadID(t *testing.T) {
	attStore, meta := newTestStores(t)
	seedThread(t, meta, "thread-a")
	seedThread(t, meta, "thread-b")
	src := realPNG(t)
	record, err := attStore.Upload("thread-a", "shot.png", "image/png", base64.StdEncoding.EncodeToString(src), 1)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, _, err := attStore.Thumbnail("thread-b", record.ID); err == nil {
		t.Fatal("expected Thumbnail to reject cross-thread id")
	}
}

func TestThumbnailMissingAttachment(t *testing.T) {
	attStore, meta := newTestStores(t)
	seedThread(t, meta, "thread-x")
	if _, _, err := attStore.Thumbnail("thread-x", "no-such-id"); err == nil {
		t.Fatal("expected error for missing attachment")
	}
}

func TestScaleToBox(t *testing.T) {
	cases := []struct{ w, h, max, ew, eh int }{
		{64, 32, 256, 64, 32},      // already small: no upscale
		{1024, 512, 256, 256, 128}, // wide: 2:1 → 256x128
		{512, 1024, 256, 128, 256}, // tall: 1:2 → 128x256
		{500, 500, 256, 256, 256},  // square at exactly max
		{300, 100, 256, 256, 85},   // 3:1 ratio
	}
	for _, c := range cases {
		gw, gh := scaleToBox(c.w, c.h, c.max)
		if gw != c.ew || gh != c.eh {
			t.Errorf("scaleToBox(%d,%d,%d) = (%d,%d), want (%d,%d)", c.w, c.h, c.max, gw, gh, c.ew, c.eh)
		}
	}
}
