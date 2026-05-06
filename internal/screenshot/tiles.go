package screenshot

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
)

// SliceOptions controls how a full-page PNG is sliced into JPEG
// tiles. The slicer never crops horizontally — the captured PNG is
// expected to be at the chosen viewport width already.
//
// Zero values fall back to defaults appropriate for the agent's
// vision-token budget — TileHeight 800, MaxTiles 8, JPEGQuality 85.
type SliceOptions struct {
	TileHeight  int
	MaxTiles    int
	JPEGQuality int // 1..100
}

const (
	// DefaultTileWidth is the canonical capture viewport width. Used
	// as the default ViewportWidth on a CaptureOptions and as the
	// expected horizontal extent of a sliced PNG.
	DefaultTileWidth = 1280
	// DefaultTileHeight is the per-tile height the agent reads as a
	// single image content block.
	DefaultTileHeight = 800
	// DefaultMaxTiles is the per-tool vision-token budget cap. Mirrors
	// internal/design.MaxScreenshotTiles; a contract test pins the
	// alignment so the two values can't drift.
	DefaultMaxTiles = 8
	// DefaultJPEGQuality balances bytes-on-the-wire against fidelity.
	// 85 is the same setting Puppeteer's screenshot-comparison tooling
	// uses by default.
	DefaultJPEGQuality = 85
)

// SliceResult is the bundle returned from SliceTiles. Tiles are raw
// base64 strings (no `data:image/jpeg;base64,` prefix), one per
// vertical slice top-to-bottom. Clipped is true when the source
// image had more rows than fit inside MaxTiles.
type SliceResult struct {
	Tiles   []string
	Clipped bool
}

// SliceTiles decodes pngBytes (a full-page screenshot) and returns
// JPEG tiles top-to-bottom. The last tile is the natural remaining
// height — never padded to TileHeight.
//
// Errors:
//   - source is not a valid PNG → "decode png: ..."
//   - source has zero width or height → "empty image"
//   - JPEG encoding fails → "encode tile N: ..."
//
// The function is pure (no I/O) so it can be table-tested against
// synthesized image.RGBA inputs without touching disk.
func SliceTiles(pngBytes []byte, opts SliceOptions) (SliceResult, error) {
	opts = opts.withDefaults()

	src, err := png.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		return SliceResult{}, fmt.Errorf("decode png: %w", err)
	}
	bounds := src.Bounds()
	if bounds.Dx() == 0 || bounds.Dy() == 0 {
		return SliceResult{}, fmt.Errorf("screenshot: empty image")
	}

	totalH := bounds.Dy()
	tiles := make([]string, 0, opts.MaxTiles)

	y := 0
	for y < totalH && len(tiles) < opts.MaxTiles {
		h := opts.TileHeight
		if y+h > totalH {
			h = totalH - y
		}
		tileRect := image.Rect(bounds.Min.X, bounds.Min.Y+y, bounds.Max.X, bounds.Min.Y+y+h)

		// SubImage avoids copying the pixel data; jpeg.Encode reads
		// from it directly. Most image types in stdlib implement
		// SubImage; if a future image type doesn't, we fall back to a
		// plain image.RGBA copy.
		var tileImg image.Image
		if sub, ok := src.(interface {
			SubImage(r image.Rectangle) image.Image
		}); ok {
			tileImg = sub.SubImage(tileRect)
		} else {
			rgba := image.NewRGBA(image.Rect(0, 0, tileRect.Dx(), tileRect.Dy()))
			for ty := 0; ty < tileRect.Dy(); ty++ {
				for tx := 0; tx < tileRect.Dx(); tx++ {
					rgba.Set(tx, ty, src.At(tileRect.Min.X+tx, tileRect.Min.Y+ty))
				}
			}
			tileImg = rgba
		}

		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, tileImg, &jpeg.Options{Quality: opts.JPEGQuality}); err != nil {
			return SliceResult{}, fmt.Errorf("encode tile %d: %w", len(tiles), err)
		}

		tiles = append(tiles, base64.StdEncoding.EncodeToString(buf.Bytes()))
		y += h
	}

	return SliceResult{
		Tiles:   tiles,
		Clipped: y < totalH,
	}, nil
}

func (o SliceOptions) withDefaults() SliceOptions {
	if o.TileHeight <= 0 {
		o.TileHeight = DefaultTileHeight
	}
	if o.MaxTiles <= 0 {
		o.MaxTiles = DefaultMaxTiles
	}
	if o.JPEGQuality <= 0 || o.JPEGQuality > 100 {
		o.JPEGQuality = DefaultJPEGQuality
	}
	return o
}
