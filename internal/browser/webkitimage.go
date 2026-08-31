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

// wkDocumentSizeScript reads the scrollable extent a full-document capture has
// to grow a WKWebView to. WKSnapshotConfiguration cannot reach past the view's
// own bounds — unlike WebKitGTK's FULL_DOCUMENT snapshot region — so on macOS
// the whole document is captured by briefly resizing the view to fit it.
const wkDocumentSizeScript = `const d=document.documentElement,b=document.body;return {width:Math.max(d.scrollWidth,b?b.scrollWidth:0,d.clientWidth),height:Math.max(d.scrollHeight,b?b.scrollHeight:0,d.clientHeight)};`

// captureSize is one bounded capture size in CSS pixels.
type captureSize struct{ width, height int }

// clampDocumentCapture bounds the size a full-document capture resizes a view
// to. A page can report a scroll extent of any size, and the frame is cropped
// to the same maximum afterwards anyway — so growing past it only buys a larger
// allocation and a longer render. Never smaller than the view: shrinking to
// capture would reflow the page being captured.
func clampDocumentCapture(documentWidth, documentHeight, viewWidth, viewHeight int) captureSize {
	size := captureSize{width: documentWidth, height: documentHeight}
	if size.width < viewWidth {
		size.width = viewWidth
	}
	if size.height < viewHeight {
		size.height = viewHeight
	}
	if size.width > maxFullScreenshotWidth {
		size.width = maxFullScreenshotWidth
	}
	if size.height > maxFullScreenshotHeight {
		size.height = maxFullScreenshotHeight
	}
	return size
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
