package attachment

import (
	"bytes"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // register webp decoder for image.Decode
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
	record, ok, err := s.meta.GetAttachment(attachmentID)
	if err != nil {
		return nil, "", err
	}
	if !ok {
		return nil, "", fmt.Errorf("attachment: id %q not found", attachmentID)
	}
	if record.ThreadID != threadID {
		return nil, "", fmt.Errorf("attachment %q belongs to thread %s, not %s", attachmentID, record.ThreadID, threadID)
	}

	if cached, mime, hit, err := s.meta.GetAttachmentThumbnail(attachmentID); err == nil && hit {
		return cached, mime, nil
	} else if err != nil {
		return nil, "", err
	}

	absolutePath, err := s.resolveAbsolute(record.RelativePath)
	if err != nil {
		return nil, "", err
	}
	srcBytes, err := os.ReadFile(absolutePath)
	if err != nil {
		return nil, "", fmt.Errorf("attachment: read file: %w", err)
	}

	thumb, mime, err := generateThumbnail(srcBytes, record.MimeType)
	if err != nil {
		return nil, "", fmt.Errorf("attachment: generate thumbnail %s: %w", attachmentID, err)
	}
	if err := s.meta.SetAttachmentThumbnail(attachmentID, thumb, mime); err != nil {
		return nil, "", err
	}
	return thumb, mime, nil
}

func generateThumbnail(src []byte, srcMIME string) ([]byte, string, error) {
	// GIF: decode the first frame explicitly so we don't accidentally
	// rasterize the whole animation. image.Decode would dispatch to the
	// gif decoder which returns only the first frame anyway, but being
	// explicit here makes the intent obvious + protects us if the stdlib
	// behavior ever changes.
	var img image.Image
	if srcMIME == "image/gif" {
		decoded, err := gif.DecodeAll(bytes.NewReader(src))
		if err != nil || len(decoded.Image) == 0 {
			return nil, "", fmt.Errorf("decode gif: %w", err)
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
		enc := png.Encoder{CompressionLevel: png.BestCompression}
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

