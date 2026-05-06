package screenshot

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

// makePNG produces a synthetic PNG of the given dimensions with a
// solid color pattern that lets the test verify which slice it
// received: every row r in [0..h) is colored uniformly by (r&0xFF).
// A tile that starts at row y and is dy rows tall will have its top
// row equal to (y & 0xFF) — easy to assert post-decode without
// pulling in image-comparison libraries.
func makePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		c := color.RGBA{R: uint8(y & 0xff), G: 0, B: 0, A: 255}
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode synthetic png: %v", err)
	}
	return buf.Bytes()
}

func decodeJPEGFromBase64(t *testing.T, s string) image.Image {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	img, err := jpeg.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("jpeg decode: %v", err)
	}
	return img
}

// TestSliceTiles_SinglePage covers a page that fits in one tile —
// the most common case for a small design preview.
func TestSliceTiles_SinglePage(t *testing.T) {
	src := makePNG(t, 400, 600)
	res, err := SliceTiles(src, SliceOptions{TileHeight: 800, MaxTiles: 8, JPEGQuality: 85})
	if err != nil {
		t.Fatalf("SliceTiles: %v", err)
	}
	if len(res.Tiles) != 1 {
		t.Fatalf("len(Tiles) = %d, want 1", len(res.Tiles))
	}
	if res.Clipped {
		t.Error("Clipped = true, want false (page fits within MaxTiles)")
	}
	img := decodeJPEGFromBase64(t, res.Tiles[0])
	b := img.Bounds()
	if b.Dx() != 400 || b.Dy() != 600 {
		t.Fatalf("tile bounds = %dx%d, want 400x600", b.Dx(), b.Dy())
	}
}

// TestSliceTiles_ExactMultiple covers a page that splits cleanly
// into N tiles of exactly TileHeight each.
func TestSliceTiles_ExactMultiple(t *testing.T) {
	src := makePNG(t, 400, 1600) // exactly 2 × 800
	res, err := SliceTiles(src, SliceOptions{TileHeight: 800, MaxTiles: 8})
	if err != nil {
		t.Fatalf("SliceTiles: %v", err)
	}
	if len(res.Tiles) != 2 {
		t.Fatalf("len(Tiles) = %d, want 2", len(res.Tiles))
	}
	if res.Clipped {
		t.Error("Clipped = true, want false (exact fit, no overflow)")
	}
	for i, b64 := range res.Tiles {
		img := decodeJPEGFromBase64(t, b64)
		bounds := img.Bounds()
		if bounds.Dy() != 800 {
			t.Errorf("tile %d height = %d, want 800", i, bounds.Dy())
		}
	}
}

// TestSliceTiles_PartialLastTile covers a page whose final tile
// doesn't fill TileHeight — the last tile must be the natural
// remaining height, NOT padded.
func TestSliceTiles_PartialLastTile(t *testing.T) {
	src := makePNG(t, 400, 1000) // 800 + 200 partial
	res, err := SliceTiles(src, SliceOptions{TileHeight: 800, MaxTiles: 8})
	if err != nil {
		t.Fatalf("SliceTiles: %v", err)
	}
	if len(res.Tiles) != 2 {
		t.Fatalf("len(Tiles) = %d, want 2", len(res.Tiles))
	}
	if res.Clipped {
		t.Error("Clipped = true, want false")
	}
	first := decodeJPEGFromBase64(t, res.Tiles[0])
	if first.Bounds().Dy() != 800 {
		t.Errorf("first tile height = %d, want 800", first.Bounds().Dy())
	}
	last := decodeJPEGFromBase64(t, res.Tiles[1])
	if last.Bounds().Dy() != 200 {
		t.Errorf("last tile height = %d, want 200 (no padding)", last.Bounds().Dy())
	}
}

// TestSliceTiles_ClippedAtMaxTiles covers a page taller than
// MaxTiles × TileHeight: we cap at MaxTiles and set Clipped = true
// so the agent knows there's content below.
func TestSliceTiles_ClippedAtMaxTiles(t *testing.T) {
	// 9 × 800 = 7200, MaxTiles = 8 → trailing 800 dropped.
	src := makePNG(t, 400, 9*800)
	res, err := SliceTiles(src, SliceOptions{TileHeight: 800, MaxTiles: 8})
	if err != nil {
		t.Fatalf("SliceTiles: %v", err)
	}
	if len(res.Tiles) != 8 {
		t.Fatalf("len(Tiles) = %d, want 8 (MaxTiles cap)", len(res.Tiles))
	}
	if !res.Clipped {
		t.Error("Clipped = false, want true (page exceeded MaxTiles × TileHeight)")
	}
}

