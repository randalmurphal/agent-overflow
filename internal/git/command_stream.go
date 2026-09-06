package git

import (
	"context"
	"errors"
	"io"
)

// os/exec owns one copying goroutine and joins it before runSpec reads err.
// A failed destination/cap cancels the child immediately instead of draining a
// multi-gigabyte pack that the transfer can no longer use. Stderr retains the
// ordinary small diagnostic buffer, independently of the object-stream cap.
type commandStreamWriter struct {
	writer    io.Writer
	remaining int64
	cancel    context.CancelFunc
	err       error
}

func (w *commandStreamWriter) Write(data []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if int64(len(data)) > w.remaining {
		w.err = errors.New("output exceeded the transfer size limit")
		w.cancel()
		return 0, w.err
	}
	n, err := w.writer.Write(data)
	w.remaining -= int64(n)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		w.err = err
		w.cancel()
	}
	return n, err
}
