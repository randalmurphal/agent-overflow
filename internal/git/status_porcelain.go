package git

import "strings"

// statusOutputLimit caps the output of the status read and of the churn
// diff. Both are read whole: a record per changed path, so a workspace
// with tens of thousands of changed files passes the default cap.
const statusOutputLimit = 64 << 20

// statusEntry is one changed tracked path of `git status --porcelain=v2 -z`.
type statusEntry struct {
	// kind is the record type: '1' changed, '2' renamed or copied,
	// 'u' unmerged.
	kind byte
	// xy is the staged and unstaged state, each '.' when unchanged.
	xy string
	// submodule reports a gitlink entry.
	submodule bool
	// regular reports a path that holds a regular file in HEAD, in the
	// index and in the working tree: one modified in place, which no
	// rename can involve.
	regular bool
	// head and index are the path's blob ids in HEAD and in the index
	// ('1' and '2' records).
	head, index string
	path        string
	// origPath is the path a '2' record was renamed or copied from.
	origPath string
	// record is the record as git printed it, origPath excluded.
	record string
}

// porcelainStatus is the parsed `git status --porcelain=v2 --branch -z`.
type porcelainStatus struct {
	// branch is the checked-out branch, empty when HEAD is detached.
	branch string
	// oid is HEAD's commit id, "(initial)" before the first commit.
	oid         string
	detached    bool
	hasUpstream bool
	ahead       int
	behind      int
	// hasChanges reports any changed, untracked or ignored record.
	hasChanges bool
	// entries are the changed tracked paths, in git's path order.
	entries []statusEntry
}

// hasCommit reports whether HEAD names a commit.
func (s *porcelainStatus) hasCommit() bool {
	return s.oid != "" && !strings.HasPrefix(s.oid, "(")
}

// parsePorcelainStatus parses NUL-delimited porcelain v2 output. Paths
// are taken as the bytes between delimiters, so a path holding spaces,
// quotes or newlines is itself. A record git did not print whole is
// counted as a change and otherwise skipped.
func parsePorcelainStatus(stdout string) porcelainStatus {
	// One record per NUL, a rename's second path included.
	status := porcelainStatus{entries: make([]statusEntry, 0, strings.Count(stdout, "\x00"))}
	rest := stdout
	next := func() (string, bool) {
		if rest == "" {
			return "", false
		}
		record, tail, _ := strings.Cut(rest, "\x00")
		rest = tail
		return record, true
	}
	for {
		record, ok := next()
		if !ok {
			return status
		}
		if record == "" {
			continue
		}
		if header, isHeader := strings.CutPrefix(record, "# "); isHeader {
			status.applyHeader(header)
			continue
		}
		status.hasChanges = true
		var entry statusEntry
		var fields [11]string
		switch record[0] {
		case '1':
			if !cutFields(record, fields[:9]) {
				continue
			}
			submodule := strings.HasPrefix(fields[2], "S")
			entry = statusEntry{
				kind: '1', xy: fields[1], submodule: submodule,
				regular: !submodule && isRegularFileMode(fields[3]) && isRegularFileMode(fields[4]) && isRegularFileMode(fields[5]),
				head:    fields[6], index: fields[7], path: fields[8], record: record,
			}
		case '2':
			origPath, hasOrig := next()
			if !cutFields(record, fields[:10]) || !hasOrig {
				continue
			}
			entry = statusEntry{
				kind: '2', xy: fields[1], submodule: strings.HasPrefix(fields[2], "S"),
				head: fields[6], index: fields[7], path: fields[9], origPath: origPath, record: record,
			}
		case 'u':
			if !cutFields(record, fields[:11]) {
				continue
			}
			entry = statusEntry{
				kind: 'u', xy: fields[1], submodule: strings.HasPrefix(fields[2], "S"), path: fields[10], record: record,
			}
		default:
			// Untracked files are counted one by one in baseStatus,
			// since git collapses a wholly untracked directory into
			// one record.
			continue
		}
		if len(entry.xy) != 2 {
			continue
		}
		status.entries = append(status.entries, entry)
	}
}

func (s *porcelainStatus) applyHeader(header string) {
	key, value, _ := strings.Cut(header, " ")
	value = strings.TrimSpace(value)
	switch key {
	case "branch.oid":
		s.oid = value
	case "branch.head":
		// "(detached)" names no branch.
		if !strings.HasPrefix(value, "(") {
			s.branch = value
		}
		s.detached = value == "(detached)"
	case "branch.upstream":
		s.hasUpstream = value != ""
	case "branch.ab":
		s.ahead, s.behind = parseAheadBehind(value)
	}
}

func isRegularFileMode(mode string) bool {
	return mode == "100644" || mode == "100755"
}

// cutFields splits record at single spaces into len(fields) fields, the
// last taking the rest, and returns false when record has fewer.
func cutFields(record string, fields []string) bool {
	last := len(fields) - 1
	for i := 0; i < last; i++ {
		field, rest, ok := strings.Cut(record, " ")
		if !ok {
			return false
		}
		fields[i], record = field, rest
	}
	fields[last] = record
	return true
}
