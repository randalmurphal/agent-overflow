//go:build windows

package webview2host

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// serve answers /cd/<name>?size=n as an attachment, /slow/<name> as one
// that holds until the smoke ends, /fail/<name> as one whose connection
// drops, and /page/<name> as a page with an anchor downloading <name>.
func (s *downloadSmoke) serve(release chan struct{}) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		kind, name, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		s.hitsMu <- struct{}{}
		s.hits[name]++
		s.ranges[name] = append(s.ranges[name], r.Header.Get("Range"))
		<-s.hitsMu
		switch kind {
		case "resume", "norange", "novalidator":
			s.serveDropped(w, r, kind, name)
			return
		case "pair", "twin":
			s.servePair(w, r, kind, name)
			return
		case "page":
			w.Header().Set("Content-Type", "text/html")
			_, _ = fmt.Fprintf(w, `<html><body><a id="a" href="/raw/x" download="%s">a</a></body></html>`, s.prefix+name)
			return
		case "raw":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(make([]byte, 2048))
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="`+s.prefix+name+`"`)
		switch kind {
		case "cd":
			size, _ := strconv.Atoi(r.URL.Query().Get("size"))
			w.Header().Set("Content-Length", strconv.Itoa(size))
			chunk := make([]byte, 1<<20)
			for size > 0 {
				n := min(size, len(chunk))
				if _, err := w.Write(chunk[:n]); err != nil {
					return
				}
				size -= n
			}
		case "slow":
			w.Header().Set("Content-Length", "1048576")
			_, _ = w.Write(make([]byte, 1024))
			w.(http.Flusher).Flush()
			select {
			case <-release:
			case <-r.Context().Done():
			}
		case "fail":
			w.Header().Set("Content-Length", "1048576")
			_, _ = w.Write(make([]byte, 4096))
			w.(http.Flusher).Flush()
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				_ = conn.Close()
			}
		}
	}
}

// droppedSize is the size of a /resume or /norange download.
const droppedSize = 1 << 20

// droppedByte is byte i of a /resume or /norange download, so a file put
// together from pieces shows whether they line up.
func droppedByte(i int) byte { return byte(i % 251) }

// serveDropped answers the first request for name with its headers and a
// part of its body, then drops the connection, as a server whose response
// is shorter than its Content-Length. A later request gets the rest: for
// "resume", a Range request is answered 206 from its offset; for "norange"
// and "novalidator", every later request gets the whole body with 200, and
// "novalidator" names no Last-Modified to resume against either.
func (s *downloadSmoke) serveDropped(w http.ResponseWriter, r *http.Request, kind, name string) {
	body := droppedHeaders(w, r, kind, s.prefix+name)
	if s.requests(name) == 1 {
		dropAfterPart(w, body)
		return
	}
	_, _ = w.Write(body)
}

// servePair answers the /pair and /twin downloads, each request reaching
// the server (no-store), with ranges. /pair: the first request holds after
// a part of its body until pairGo, the second drops after a part, and a
// later one, its resume, gets the rest. /twin: each odd request (a
// download) drops after a part, and each even one (its resume) sends a
// part, holds until twinGo, then sends the rest.
func (s *downloadSmoke) servePair(w http.ResponseWriter, r *http.Request, kind, name string) {
	w.Header().Set("Cache-Control", "no-store")
	body := droppedHeaders(w, r, "resume", s.prefix+name)
	n := s.requests(name)
	switch {
	case kind == "pair" && n == 1:
		_, _ = w.Write(body[:64<<10])
		w.(http.Flusher).Flush()
		select {
		case <-s.pairGo:
			_, _ = w.Write(body[64<<10:])
		case <-r.Context().Done():
		}
	case kind == "pair" && n == 2:
		dropAfterPart(w, body)
	case kind == "twin" && n%2 == 1:
		dropAfterPart(w, body)
	case kind == "twin":
		_, _ = w.Write(body[:16<<10])
		w.(http.Flusher).Flush()
		select {
		case <-s.twinGo:
			_, _ = w.Write(body[16<<10:])
		case <-r.Context().Done():
		}
	default:
		_, _ = w.Write(body)
	}
}

// dropAfterPart writes a part of body and drops the connection.
func dropAfterPart(w http.ResponseWriter, body []byte) {
	_, _ = w.Write(body[:64<<10])
	w.(http.Flusher).Flush()
	if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
		_ = conn.Close()
	}
}

// droppedHeaders writes the headers of a dropped download's response saved
// as file and answers the body to write: for "resume" with validators and
// ranges, a Range request answered from its offset.
func droppedHeaders(w http.ResponseWriter, r *http.Request, kind, file string) []byte {
	ranges := kind == "resume"
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+file+`"`)
	if kind != "novalidator" {
		w.Header().Set("Last-Modified", "Mon, 02 Jan 2006 15:04:05 GMT")
	}
	if ranges {
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("ETag", `"ao-smoke-1"`)
	}
	from := 0
	if spec, ok := strings.CutPrefix(r.Header.Get("Range"), "bytes="); ok && ranges {
		from, _ = strconv.Atoi(strings.TrimSuffix(spec, "-"))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, droppedSize-1, droppedSize))
		w.Header().Set("Content-Length", strconv.Itoa(droppedSize-from))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.Header().Set("Content-Length", strconv.Itoa(droppedSize))
	}
	body := make([]byte, droppedSize-from)
	for i := range body {
		body[i] = droppedByte(from + i)
	}
	return body
}

// rangesAsked answers the Range header of every request for name, "" for
// none.
func (s *downloadSmoke) rangesAsked(name string) []string {
	s.hitsMu <- struct{}{}
	defer func() { <-s.hitsMu }()
	return append([]string(nil), s.ranges[name]...)
}

func (s *downloadSmoke) requests(name string) int {
	s.hitsMu <- struct{}{}
	defer func() { <-s.hitsMu }()
	return s.hits[name]
}
