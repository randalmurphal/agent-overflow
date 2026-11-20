package git

import (
	"bytes"
	"errors"
	"io"

	"agent-overflow/internal/attachment"
	"agent-overflow/internal/gitdiff"
)

var (
	// ErrNotDisplayImage marks bytes that are not an image a browser
	// displays.
	ErrNotDisplayImage = errors.New("not an image a browser displays")
	// ErrLFSPointer marks bytes that are a Git LFS pointer.
	ErrLFSPointer = errors.New("a Git LFS pointer")
)

// DisplayImageMIME answers the MIME type of a blob's bytes a browser
// displays as an image, ErrLFSPointer for a Git LFS pointer,
// ErrNotDisplayImage otherwise. The review pane's image reads and the
// image facts classify a blob with it, so both agree on what is shown.
func DisplayImageMIME(buf []byte) (string, error) {
	mime, _, err := classifyDisplayImage(buf, bytes.NewReader(buf))
	return mime, err
}

// classifyDisplayImage classifies a blob from head, its first bytes (the
// whole blob when it is shorter than gitdiff.MaxLFSPointerBytes, and at
// least what the MIME sniff reads), and blob, a reader of the whole blob
// from its first byte, which is read only as far as the image header:
// the MIME type and how an <img> of it is sized (an svg's from head,
// attachment.SVGDisplaySize; an avif's from its meta box,
// attachment.AVIFSize; unknown for ico).
func classifyDisplayImage(head []byte, blob io.Reader) (mime string, size attachment.DisplaySize, err error) {
	if gitdiff.IsLFSPointer(head) {
		return "", attachment.DisplaySize{}, ErrLFSPointer
	}
	mime, err = attachment.DetectDisplayImageMIME(head)
	if err != nil {
		return "", attachment.DisplaySize{}, ErrNotDisplayImage
	}
	switch mime {
	case "image/svg+xml":
		return mime, attachment.SVGDisplaySize(head), nil
	case "image/avif":
		return mime, attachment.AVIFSize(blob), nil
	case "image/x-icon", "image/vnd.microsoft.icon":
		return mime, attachment.DisplaySize{}, nil
	}
	width, height, err := attachment.ImageDimensionsFrom(blob)
	if err != nil {
		return "", attachment.DisplaySize{}, ErrNotDisplayImage
	}
	return mime, attachment.DisplaySize{Width: width, Height: height}, nil
}
