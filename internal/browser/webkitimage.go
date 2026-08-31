package browser

import (
	"fmt"
	"image"
)

// The WebKit driver's screenshot arithmetic. Tag-free and free of cgo for the
// same reason webkitjs.go is: the pixel handling is where a screenshot is
// silently wrong, and it should be tested on every platform rather than only
// where the engine happens to build.

// webkitDecodeSnapshot turns a texture's premultiplied BGRA into an
// *image.RGBA, which is exactly Go's premultiplied-alpha image type. The swap
// is in place and the buffer becomes the image's own storage, so a screenshot
// costs no second full-frame allocation.
func webkitDecodeSnapshot(pixels []byte, width, height int) (*image.RGBA, error) {
	if width <= 0 || height <= 0 || len(pixels) < width*height*4 {
		return nil, fmt.Errorf("browser: screenshot returned no pixels")
	}
	frame := pixels[:width*height*4]
	for i := 0; i < len(frame); i += 4 {
		frame[i], frame[i+2] = frame[i+2], frame[i]
	}
	return &image.RGBA{Pix: frame, Stride: width * 4, Rect: image.Rect(0, 0, width, height)}, nil
}

// webkitCrop narrows a frame to a rect, clamped to what exists. A rect wholly
// outside the frame keeps the frame: a clip past the end of a short document
// should answer with the document, not with nothing.
func webkitCrop(frame *image.RGBA, x, y, width, height int) *image.RGBA {
	rect := image.Rect(x, y, x+width, y+height).Intersect(frame.Rect)
	if rect.Empty() {
		return frame
	}
	return frame.SubImage(rect).(*image.RGBA)
}
