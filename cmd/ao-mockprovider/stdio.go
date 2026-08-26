package main

import (
	"bufio"
	"bytes"
	"io"
	"log"
	"os"
	"sync"
	"time"
)

// lineWriter serializes all stdout writes. A single mutex spans an
// entire line — including its chunked partial writes — so control
// "emit" commands and concurrent protocol replies can never interleave
// bytes mid-line with scenario output.
type lineWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func newLineWriter(w io.Writer) *lineWriter {
	return &lineWriter{w: w}
}

// writeLine writes line + "\n". chunkBytes > 0 splits the bytes
// (newline included, so the final chunk always ends the line) into
// flushed writes of that size, chunkIntervalMs apart — the partial
// NDJSON writes that shake out reassembly bugs in the app's readers.
func (lw *lineWriter) writeLine(line string, chunkBytes, chunkIntervalMs int) {
	data := append([]byte(line), '\n')
	lw.mu.Lock()
	defer lw.mu.Unlock()

	if chunkBytes <= 0 || chunkBytes >= len(data) {
		lw.write(data)
		return
	}
	for off := 0; off < len(data); off += chunkBytes {
		end := min(off+chunkBytes, len(data))
		lw.write(data[off:end])
		if end < len(data) && chunkIntervalMs > 0 {
			time.Sleep(time.Duration(chunkIntervalMs) * time.Millisecond)
		}
	}
}

// writeLines writes every line, each newline-terminated, in ONE write
// under the same lock writeLine takes — several NDJSON frames landing in
// a single read on the app's side.
//
// This is the shape a real node CLI produces under load (its stdout
// stream flushes a batch), and the one the per-line default can never
// produce. An app reader that only ever sees one frame per read is
// untested against it.
func (lw *lineWriter) writeLines(lines []string) {
	if len(lines) == 0 {
		return
	}
	total := 0
	for _, line := range lines {
		total += len(line) + 1
	}
	data := make([]byte, 0, total)
	for _, line := range lines {
		data = append(data, line...)
		data = append(data, '\n')
	}
	lw.mu.Lock()
	defer lw.mu.Unlock()
	lw.write(data)
}

func (lw *lineWriter) write(data []byte) {
	if _, err := lw.w.Write(data); err != nil {
		// Stdout gone means the app-side reader is gone; nothing useful
		// can happen after this. Fail loudly and promptly.
		log.Fatalf("stdout write: %v", err)
	}
}

// forEachStdinLine feeds every non-blank stdin line to fn and returns
// on EOF (or read error, which it logs). bufio.Reader instead of
// Scanner so arbitrarily long lines (base64 attachments) can't abort
// the loop.
func forEachStdinLine(fn func(line []byte)) {
	r := bufio.NewReaderSize(os.Stdin, 64*1024)
	for {
		line, err := r.ReadBytes('\n')
		line = bytes.TrimRight(line, "\r\n")
		if len(bytes.TrimSpace(line)) > 0 {
			fn(line)
		}
		if err != nil {
			if err != io.EOF {
				log.Printf("stdin read: %v", err)
			}
			return
		}
	}
}
