package git

import (
	"crypto/sha256"
	"strconv"
	"strings"
)

// objectID is a git object id's bytes, zero-padded.
type objectID [sha256.Size]byte

// parseObjectID reads a hexadecimal object id.
func parseObjectID(hexID string) (objectID, bool) {
	var id objectID
	if hexID == "" || len(hexID)%2 != 0 || len(hexID) > 2*len(id) {
		return objectID{}, false
	}
	for i := 0; i < len(hexID); i += 2 {
		high, low := hexDigit(hexID[i]), hexDigit(hexID[i+1])
		if high < 0 || low < 0 {
			return objectID{}, false
		}
		id[i/2] = byte(high<<4 | low)
	}
	return id, true
}

func hexDigit(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	default:
		return -1
	}
}

// numstatRecord is one path of `git diff --numstat -z`. oldPath is set on
// a rename, whose counts belong to path.
type numstatRecord struct {
	insertions, deletions int
	path, oldPath         string
}

// numstatRecords parses `git diff --numstat -z` output: a record is
// "<insertions>\t<deletions>\t<path>\0", and a rename leaves the path
// empty and follows with "<old>\0<new>\0". A binary file's counts are "-"
// and read as zero.
func numstatRecords(stdout string) func(yield func(numstatRecord) bool) {
	return func(yield func(numstatRecord) bool) {
		rest := stdout
		for rest != "" {
			var head string
			head, rest, _ = strings.Cut(rest, "\x00")
			insertions, tail, ok := strings.Cut(head, "\t")
			deletions, path, ok2 := strings.Cut(tail, "\t")
			if !ok || !ok2 {
				continue
			}
			record := numstatRecord{insertions: numstatCount(insertions), deletions: numstatCount(deletions), path: path}
			if record.path == "" {
				record.oldPath, rest, _ = strings.Cut(rest, "\x00")
				record.path, rest, _ = strings.Cut(rest, "\x00")
			}
			if !yield(record) {
				return
			}
		}
	}
}

func numstatCount(raw string) int {
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	return value
}