// TestSliceTiles_TilesAreInOrder verifies tiles come out
// top-to-bottom by checking the synthetic color pattern: row N has
// red channel = N & 0xff. Tile N's first row should match the
// expected source row.
func TestSliceTiles_TilesAreInOrder(t *testing.T) {
	src := makePNG(t, 64, 1600) // 2 tiles of 800
	res, err := SliceTiles(src, SliceOptions{TileHeight: 800, MaxTiles: 8})
	if err != nil {
		t.Fatalf("SliceTiles: %v", err)
	}
	if len(res.Tiles) != 2 {
		t.Fatalf("len(Tiles) = %d, want 2", len(res.Tiles))
	}
	// JPEG is lossy; values can drift a few units from the source.
	// Use a tolerance band rather than exact equality.
	red := func(img image.Image, x, y int) uint8 {
		r, _, _, _ := img.At(x, y).RGBA()
		return uint8(r >> 8)
	}
	near := func(got, want uint8) bool {
		d := int(got) - int(want)
		if d < 0 {
			d = -d
		}
		return d <= 6
	}
	// Tile 0 starts at source row 0 (red=0).
	tile0 := decodeJPEGFromBase64(t, res.Tiles[0])
	if got := red(tile0, 0, 0); !near(got, 0) {
		t.Errorf("tile 0 row 0 red = %d, want ~0", got)
	}
	// Tile 1 starts at source row 800 (red=0x20).
	tile1 := decodeJPEGFromBase64(t, res.Tiles[1])
	if got := red(tile1, 0, 0); !near(got, 0x20) {
		t.Errorf("tile 1 row 0 red = %d, want ~%d", got, 0x20)
	}
}

// TestSliceTiles_DefaultsApplied verifies a zero-valued opts gets
// the documented defaults — TileHeight 800, MaxTiles 8, JPEG 85.
func TestSliceTiles_DefaultsApplied(t *testing.T) {
	// 9 × DefaultTileHeight to force MaxTiles cap.
	src := makePNG(t, 400, 9*DefaultTileHeight)
	res, err := SliceTiles(src, SliceOptions{})
	if err != nil {
		t.Fatalf("SliceTiles: %v", err)
	}
	if len(res.Tiles) != DefaultMaxTiles {
		t.Fatalf("len(Tiles) = %d, want %d (DefaultMaxTiles)", len(res.Tiles), DefaultMaxTiles)
	}
	if !res.Clipped {
		t.Error("Clipped = false, want true (defaults should produce a clip on tall page)")
	}
	// Each tile should be DefaultTileHeight tall except possibly the
	// last; with 9 source tile-heights and 8 emitted, all 8 are full.
	for i, b64 := range res.Tiles {
		img := decodeJPEGFromBase64(t, b64)
		if img.Bounds().Dy() != DefaultTileHeight {
			t.Errorf("tile %d height = %d, want %d", i, img.Bounds().Dy(), DefaultTileHeight)
		}
	}
}

// TestSliceTiles_InvalidPNG covers the decode-error path.
func TestSliceTiles_InvalidPNG(t *testing.T) {
	_, err := SliceTiles([]byte("not a png"), SliceOptions{})
	if err == nil {
		t.Fatal("SliceTiles(garbage) = nil err, want error")
	}
}

// Empty-dimension PNGs can't be constructed via the standard
// encoder (0×0 is rejected). The slicer's `empty image` guard is
// defensive against any image.Image whose bounds report zero
// width or height — real PNG decoding never lands there. We
// exercise the guard via a fake decoder by going through the
// stdlib `image.NewRGBA` path with bounds.Dx()=0 inside a
// re-encoded PNG of width 1 — png.Decode preserves that as Dx=1,
// so the guard isn't reachable through public API. Leaving the
// guard in for defense in depth and not asserting it here.

// TestSliceTiles_OneTilePerRowAtTinyTileHeight covers the boundary
// case where TileHeight=1 and the source is 5 rows: we should get
// MaxTiles=5 tiles capped (or fewer if MaxTiles is lower) — proving
// the loop respects MaxTiles even for unusual configs.
func TestSliceTiles_RespectsMaxTilesBelowDefault(t *testing.T) {
	src := makePNG(t, 400, 1600)
	res, err := SliceTiles(src, SliceOptions{TileHeight: 100, MaxTiles: 3})
	if err != nil {
		t.Fatalf("SliceTiles: %v", err)
	}
	if len(res.Tiles) != 3 {
		t.Fatalf("len(Tiles) = %d, want 3 (MaxTiles=3)", len(res.Tiles))
	}
	if !res.Clipped {
		t.Error("Clipped = false, want true (1600 rows / 100 = 16 tiles, capped at 3)")
	}
}

// TestSliceTiles_QualityRespected proves the JPEGQuality field
// actually changes the output. Lower quality → smaller bytes for
// the same image. Not a strict equality assertion (jpeg encoding
// has internal heuristics that can produce identical sizes for
// trivial images) but a strict ordering one.
func TestSliceTiles_QualityRespected(t *testing.T) {
	// Use a high-frequency pattern so quality has visible byte impact.
	w, h := 256, 256
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{uint8(x ^ y), uint8(x*7 + y*3), uint8(x*y%255 + 1), 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode: %v", err)
	}

	low, err := SliceTiles(buf.Bytes(), SliceOptions{TileHeight: 800, MaxTiles: 8, JPEGQuality: 30})
	if err != nil {
		t.Fatalf("low: %v", err)
	}
	high, err := SliceTiles(buf.Bytes(), SliceOptions{TileHeight: 800, MaxTiles: 8, JPEGQuality: 95})
	if err != nil {
		t.Fatalf("high: %v", err)
	}
	if len(low.Tiles) != 1 || len(high.Tiles) != 1 {
		t.Fatalf("expected 1 tile each, got low=%d high=%d", len(low.Tiles), len(high.Tiles))
	}
	if len(low.Tiles[0]) >= len(high.Tiles[0]) {
		t.Errorf("low-quality tile (%d bytes) is not smaller than high-quality (%d bytes)",
			len(low.Tiles[0]), len(high.Tiles[0]))
	}
}
