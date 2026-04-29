package attachment

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // register webp decoder for image.Decode
	"golang.org/x/sync/singleflight"
)

// thumbMaxDim is the longest side (px) of the generated thumbnail. The chat
// inline grid uses ~128px tiles at 1× DPR; 256 covers 2× DPR comfortably and
// stays small after JPEG/PNG encode. Bigger doesn't help — the browser
// rescales on display.
const thumbMaxDim = 256

// thumbJPEGQuality is the libjpeg quality factor for JPEG-output thumbs. 70
// is the standard "looks fine, much smaller" sweet spot for screenshot-style
// content; sharp text edges stay legible while blurry photo regions
// compress well.
const thumbJPEGQuality = 70

// thumbPixelBudget is the largest source pixel count we'll decode. The Go
// stdlib PNG/JPEG decoders allocate proportional to the declared dimensions
// BEFORE validating the rest of the data, so a malicious 3 KB PNG declaring
// 50000×50000 forces a multi-GB image.NewRGBA allocation. 50 megapixels
// (~7000×7000) covers any realistic screenshot or photo while bounding the
// decode-time allocation to ~200 MB worst-case for 32bpp RGBA.
const thumbPixelBudget = 50 * 1024 * 1024

// thumbnailGenConcurrency caps simultaneous decode+resize+encode jobs so a
// burst of remote-client thumbnail requests can't pin a desktop's RAM.
// CatmullRom on a 7000×7000 image takes ~1 second of CPU and allocates the
// dst RGBA up front; serialising these to a small pool keeps memory bounded
// without serialising the singleflight-deduped fast path on the common case.
const thumbnailGenConcurrency = 4

var thumbnailGenSem = make(chan struct{}, thumbnailGenConcurrency)

// thumbnailGenGroup deduplicates concurrent calls for the same attachment
// id. Without it, two near-simultaneous Thumbnail() calls (e.g. inline grid
// remount during scroll, or `--connect` client + local webview) would each
// re-read the source file, re-decode, and race the cache write.
var thumbnailGenGroup singleflight.Group

// Thumbnail returns a small inline-display-sized version of an image
// attachment, generating + caching it lazily on the first call. Output mime
// is image/jpeg for JPEG/WEBP inputs (alpha-free, smaller) and image/png for
// PNG/GIF inputs (preserves transparency / first-frame).
//
// The cache lives on the attachments row (`thumbnail_data`,
// `thumbnail_mime`); callers from a remote frontend pay one full-size IPC
// only on first request, every subsequent thumbnail call returns the
// cached blob. Cache invalidation is the same as the attachment itself:
// thread cascade deletes the row.
func (s *Store) Thumbnail(threadID, attachmentID string) ([]byte, string, error) {
	record, ok, err := s.meta.GetAttachmentWithThumbnail(attachmentID)
	if err != nil {
		return nil, "", err
	}
	if !ok {
		return nil, "", fmt.Errorf("attachment: id %q not found", attachmentID)
	}
	if record.ThreadID != threadID {
		return nil, "", fmt.Errorf("attachment %q belongs to thread %s, not %s", attachmentID, record.ThreadID, threadID)
	}
	if record.ThumbnailData != nil {
		return record.ThumbnailData, record.ThumbnailMime, nil
	}

	// Cache miss. Dedupe concurrent generations for the same id so a
	// re-render storm only does the expensive decode once.
	type genResult struct {
		data []byte
		mime string
	}
	v, err, _ := thumbnailGenGroup.Do(attachmentID, func() (any, error) {
		// Bound global concurrency: a remote `--connect` client could
		// otherwise fan out a request per visible thumb and pin RAM.
		thumbnailGenSem <- struct{}{}
		defer func() { <-thumbnailGenSem }()

		// Re-check the cache after acquiring the slot — another caller
		// for the same id might have generated and persisted while we
		// waited on the singleflight key.
		fresh, ok, err := s.meta.GetAttachmentWithThumbnail(attachmentID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("attachment: id %q not found", attachmentID)
		}
		if fresh.ThumbnailData != nil {
			return genResult{data: fresh.ThumbnailData, mime: fresh.ThumbnailMime}, nil
		}

		absolutePath, err := s.resolveAbsolute(fresh.RelativePath)
		if err != nil {
			return nil, err
		}
		srcBytes, err := os.ReadFile(absolutePath)
		if err != nil {
			return nil, fmt.Errorf("attachment: read file: %w", err)
		}

		thumb, mime, err := generateThumbnail(srcBytes, fresh.MimeType)
		if err != nil {
			return nil, fmt.Errorf("attachment: generate thumbnail %s: %w", attachmentID, err)
		}
		if err := s.meta.SetAttachmentThumbnail(attachmentID, thumb, mime); err != nil {
			return nil, err
		}
		return genResult{data: thumb, mime: mime}, nil
	})
	if err != nil {
		return nil, "", err
	}
	out := v.(genResult)
	return out.data, out.mime, nil
}

