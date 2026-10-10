package attachment

import (
	"fmt"
	"image"
	"strconv"

	"golang.org/x/sync/singleflight"
)

// DeriveWidths is the ladder of display widths a derivative is made at. A
// request rounds UP to the next tier, so a handful of widths serve every pane
// and device pixel ratio and each one is derived once. The frontend mirrors
// this list.
var DeriveWidths = []int{320, 480, 720, 1080, 1440, 2160, 2880, 3840, 5120}

// deriveJPEGQuality is the quality a derivative of a lossy source is
// re-encoded at. High because the derivative is what the timeline shows at
// full size, not a preview tile.
const deriveJPEGQuality = 90

// deriveMaxPixels caps a derivative's pixel count, about 2508x2508. Above it
// the source is served and the browser downsamples it. The cap is what
// keeps the largest derivation inside the decode memory budget: the
// resampler's scratch grows with tier x source height, and this cap with the
// source pixel budget bounds that product (imagecodec.go has the numbers).
// It admits a 16:9 derivative up to the 2880 tier and a 4:3 one at 2880x2160.
const deriveMaxPixels = 6 << 20

// deriveGroup dedupes concurrent derivations of the same source and tier, so
// two panes asking for one width decode once.
var deriveGroup singleflight.Group

// Derived is what Derive answers. Derived is false when Data is the source
// itself, unchanged.
type Derived struct {
	Data     []byte
	MimeType string
	// Width and Height are the served pixel size; zero when unknown (svg,
	// ico, avif).
	Width   int
	Height  int
	Derived bool
}

// DeriveTier is the ladder width that serves a display of maxWidth device
// pixels: the smallest tier at least maxWidth. Zero means the original, for
// a non-positive maxWidth or one above the ladder.
func DeriveTier(maxWidth int) int {
	if maxWidth <= 0 {
		return 0
	}
	for _, width := range DeriveWidths {
		if width >= maxWidth {
			return width
		}
	}
	return 0
}

// Derive answers a display-density version of one image at the tier that
// serves maxWidth (DeriveTier), or the source itself when no derivative is
// smaller or one would lose something:
//
//   - svg, ico and avif, which Go cannot decode;
//   - an animated GIF, whose derivative would silently drop the animation;
//   - a source no wider than the tier;
//   - a derivative over deriveMaxPixels;
//   - a source whose header reads but whose pixel data Go cannot decode,
//     which a browser's decoder may still show.
//
// A derivative is exactly the tier wide with the height rounded to keep the
// aspect ratio. PNG, GIF, BMP and TIFF sources derive to PNG (lossless, so
// a screenshot's text stays sharp); JPEG to JPEG; WebP to JPEG when it
// decoded as lossy YCbCr and to PNG when it was lossless or carries alpha.
//
// key names the source for deduplication: concurrent calls with the same key
// and tier share one derivation, so it must change whenever the bytes do.
// The header is checked against the pixel budget before any decode, and the
// decode runs under the shared memory budget (imagecodec.go).
func Derive(key string, src []byte, mime string, maxWidth int) (Derived, error) {
	original := Derived{Data: src, MimeType: mime}
	if !decodableDisplayMIME(mime) {
		return original, nil
	}
	cfg, err := imageConfig(src)
	if err != nil {
		return Derived{}, fmt.Errorf("%s: %w", mime, err)
	}
	original.Width, original.Height = cfg.Width, cfg.Height
	tier := DeriveTier(maxWidth)
	if tier == 0 || cfg.Width <= tier || !withinDerivedPixels(cfg.Width, cfg.Height, tier) {
		return original, nil
	}
	if mime == "image/gif" && gifIsAnimated(src) {
		return original, nil
	}
	value, err, _ := deriveGroup.Do(key+"\x00"+strconv.Itoa(tier), func() (any, error) {
		return deriveAt(src, mime, cfg, tier)
	})
	if err != nil {
		return Derived{}, fmt.Errorf("%s: derive %dpx: %w", mime, tier, err)
	}
	derived := value.(Derived)
	if !derived.Derived {
		return original, nil
	}
	return derived, nil
}

// withinDerivedPixels reports whether the tier's derivative of a width x
// height source stays within deriveMaxPixels.
func withinDerivedPixels(width, height, tier int) bool {
	return int64(tier)*int64(scaledHeight(width, height, tier)) <= deriveMaxPixels
}

