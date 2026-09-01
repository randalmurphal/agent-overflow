package harnessrpc

import (
	"bytes"
	"testing"
)

func TestHarnessBrowserDiagnosticsUseProductionHost(t *testing.T) {
	h, host := newHarnessTestHost(t)

	var gotThread, gotPage string
	var gotX, gotY float64
	host.browserScroll = func(threadID, pageID string, x, y float64) error {
		gotThread, gotPage, gotX, gotY = threadID, pageID, x, y
		return nil
	}
	wantJPEG := []byte{0xff, 0xd8, 0xff, 0xd9}
	host.browserScreenshot = func(threadID, pageID string) ([]byte, error) {
		gotThread, gotPage = threadID, pageID
		return wantJPEG, nil
	}

	if err := h.HarnessBrowserScroll("thread-1", "page-1", 12, 900); err != nil {
		t.Fatalf("HarnessBrowserScroll: %v", err)
	}
	if gotThread != "thread-1" || gotPage != "page-1" || gotX != 12 || gotY != 900 {
		t.Fatalf("scroll forwarded (%q, %q, %v, %v)", gotThread, gotPage, gotX, gotY)
	}
	gotJPEG, err := h.HarnessBrowserScreenshot("thread-2", "page-2")
	if err != nil {
		t.Fatalf("HarnessBrowserScreenshot: %v", err)
	}
	if gotThread != "thread-2" || gotPage != "page-2" || !bytes.Equal(gotJPEG, wantJPEG) {
		t.Fatalf("screenshot forwarded (%q, %q, %x)", gotThread, gotPage, gotJPEG)
	}
}

func TestHarnessBrowserDiagnosticsRequireHost(t *testing.T) {
	h := New(Config{})
	if err := h.HarnessBrowserScroll("thread", "page", 0, 1); err == nil {
		t.Fatal("HarnessBrowserScroll succeeded without a host")
	}
	if _, err := h.HarnessBrowserScreenshot("thread", "page"); err == nil {
		t.Fatal("HarnessBrowserScreenshot succeeded without a host")
	}
}