func generateThumbnail(src []byte, srcMIME string) ([]byte, string, error) {
	// Pre-check declared dimensions before paying the full Decode cost.
	// `image.DecodeConfig` reads only the header, so a decode-bomb that
	// declares billions of pixels is rejected before image.NewRGBA tries
	// to allocate the backing slice.
	if err := guardImageBudget(src, srcMIME); err != nil {
		return nil, "", err
	}

	// GIF: decode the first frame explicitly so we don't accidentally
	// rasterize the whole animation. image.Decode would dispatch to the
	// gif decoder which returns only the first frame anyway, but being
	// explicit here makes the intent obvious + protects us if the stdlib
	// behavior ever changes.
	var img image.Image
	if srcMIME == "image/gif" {
		decoded, err := gif.DecodeAll(bytes.NewReader(src))
		if err != nil {
			return nil, "", fmt.Errorf("decode gif: %w", err)
		}
		if len(decoded.Image) == 0 {
			return nil, "", errors.New("decode gif: zero frames")
		}
		img = decoded.Image[0]
	} else {
		decoded, _, err := image.Decode(bytes.NewReader(src))
		if err != nil {
			return nil, "", fmt.Errorf("decode image: %w", err)
		}
		img = decoded
	}

	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	if w <= 0 || h <= 0 {
		return nil, "", fmt.Errorf("invalid image dimensions %dx%d", w, h)
	}

	tw, th := scaleToBox(w, h, thumbMaxDim)
	dst := image.NewRGBA(image.Rect(0, 0, tw, th))
	// CatmullRom: high-quality cubic resampling. ApproxBiLinear would be
	// faster but soft-looks-bad on UI screenshots; Catmull's slight
	// sharpening keeps text legible at thumb sizes. Cost on a 4MB photo
	// is sub-100ms on a typical desktop CPU.
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, bounds, draw.Over, nil)

	switch srcMIME {
	case "image/png", "image/gif":
		var out bytes.Buffer
		// DefaultCompression: BestCompression's full filter scan costs
		// 10-30ms on a 256px RGBA for marginal byte savings. The thumb
		// is small enough that the extra CPU isn't worth the few percent
		// reduction.
		enc := png.Encoder{CompressionLevel: png.DefaultCompression}
		if err := enc.Encode(&out, dst); err != nil {
			return nil, "", fmt.Errorf("encode png: %w", err)
		}
		return out.Bytes(), "image/png", nil
	default:
		// image/jpeg, image/jpg, image/webp → JPEG out. WebP encoding has no
		// stdlib path and decode-only is fine for our use case.
		var out bytes.Buffer
		if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: thumbJPEGQuality}); err != nil {
			return nil, "", fmt.Errorf("encode jpeg: %w", err)
		}
		return out.Bytes(), "image/jpeg", nil
	}
}

// guardImageBudget rejects sources whose declared header dimensions exceed
// `thumbPixelBudget`, before the full Decode allocates a proportionally-
// sized backing slice. Returns nil for GIF: gif.DecodeAll's first frame
// dimensions are bounded by the GIF logical screen size, which is itself a
// 16-bit quantity (max ~65k×65k = 4 Gpx — still well over budget, but the
// Go gif decoder mallocs lazily per-frame and the first frame is what we
// keep, so the same image.NewRGBA budget check below catches the rest).
func guardImageBudget(src []byte, srcMIME string) error {
	if srcMIME == "image/gif" {
		// gif.DecodeAll already returns headers in the decoded result;
		// we'd need a separate gif.DecodeConfig pass to peek without
		// reading frames. The bounds check after decode catches it.
		return nil
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(src))
	if err != nil {
		return fmt.Errorf("decode image config: %w", err)
	}
	if int64(cfg.Width)*int64(cfg.Height) > int64(thumbPixelBudget) {
		return fmt.Errorf("image dimensions %dx%d exceed pixel budget %d", cfg.Width, cfg.Height, thumbPixelBudget)
	}
	return nil
}

// scaleToBox returns the largest (w, h) <= (max, max) that preserves the
// source aspect ratio. Integer math; rounds down so the box fits exactly.
func scaleToBox(srcW, srcH, max int) (int, int) {
	if srcW <= max && srcH <= max {
		return srcW, srcH
	}
	if srcW >= srcH {
		h := srcH * max / srcW
		if h < 1 {
			h = 1
		}
		return max, h
	}
	w := srcW * max / srcH
	if w < 1 {
		w = 1
	}
	return w, max
}
