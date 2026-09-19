package attachment

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"

	_ "golang.org/x/image/bmp"  // register bmp for image.DecodeConfig
	_ "golang.org/x/image/tiff" // register tiff for image.DecodeConfig
)

// DisplayImageMaxBytes caps a local image read for display in rendered
// markdown. Larger than DefaultMaxSize because these bytes are shown, not
// re-encoded into a model's context: a full-resolution screenshot of a 4K
// display is routinely over 10 MiB as PNG.
const DisplayImageMaxBytes int64 = 25 * 1024 * 1024

// DetectDisplayImageMIME sniffs the MIME type of an image that will be
// handed to a browser <img> for display. Wider than DetectImageMIME, whose
// set is what providers ingest: anything a browser paints is accepted here,
// which is the provider set plus bmp, ico, avif, tiff and svg.
//
// SVG is text, so its sniff is by structure (an <svg root after optional
// BOM, XML prolog, comments and doctype) rather than by magic bytes. In an
// <img> element the browser runs no script from it.
func DetectDisplayImageMIME(data []byte) (string, error) {
	if mime, err := DetectImageMIME(data); err == nil {
		return mime, nil
	}
	if len(data) >= 12 && string(data[4:8]) == "ftyp" {
		switch string(data[8:12]) {
		case "avif", "avis":
			return "image/avif", nil
		}
	}
	switch sniffed := http.DetectContentType(data); sniffed {
	case "image/bmp", "image/x-icon", "image/vnd.microsoft.icon", "image/tiff":
		return sniffed, nil
	}
	if looksLikeSVG(data) {
		return "image/svg+xml", nil
	}
	return "", errors.New("attachment: payload is not an image a browser displays")
}

// ValidateDisplayImage bounds the decode a browser will perform. Formats Go
// can read the header of get the pixel budget check; formats it cannot
// (svg, ico, avif) are accepted on the byte cap alone, which the caller has
// already applied.
func ValidateDisplayImage(data []byte, mime string) error {
	switch mime {
	case "image/svg+xml", "image/x-icon", "image/vnd.microsoft.icon", "image/avif":
		return nil
	}
	if err := ValidateImageDimensions(data); err != nil {
		return fmt.Errorf("%s: %w", mime, err)
	}
	return nil
}

func looksLikeSVG(data []byte) bool {
	head := data
	if len(head) > 4096 {
		head = head[:4096]
	}
	head = bytes.TrimPrefix(head, []byte{0xEF, 0xBB, 0xBF})
	for {
		head = bytes.TrimLeft(head, " \t\r\n")
		switch {
		case bytes.HasPrefix(head, []byte("<?")):
			end := bytes.Index(head, []byte("?>"))
			if end < 0 {
				return false
			}
			head = head[end+2:]
		case bytes.HasPrefix(head, []byte("<!--")):
			end := bytes.Index(head, []byte("-->"))
			if end < 0 {
				return false
			}
			head = head[end+3:]
		case bytes.HasPrefix(head, []byte("<!")):
			end := bytes.IndexByte(head, '>')
			if end < 0 {
				return false
			}
			head = head[end+1:]
		case bytes.HasPrefix(head, []byte("<svg")):
			return len(head) == 4 || bytes.IndexByte([]byte(" >\t\r\n"), head[4]) >= 0
		default:
			return false
		}
	}
}
