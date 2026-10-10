package attachment

import (
	"fmt"
	"image"
	"log"
	"os"

	"agent-overflow/internal/store"

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
	owned, err := s.meta.OwnsAttachment(threadID, attachmentID)
	if err != nil {
		return nil, "", err
	}
	if !owned {
		return nil, "", fmt.Errorf("attachment %q belongs to thread %s, not %s", attachmentID, record.ThreadID, threadID)
	}
	// Refused on the ROW, before any decode is attempted. A `file` carries
	// arbitrary bytes that no decoder should be pointed at, and a file has
	// no visual representation to cache even if one succeeded.
	if record.Kind != store.AttachmentKindImage {
		return nil, "", fmt.Errorf("%w: %q is a %s attachment", ErrNotAnImage, attachmentID, record.Kind)
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
		// Re-check the cache inside the flight: another caller for the
		// same id might have generated and persisted while we waited on
		// the singleflight key.
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

// OriginalSize reads the pixel size of an image attachment's stored original
// from its header, without decoding the pixels, so a client can reserve the
// full-size box before the bytes arrive. A header that does not read (or
// declares more than the pixel budget) answers 0, 0: the size is a layout
// hint, and its absence is not a failure. Ownership and kind are checked as
// for every byte accessor.
func (s *Store) OriginalSize(threadID, attachmentID string) (width, height int, err error) {
	record, absolutePath, err := s.resolveThreadAttachment(threadID, attachmentID)
	if err != nil {
		return 0, 0, err
	}
	if record.Kind != store.AttachmentKindImage {
		return 0, 0, fmt.Errorf("%w: %q is a %s attachment", ErrNotAnImage, attachmentID, record.Kind)
	}
	file, err := os.Open(absolutePath)
	if err != nil {
		return 0, 0, nil
	}
	defer func() {
		// The header was already read; a failed close changes nothing the
		// caller receives.
		if closeErr := file.Close(); closeErr != nil {
			log.Printf("attachment: close %s: %v", absolutePath, closeErr)
		}
	}()
	cfg, err := imageConfigFrom(file)
	if err != nil {
		return 0, 0, nil
	}
	return cfg.Width, cfg.Height, nil
}

// generateThumbnail decodes, scales and encodes one thumbnail under the
// shared decode memory budget (imagecodec.go), so a remote `--connect`
// client fanning out a request per visible thumb waits for room rather than
// pinning RAM.
func generateThumbnail(src []byte, srcMIME string) ([]byte, string, error) {
	// The header is read first, so a decode bomb that declares billions of
	// pixels is rejected before anything allocates its pixel buffer.
	cfg, err := imageConfig(src)
	if err != nil {
		return nil, "", err
	}
	tw, th := scaleToBox(cfg.Width, cfg.Height, thumbMaxDim)
	release, err := acquireDecodeMemory(resampleCost(cfg, tw, th))
	if err != nil {
		return nil, "", err
	}
	defer release()

	img, err := decodeImage(src)
	if err != nil {
		return nil, "", err
	}
	bounds := img.Bounds()
	tw, th = scaleToBox(bounds.Dx(), bounds.Dy(), thumbMaxDim)
	dst := image.NewRGBA(image.Rect(0, 0, tw, th))
	resample(dst, img)

	switch srcMIME {
	case "image/png", "image/gif":
		out, err := encodePNG(dst)
		if err != nil {
			return nil, "", err
		}
		return out, "image/png", nil
	default:
		// image/jpeg, image/webp: JPEG out. WebP has no stdlib encoder.
		out, err := encodeJPEG(dst, thumbJPEGQuality)
		if err != nil {
			return nil, "", err
		}
		return out, "image/jpeg", nil
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
