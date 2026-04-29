package attachment

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
	"sync"
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
	row, ok, err := meta.GetAttachmentWithThumbnail(record.ID)
	if err != nil {
		t.Fatalf("GetAttachmentWithThumbnail: %v", err)
	}
	if !ok {
		t.Fatal("expected attachment row to still exist")
	}
	if row.ThumbnailData == nil {
		t.Fatal("expected DB to carry cached thumbnail after first generation")
	}
	if !bytes.Equal(row.ThumbnailData, got) {
		t.Fatal("DB-cached bytes don't match returned bytes")
	}
	if row.ThumbnailMime != mime {
		t.Fatalf("DB-cached mime %q != returned mime %q", row.ThumbnailMime, mime)
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

// realGIF returns a 96x48 GIF (single frame). The GIF first-frame branch
// in generateThumbnail is otherwise untested.
func realGIF(t *testing.T) []byte {
	t.Helper()
	rect := image.Rect(0, 0, 96, 48)
	palette := color.Palette{
		color.RGBA{R: 0xff, G: 0, B: 0, A: 0xff},
		color.RGBA{R: 0, G: 0xff, B: 0, A: 0xff},
		color.RGBA{R: 0, G: 0, B: 0xff, A: 0xff},
	}
	img := image.NewPaletted(rect, palette)
	for y := 0; y < 48; y++ {
		for x := 0; x < 96; x++ {
			img.SetColorIndex(x, y, uint8((x+y)%len(palette)))
		}
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, &gif.GIF{Image: []*image.Paletted{img}, Delay: []int{0}}); err != nil {
		t.Fatalf("encode test gif: %v", err)
	}
	return buf.Bytes()
}

func TestThumbnailGIFFirstFrame(t *testing.T) {
	attStore, meta := newTestStores(t)
	seedThread(t, meta, "thread-thumb-gif")
	src := realGIF(t)
	record, err := attStore.Upload("thread-thumb-gif", "anim.gif", "image/gif", base64.StdEncoding.EncodeToString(src), 1)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	got, mime, err := attStore.Thumbnail(record.ThreadID, record.ID)
	if err != nil {
		t.Fatalf("Thumbnail: %v", err)
	}
	if mime != "image/png" {
		t.Fatalf("GIF input should produce PNG output, got %q", mime)
	}
	// Thumbnail must be a valid PNG of the first frame's dimensions.
	thumbImg, err := png.Decode(bytes.NewReader(got))
	if err != nil {
		t.Fatalf("decode generated thumb: %v", err)
	}
	if thumbImg.Bounds().Dx() != 96 || thumbImg.Bounds().Dy() != 48 {
		t.Fatalf("expected 96x48 (no upscale), got %v", thumbImg.Bounds())
	}
}

func TestGenerateThumbnailRejectsCorruptBytes(t *testing.T) {
	// Pin: corrupt bytes surface as a wrapped decode error rather than
	// a panic or stalled generator. Tested at the generateThumbnail
	// level — Upload() refuses to accept bytes whose magic numbers
	// don't match the declared MIME, so a corrupt-source path can
	// only arise from on-disk degradation of a once-valid upload.
	bogus := []byte("definitely not a real PNG, no IHDR, no IDAT")
	_, _, err := generateThumbnail(bogus, "image/png")
	if err == nil {
		t.Fatal("expected error decoding corrupt PNG")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Fatalf("error should mention decode, got %q", err)
	}
}

func TestGenerateThumbnailGIFErrorWraps(t *testing.T) {
	// Regression: an empty-frame GIF previously surfaced the unwrap-of-nil
	// formatting bug "decode gif: %!w(<nil>)". Now: a clean errors.New.
	emptyGIF := []byte{
		0x47, 0x49, 0x46, 0x38, 0x39, 0x61, // "GIF89a"
		0x01, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, // 1x1, no global color table
		0x3b, // trailer (immediately, so 0 frames)
	}
	_, _, err := generateThumbnail(emptyGIF, "image/gif")
	if err == nil {
		t.Skip("stdlib gif decoder accepted zero-frame GIF (unlikely path)")
	}
	if strings.Contains(err.Error(), "%!w(<nil>)") {
		t.Fatalf("error format bug regressed: %q", err)
	}
}

func TestThumbnailRejectsDecodeBomb(t *testing.T) {
	// Construct a PNG header that declares a 100,000×100,000 image (10
	// gigapixels — well past the 50 Mpx budget) but supplies no real
	// pixel data. Without the DecodeConfig pre-check, image.NewRGBA
	// inside Decode would attempt a ~40 GB allocation and OOM the
	// process. With the pre-check, we reject before allocation.
	bomb := pngWithDeclaredDimensions(t, 100_000, 100_000)
	if err := guardImageBudget(bomb, "image/png"); err == nil {
		t.Fatal("expected decode-bomb pre-check to reject 100k×100k declared image")
	}
}

// pngWithDeclaredDimensions returns the minimum PNG byte sequence that has a
// valid signature + IHDR chunk declaring the requested dimensions. Used to
// exercise the DecodeConfig path without allocating the real pixel buffer.
func pngWithDeclaredDimensions(t *testing.T, width, height int) []byte {
	t.Helper()
	// We rely on the Go stdlib png package to produce a valid IHDR by
	// encoding a 1×1 image, then patching the width/height bytes. This
	// gives us a parseable IHDR for image.DecodeConfig without us
	// hand-rolling the chunk + CRC32.
	var buf bytes.Buffer
	tiny := image.NewRGBA(image.Rect(0, 0, 1, 1))
	if err := png.Encode(&buf, tiny); err != nil {
		t.Fatalf("encode tiny png: %v", err)
	}
	out := buf.Bytes()
	// IHDR is the first chunk after the 8-byte PNG signature. Its layout
	// is: 4-byte length, 4-byte type "IHDR", 4-byte width (BE), 4-byte
	// height (BE), then bit-depth/color-type/etc., then 4-byte CRC.
	const ihdrWidthOffset = 8 + 4 + 4 // signature + chunk-len + chunk-type
	out[ihdrWidthOffset+0] = byte(width >> 24)
	out[ihdrWidthOffset+1] = byte(width >> 16)
	out[ihdrWidthOffset+2] = byte(width >> 8)
	out[ihdrWidthOffset+3] = byte(width)
	out[ihdrWidthOffset+4] = byte(height >> 24)
	out[ihdrWidthOffset+5] = byte(height >> 16)
	out[ihdrWidthOffset+6] = byte(height >> 8)
	out[ihdrWidthOffset+7] = byte(height)
	// We deliberately do NOT recompute the CRC. image.DecodeConfig
	// reads the IHDR and accepts it without validating the chunk CRC,
	// which is exactly the attack surface guardImageBudget protects.
	return out
}

func TestThumbnailDedupesConcurrentSameID(t *testing.T) {
	// singleflight + per-id semaphore: two near-simultaneous Thumbnail
	// calls for the same id must produce the same bytes (no race on the
	// cache write) and at most one decode pass internally. We assert
	// the byte identity here; the singleflight is deduplication, not
	// serialisation, so timing isn't asserted.
	attStore, meta := newTestStores(t)
	seedThread(t, meta, "thread-thumb-conc")
	src := realPNG(t)
	record, err := attStore.Upload("thread-thumb-conc", "shot.png", "image/png", base64.StdEncoding.EncodeToString(src), 1)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	const N = 8
	var wg sync.WaitGroup
	results := make([][]byte, N)
	errs := make([]error, N)
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			got, _, err := attStore.Thumbnail(record.ThreadID, record.ID)
			results[i] = got
			errs[i] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Thumbnail #%d: %v", i, err)
		}
	}
	first := results[0]
	if len(first) == 0 {
		t.Fatal("first call returned empty")
	}
	for i := 1; i < N; i++ {
		if !bytes.Equal(first, results[i]) {
			t.Fatalf("Thumbnail #%d returned %d bytes, want %d (race on cache write?)", i, len(results[i]), len(first))
		}
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
