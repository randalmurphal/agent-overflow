package harnessclient

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// tailChunk is how much of a file's end TailFile reads per attempt. Log
// lines here are short; 64 KiB usually holds far more than the requested
// count, and the loop doubles when it does not.
const tailChunk = 64 * 1024

// maxTailBytes bounds the doubling. A caller asking for more history
// than this wants the file, not a tail.
const maxTailBytes = 8 * 1024 * 1024

// TailFile returns the last n lines of a file. A file shorter than the
// window is returned whole; a missing file is an error, because "the
// evidence file is not there" is a finding, not an empty result.
func TailFile(path string, n int) ([]string, error) {
	if n <= 0 {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()

	for window := int64(tailChunk); ; window *= 2 {
		from := size - window
		if from < 0 {
			from = 0
		}
		if _, err := f.Seek(from, io.SeekStart); err != nil {
			return nil, err
		}
		lines, err := readLines(f)
		if err != nil {
			return nil, err
		}
		// A window that did not start at byte 0 may have cut its first
		// line in half; drop it rather than report a fragment.
		if from > 0 && len(lines) > 0 {
			lines = lines[1:]
		}
		if len(lines) >= n || from == 0 || window >= maxTailBytes {
			if len(lines) > n {
				lines = lines[len(lines)-n:]
			}
			return lines, nil
		}
	}
}

func readLines(r io.Reader) ([]string, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines, scanner.Err()
}

// FollowFile streams appended lines to emit until ctx ends, starting
// from the current end of the file. A file that does not exist yet is
// waited for rather than refused: `logs -f` on a fresh instance should
// start printing when the first line lands, not fail.
//
// Truncation (a rotated log) is detected by the file shrinking below the
// read offset; the follow restarts from the new beginning so a rotation
// costs at most a repeated line, never a silent stall.
func FollowFile(ctx context.Context, path string, emit func(string)) error {
	const pollInterval = 200 * time.Millisecond
	var offset int64
	if info, err := os.Stat(path); err == nil {
		offset = info.Size()
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", path, err)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(pollInterval):
		}

		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			offset = 0
			continue
		}
		if err != nil {
			return fmt.Errorf("stat %s: %w", path, err)
		}
		if info.Size() < offset {
			offset = 0
		}
		if info.Size() == offset {
			continue
		}
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open %s: %w", path, err)
		}
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			f.Close()
			return fmt.Errorf("seek %s: %w", path, err)
		}
		data, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		// Advance only past COMPLETE lines: a writer caught mid-append
		// leaves a fragment, and emitting it would print half a log line
		// now and the other half next tick.
		consumed := 0
		for {
			at := bytes.IndexByte(data[consumed:], '\n')
			if at < 0 {
				break
			}
			emit(strings.TrimSuffix(string(data[consumed:consumed+at]), "\r"))
			consumed += at + 1
		}
		offset += int64(consumed)
	}
}
