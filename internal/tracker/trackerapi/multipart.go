package trackerapi

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"sync"
	"sync/atomic"
)

// Part is one part of a multipart upload: a file when FileName is set,
// else a plain form field. Content holds exactly Size bytes.
//
// A Content read once (not an io.Seeker) that is an io.Closer is closed
// when the attempt sending it ends before reading it to its end: the
// call's context ended, the transfer stalled, the client closed, or the
// tracker answered early. Closing must end a read of it in progress,
// which net/http otherwise waits on before the attempt returns. A Content
// read to its end, which is its size, is never closed.
type Part struct {
	FieldName string
	FileName  string
	Content   io.Reader
	Size      int64
}

// Upload is a multipart/form-data request body, sent as a Request's Body
// with Transfer set. Its framing (each part's boundary and headers, and
// the closing boundary) is written once, by mime/multipart, when it is
// made; each part's content is read only as the body is sent, so an
// upload holds no content of its own, and its length, the framing's plus
// each part's Size, is declared as the request's Content-Length.
//
// A content that is an io.Seeker is read again from where it stood when
// the upload was made for every attempt of the call; any other is read
// once, so a call whose upload holds one is sent at most once: no resend
// after a refused credential or a rate limit.
type Upload struct {
	contentType string
	size        int64
	// framing[i] goes before parts[i]; the last is the closing boundary.
	framing [][]byte
	parts   []Part
	// starts are each seekable content's offset when the upload was made.
	starts []int64
	once   bool
	opened bool
}

// Multipart makes the upload of parts, in order. A part needs a field
// name, content and a size that is not negative.
func Multipart(parts []Part) (*Upload, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	u := &Upload{parts: parts, starts: make([]int64, len(parts))}
	for i, part := range parts {
		if part.FieldName == "" || part.Content == nil || part.Size < 0 {
			return nil, fmt.Errorf("trackerapi: upload part %d needs a field name, content and a size", i)
		}
		var err error
		if part.FileName != "" {
			_, err = w.CreateFormFile(part.FieldName, part.FileName)
		} else {
			_, err = w.CreateFormField(part.FieldName)
		}
		if err != nil {
			return nil, fmt.Errorf("trackerapi: upload part %d (%s): %w", i, part.FieldName, err)
		}
		if s, ok := part.Content.(io.Seeker); ok {
			if u.starts[i], err = s.Seek(0, io.SeekCurrent); err != nil {
				return nil, fmt.Errorf("trackerapi: upload part %d (%s): %w", i, part.FieldName, err)
			}
		} else {
			u.once = true
		}
		u.framing = append(u.framing, bytes.Clone(buf.Bytes()))
		u.size += int64(buf.Len()) + part.Size
		buf.Reset()
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("trackerapi: finish the upload body: %w", err)
	}
	u.framing = append(u.framing, bytes.Clone(buf.Bytes()))
	u.size += int64(buf.Len())
	u.contentType = w.FormDataContentType()
	return u, nil
}

// Size is the body's length in bytes, framing included.
func (u *Upload) Size() int64 { return u.size }

// open is the body one attempt sends. It refuses a second opening of an
// upload whose content is read once; a call holding one never asks.
func (u *Upload) open() (*uploadBody, error) {
	if u.opened && u.once {
		return nil, errors.New("an upload whose content is read once was sent already")
	}
	u.opened = true
	b := &uploadBody{}
	readers := make([]io.Reader, 0, 2*len(u.parts)+1)
	for i, part := range u.parts {
		if s, ok := part.Content.(io.Seeker); ok {
			if _, err := s.Seek(u.starts[i], io.SeekStart); err != nil {
				return nil, fmt.Errorf("rewind upload part %d (%s): %w", i, part.FieldName, err)
			}
		}
		e := &exactReader{r: part.Content, left: part.Size, part: i, body: b}
		if _, seeks := part.Content.(io.Seeker); !seeks {
			if closer, ok := part.Content.(io.Closer); ok {
				e.closer = closer
				b.closers = append(b.closers, e)
			}
		}
		readers = append(readers, bytes.NewReader(u.framing[i]), e)
	}
	b.r = io.MultiReader(append(readers, bytes.NewReader(u.framing[len(u.parts)]))...)
	return b, nil
}

// uploadBody is one attempt's body of an Upload. It records the first
// failure of a part's content, which hop reports as a *SourceError.
type uploadBody struct {
	r io.Reader
	// closers are the parts abort closes: read once, and io.Closers.
	closers []*exactReader

	mu     sync.Mutex
	failed error
}

func (b *uploadBody) Read(p []byte) (int, error) { return b.r.Read(p) }

// fail records err as the source's failure and returns it.
func (b *uploadBody) fail(err error) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failed == nil {
		b.failed = err
	}
	return b.failed
}

// abort closes each part's content that Part says it may and that was
// not read to its end, ending a read of it in progress. A close that
// fails is the source's failure. It runs when the attempt's context ends,
// on a goroutine of its own, so it may overlap a Read.
func (b *uploadBody) abort() {
	for _, e := range b.closers {
		if e.whole.Load() {
			continue
		}
		if err := e.closer.Close(); err != nil {
			_ = b.fail(fmt.Errorf("stop reading upload part %d: %w", e.part, err))
		}
	}
}

// sourceFailure is the content's failure, nil while every part read as
// declared. The transport reads the body on its own goroutine.
func (b *uploadBody) sourceFailure() error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.failed
}

// exactReader reads one part's content, which must hold exactly left
// bytes: one that ends early, runs past them or fails is the source's
// failure.
type exactReader struct {
	r    io.Reader
	left int64
	part int
	body *uploadBody
	// closer is r when abort may close it; whole is set once r ended at
	// its size, after which abort leaves it open.
	closer io.Closer
	whole  atomic.Bool
}

func (e *exactReader) Read(p []byte) (int, error) {
	if e.left == 0 {
		var probe [1]byte
		n, err := io.ReadFull(e.r, probe[:])
		switch {
		case n > 0:
			return 0, e.body.fail(fmt.Errorf("upload part %d holds more than its declared size", e.part))
		case errors.Is(err, io.EOF):
			e.whole.Store(true)
			return 0, io.EOF
		}
		return 0, e.body.fail(fmt.Errorf("upload part %d: %w", e.part, err))
	}
	if int64(len(p)) > e.left {
		p = p[:e.left]
	}
	n, err := e.r.Read(p)
	e.left -= int64(n)
	switch {
	case err == nil:
		return n, nil
	case errors.Is(err, io.EOF) && e.left == 0:
		// The content ended at its size; the next read sees the end.
		e.whole.Store(true)
		return n, nil
	case errors.Is(err, io.EOF):
		return n, e.body.fail(fmt.Errorf("upload part %d ended %d bytes short of its declared size", e.part, e.left))
	}
	return n, e.body.fail(fmt.Errorf("upload part %d: %w", e.part, err))
}
