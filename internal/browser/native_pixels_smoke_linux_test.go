//go:build linux && cgo && !gtk3 && !android && !server && !nogui

package browser

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// westonScreenshot captures the run's weston output with
// weston-screenshooter, which the compositor allows under --debug.
func westonScreenshot(t *testing.T) (image.Image, []byte) {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command("weston-screenshooter")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("weston-screenshooter: %v: %s", err, out)
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.png"))
	if err != nil || len(files) != 1 {
		t.Fatalf("weston-screenshooter wrote %v (%v)", files, err)
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode the screenshot: %v", err)
	}
	return img, raw
}

func sameRGB(c color.Color, want color.RGBA) bool {
	r, g, b, _ := c.RGBA()
	return uint8(r>>8) == want.R && uint8(g>>8) == want.G && uint8(b>>8) == want.B
}

// boundsOf answers the smallest rectangle holding every pixel of want.
func boundsOf(img image.Image, want color.RGBA) image.Rectangle {
	var out image.Rectangle
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if sameRGB(img.At(x, y), want) {
				out = out.Union(image.Rect(x, y, x+1, y+1))
			}
		}
	}
	return out
}

// shareNear answers the share of img's pixels within a JPEG's error of want.
func shareNear(img image.Image, want color.RGBA) float64 {
	near := func(a uint32, b uint8) bool { return max(int(a>>8), int(b))-min(int(a>>8), int(b)) <= 8 }
	n, hit := 0, 0
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			n++
			r, g, bl, _ := img.At(x, y).RGBA()
			if near(r, want.R) && near(g, want.G) && near(bl, want.B) {
				hit++
			}
		}
	}
	if n == 0 {
		return 0
	}
	return float64(hit) / float64(n)
}

// shareOf answers the share of region's pixels that are want.
func shareOf(img image.Image, region image.Rectangle, want color.RGBA) float64 {
	n, hit := 0, 0
	for y := region.Min.Y; y < region.Max.Y; y++ {
		for x := region.Min.X; x < region.Max.X; x++ {
			n++
			if sameRGB(img.At(x, y), want) {
				hit++
			}
		}
	}
	if n == 0 {
		return 0
	}
	return float64(hit) / float64(n)
}
