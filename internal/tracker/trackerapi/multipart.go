package trackerapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
)

// ErrUploadTooLarge is a Multipart body over its limit.
var ErrUploadTooLarge = errors.New("trackerapi: upload is over its size limit")

// Part is one part of a multipart upload: a file when FileName is set,
// else a plain form field.
type Part struct {
	FieldName string
	FileName  string
	Content   io.Reader
}

// Multipart builds a multipart/form-data body from parts in order, reading
// each Content to its end, and returns the body and its content type, to
// be sent as a Request's Body and ContentType with Transfer set. A body
// that would pass limit bytes, boundaries included, fails with
// ErrUploadTooLarge before more than limit bytes are held. It stops
// between reads once ctx ends.
func Multipart(ctx context.Context, parts []Part, limit int64) (body []byte, contentType string, err error) {
	if limit <= 0 {
		return nil, "", errors.New("trackerapi: Multipart needs a positive limit")
	}
	buf := &cappedBuffer{limit: limit}
	w := multipart.NewWriter(buf)
	for i, part := range parts {
		if part.FieldName == "" || part.Content == nil {
			return nil, "", fmt.Errorf("trackerapi: upload part %d needs a field name and content", i)
		}
		var dst io.Writer
		if part.FileName != "" {
			dst, err = w.CreateFormFile(part.FieldName, part.FileName)
		} else {
			dst, err = w.CreateFormField(part.FieldName)
		}
		if err == nil {
			_, err = io.Copy(dst, contextReader{ctx: ctx, r: part.Content})
		}
		if err != nil {
			return nil, "", fmt.Errorf("trackerapi: upload part %d (%s): %w", i, part.FieldName, err)
		}
	}
	if err := w.Close(); err != nil {
		return nil, "", fmt.Errorf("trackerapi: finish the upload body: %w", err)
	}
	return buf.buf.Bytes(), w.FormDataContentType(), nil
}

// contextReader stops reading once ctx ends.
type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// cappedBuffer is a buffer that refuses a write past limit. It has no
// method but Write, so no io helper writes around the cap.
type cappedBuffer struct {
	buf   bytes.Buffer
	limit int64
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if int64(b.buf.Len())+int64(len(p)) > b.limit {
		return 0, ErrUploadTooLarge
	}
	return b.buf.Write(p)
}