func decodableDisplayMIME(mime string) bool {
	switch mime {
	case "image/png", "image/jpeg", "image/gif", "image/webp", "image/bmp", "image/tiff":
		return true
	}
	return false
}

func deriveAt(src []byte, mime string, cfg image.Config, tier int) (Derived, error) {
	release, err := acquireDecodeMemory(resampleCost(cfg, tier, scaledHeight(cfg.Width, cfg.Height, tier)))
	if err != nil {
		return Derived{}, err
	}
	defer release()

	img, err := decodeImage(src)
	if err != nil {
		// The header passed, so the bytes reach the browser either way;
		// its decoder may show what Go's could not.
		return Derived{}, nil
	}
	bounds := img.Bounds()
	// A GIF's first frame can be narrower than its logical screen, which
	// is what the header reported.
	if bounds.Dx() <= tier {
		return Derived{}, nil
	}
	width, height := tier, scaledHeight(bounds.Dx(), bounds.Dy(), tier)
	// NRGBA rather than RGBA: the PNG encoder writes unpremultiplied
	// alpha, and scaling into an 8-bit premultiplied buffer first would
	// lose the low-alpha precision the conversion back needs.
	dst := image.NewNRGBA(image.Rect(0, 0, width, height))
	resample(dst, img)

	data, outMIME, err := encodeDerivative(dst, mime, img)
	if err != nil {
		return Derived{}, err
	}
	return Derived{Data: data, MimeType: outMIME, Width: width, Height: height, Derived: true}, nil
}

// encodeDerivative picks the output format from the source's: lossy in,
// lossy out; everything else lossless.
func encodeDerivative(dst image.Image, srcMIME string, decoded image.Image) ([]byte, string, error) {
	lossy := srcMIME == "image/jpeg"
	if srcMIME == "image/webp" {
		// Lossy WebP decodes to YCbCr, or NYCbCrA when it carries alpha;
		// lossless decodes to NRGBA. JPEG cannot carry alpha.
		_, lossy = decoded.(*image.YCbCr)
	}
	if lossy {
		data, err := encodeJPEG(dst, deriveJPEGQuality)
		return data, "image/jpeg", err
	}
	data, err := encodePNG(dst)
	return data, "image/png", err
}

// scaledHeight keeps the aspect ratio at the given width, rounded to the
// nearest pixel and never below one.
func scaledHeight(width, height, toWidth int) int {
	scaled := (int64(height)*int64(toWidth) + int64(width)/2) / int64(width)
	if scaled < 1 {
		return 1
	}
	return int(scaled)
}

// gifIsAnimated reports whether a GIF holds more than one frame by walking
// its block structure. It never decompresses a frame: gif.DecodeAll would
// expand every frame, and the pixel budget bounds one frame, not how many
// there are, so a small file of near-uniform frames could expand to tens of
// gigabytes. A structure the walk cannot follow stops it and reads as one
// frame, which is what gif.Decode would show.
func gifIsAnimated(src []byte) bool {
	const (
		header              = 13 // signature, version, logical screen descriptor
		extensionIntroducer = 0x21
		imageSeparator      = 0x2C
	)
	if len(src) < header {
		return false
	}
	pos := header
	if flags := src[10]; flags&0x80 != 0 {
		pos += 3 << ((flags & 0x07) + 1)
	}
	frames := 0
	for pos < len(src) {
		switch src[pos] {
		case extensionIntroducer:
			pos = skipGIFSubBlocks(src, pos+2)
		case imageSeparator:
			frames++
			if frames > 1 {
				return true
			}
			const descriptor = 10
			if pos+descriptor > len(src) {
				return false
			}
			flags := src[pos+9]
			pos += descriptor
			if flags&0x80 != 0 {
				pos += 3 << ((flags & 0x07) + 1)
			}
			// The LZW minimum code size precedes the data sub-blocks.
			pos = skipGIFSubBlocks(src, pos+1)
		default:
			return false
		}
	}
	return false
}

// skipGIFSubBlocks returns the position after a run of length-prefixed
// sub-blocks and its zero terminator, or len(src) when it runs off the end.
func skipGIFSubBlocks(src []byte, pos int) int {
	for pos < len(src) {
		size := int(src[pos])
		pos++
		if size == 0 {
			return pos
		}
		pos += size
	}
	return len(src)
}
