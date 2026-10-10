package attachment

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"sync/atomic"

	_ "golang.org/x/image/webp" // register webp decoder for image.Decode
	"golang.org/x/sync/semaphore"
)

// The decode, resample and encode steps thumbnails and display derivatives
// share, and the memory budget every one of them runs under.

// thumbPixelBudget is the largest source pixel count we'll decode. The Go
// stdlib PNG/JPEG decoders allocate proportional to the declared dimensions
// BEFORE validating the rest of the data, so a malicious 3 KB PNG declaring
// 50000×50000 forces a multi-GB image.NewRGBA allocation. 50 megapixels
// (~7000×7000) covers any realistic screenshot or photo while bounding the
// decode-time allocation to ~200 MB worst-case for 32bpp RGBA.
const thumbPixelBudget = 50 * 1024 * 1024

// decodeMemoryBudget bounds the transient memory all concurrent decode +
// resample + encode jobs hold together. A count of jobs cannot bound it: a
// 16-bit source at the pixel budget decodes to 400 MiB, a screenshot to tens.
// Each job therefore acquires its estimated cost (resampleCost). 1 GiB holds
// two thumbnails of the largest image the pixel budget admits, or one
// derivation of it beside one thumbnail, or many jobs on screenshots, and
// makes another wait for room instead of stacking on them.
//
// Every job on a source up to 9.5 million pixels wide fits. The worst
// derivation is a 16-bit source at the pixel budget (400 MiB) whose
// derivative is at deriveMaxPixels (64 MiB, and an encoded copy no larger),
// plus the resampler's scratch of a few hundred bytes per source column
// (resample.go): about 530 MiB. A thumbnail of it holds the source plus a
// few MiB. The scratch follows the source width rather than its pixel
// count, so a wider source, at most 5 rows tall under the pixel budget, can
// exceed the budget; acquireDecodeMemory refuses it rather than wait for
// room that cannot exist.
const decodeMemoryBudget int64 = 1 << 30

var (
	decodeMemory      = semaphore.NewWeighted(decodeMemoryBudget)
	decodeMemoryLimit = decodeMemoryBudget
	// decodeMemoryHeld is the budget currently acquired, for tests and
	// diagnostics.
	decodeMemoryHeld atomic.Int64
	// decodeAcquired runs after a job acquires its budget and before it
	// decodes. Tests replace it to observe or hold a job inside the window.
	decodeAcquired = func(cost int64) {}
)

// acquireDecodeMemory blocks until cost bytes of the budget are free and
// returns the release. An estimate above the whole budget is refused as
// ErrPixelBudget: the semaphore would otherwise wait for it forever.
func acquireDecodeMemory(cost int64) (release func(), err error) {
	sem, limit := decodeMemory, decodeMemoryLimit
	if cost > limit {
		return nil, fmt.Errorf("%w: decoding needs about %d bytes, more than the %d-byte decode budget", ErrPixelBudget, cost, limit)
	}
	// Acquire with a background context cannot fail; the check is the
	// contract with the semaphore, not a reachable path.
	if err := sem.Acquire(context.Background(), cost); err != nil {
		return nil, fmt.Errorf("acquire decode memory: %w", err)
	}
	decodeMemoryHeld.Add(cost)
	decodeAcquired(cost)
	return func() {
		decodeMemoryHeld.Add(-cost)
		sem.Release(cost)
	}, nil
}

// resampleCost estimates the peak bytes one job holds: the decoded source,
// the resampler's scratch (resampleScratchBytes), the destination pixels and
// an encoded copy no larger than them.
func resampleCost(cfg image.Config, dstW, dstH int) int64 {
	source := int64(cfg.Width) * int64(cfg.Height) * decodedBytesPerPixel(cfg.ColorModel)
	scratch := resampleScratchBytes(dstW, dstH, cfg.Width, cfg.Height)
	destination := int64(dstW) * int64(dstH) * 4
	return source + scratch + 2*destination
}

// decodedBytesPerPixel is the in-memory size of one decoded pixel for a
// header's color model, rounded up to the 32-bit case for every model the
// decoders store more compactly (paletted, gray, YCbCr).
func decodedBytesPerPixel(model color.Model) int64 {
	// A palette is a slice, which an interface comparison would panic on.
	if _, paletted := model.(color.Palette); paletted {
		return 4
	}
	switch model {
	case color.RGBA64Model, color.NRGBA64Model, color.Gray16Model, color.Alpha16Model:
		return 8
	}
	return 4
}

// ErrPixelBudget marks an image whose declared dimensions exceed the decode
// budget. Callers that name the refusal to a person match it with errors.Is.
var ErrPixelBudget = errors.New("image exceeds the pixel budget")

// imageConfig reads an image header and rejects corrupt images and sources
// whose dimensions exceed the decode budget. It runs before browser delivery
// or a full Go decode, so a tiny file with hostile dimensions cannot force
// either process to allocate a multi-gigabyte pixel buffer.
func imageConfig(src []byte) (image.Config, error) {
	return imageConfigFrom(bytes.NewReader(src))
}

// imageConfigFrom is imageConfig over a reader, which it reads only as far
// as the header.
func imageConfigFrom(r io.Reader) (image.Config, error) {
	cfg, _, err := image.DecodeConfig(r)
	if err != nil {
		return image.Config{}, fmt.Errorf("decode image config: %w", err)
	}
	if int64(cfg.Width)*int64(cfg.Height) > int64(thumbPixelBudget) {
		return image.Config{}, fmt.Errorf("%w: %dx%d exceeds %d pixels", ErrPixelBudget, cfg.Width, cfg.Height, thumbPixelBudget)
	}
	return cfg, nil
}

// ImageDimensions reads the declared width and height of an image header
// under the pixel budget (imageConfig).
func ImageDimensions(src []byte) (width, height int, err error) {
	cfg, err := imageConfig(src)
	if err != nil {
		return 0, 0, err
	}
	return cfg.Width, cfg.Height, nil
}

// decodeImage fully decodes a source whose header already passed
// imageConfig. A GIF decodes to its first frame: gif.Decode returns after it.
func decodeImage(src []byte) (image.Image, error) {
	img, _, err := image.Decode(bytes.NewReader(src))
	if err != nil {
		return nil, fmt.Errorf("decode image: %w", err)
	}
	if bounds := img.Bounds(); bounds.Dx() <= 0 || bounds.Dy() <= 0 {
		return nil, fmt.Errorf("invalid image dimensions %dx%d", bounds.Dx(), bounds.Dy())
	}
	return img, nil
}

// encodePNG uses DefaultCompression: BestCompression's full filter scan
// costs far more CPU for a few percent fewer bytes.
func encodePNG(img image.Image) ([]byte, error) {
	var out bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.DefaultCompression}
	if err := enc.Encode(&out, img); err != nil {
		return nil, fmt.Errorf("encode png: %w", err)
	}
	return out.Bytes(), nil
}

func encodeJPEG(img image.Image, quality int) ([]byte, error) {
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, fmt.Errorf("encode jpeg: %w", err)
	}
	return out.Bytes(), nil
}
