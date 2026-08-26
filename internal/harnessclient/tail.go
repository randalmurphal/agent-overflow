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

// followPollInterval is how often FollowFile looks for new bytes. A
// package var rather than a const so a test can shrink it: at 200ms every
// case would otherwise be built out of real sub-second sleeps, which is
// both slow and the kind of timing the repo's test discipline bans.
// Production never writes it.
var followPollInterval = 200 * time.Millisecond

// followReadCap bounds ONE poll's read. A backend mid-burst can append
// megabytes between two polls, and an uncapped read would pull all of it
// into memory in one allocation to print a screen's worth of lines. What
// does not fit is read by the next poll, one interval later.
const followReadCap = 1 << 20

// followMaxFragment bounds the carried partial line. Only a writer that
// never emits a newline can reach it, and something that long is not a
// log line; it is emitted as one rather than buffered without limit.
const followMaxFragment = maxTailBytes

// followStartedHook fires once FollowFile has taken its starting offset.
// Nil in production. A test installs one so it can append AFTER the
// follower has decided where the file ends — the alternative is sleeping
// and hoping the goroutine got there first.
var followStartedHook func()

// FollowFile streams appended lines to emit until ctx ends, starting
// from the current end of the file. A file that does not exist yet is
// waited for rather than refused: `logs -f` on a fresh instance should
// start printing when the first line lands, not fail.
//
// Rotation is detected two ways, because either one alone misses a real
// case. SIZE catches the file that SHRANK (an in-place truncate).
// IDENTITY (device+inode, empty where the platform has no cheap answer)
// catches the file that was REPLACED and then grew back past the old
// offset — to the size check that is indistinguishable from ordinary
// growth, so the follow would resume mid-record in a file it has never
// read and silently skip everything before it. Either signal restarts
// from the new beginning, so a rotation costs at most a repeated line.
//
// The read offset advances past everything READ, complete or not, and the
// trailing fragment is carried in memory until its newline arrives. The
// naive alternative — leave the offset before the fragment — re-reads and
// re-scans it on every poll until the writer finishes the line, which for
// a slow writer of a long line is the same bytes over and over.
func FollowFile(ctx context.Context, path string, emit func(string)) error {
	var offset int64
	var fragment []byte
	var ident string
	if info, err := os.Stat(path); err == nil {
		offset = info.Size()
		ident = FileIdentity(info)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if followStartedHook != nil {
		followStartedHook()
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(followPollInterval):
		}

		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			offset, fragment, ident = 0, nil, ""
			continue
		}
		if err != nil {
			return fmt.Errorf("stat %s: %w", path, err)
		}
		current := FileIdentity(info)
		replaced := ident != "" && current != "" && current != ident
		if replaced || info.Size() < offset {
			// Rotated, replaced or truncated: the fragment belonged to the
			// old file and its newline is never coming.
			offset, fragment = 0, nil
		}
		ident = current
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
		read, err := io.ReadAll(io.LimitReader(f, followReadCap))
		f.Close()
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		offset += int64(len(read))

		data := append(fragment, read...)
		consumed := 0
		for {
			at := bytes.IndexByte(data[consumed:], '\n')
			if at < 0 {
				break
			}
			emit(strings.TrimSuffix(string(data[consumed:consumed+at]), "\r"))
			consumed += at + 1
		}
		rest := data[consumed:]
		if len(rest) > followMaxFragment {
			emit(strings.TrimSuffix(string(rest), "\r"))
			rest = nil
		}
		// append onto its own prefix: the copy is forward-overlapping,
		// which is exactly what append's memmove handles, and it keeps the
		// one buffer rather than allocating a fresh one per poll.
		fragment = append(fragment[:0], rest...)
	}
}
