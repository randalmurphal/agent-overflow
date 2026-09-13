package remotejobs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/store"
	"github.com/shirou/gopsutil/v4/disk"
)

const (
	DefaultMaxJobBytes      int64 = 4 << 30
	DefaultMaxRetainedBytes int64 = 20 << 30
	MaxLogReadBytes               = 128 << 10
	logHeaderBytes                = 48
)

// Options limits disk use independently of the bounded reply and SQLite tail.
// An omitted LogDir creates disposable storage, for isolated test managers.
// Production always supplies a private directory beneath its durable data root.
type Options struct {
	LogDir           string
	MaxJobBytes      int64
	MaxRetainedBytes int64
	MinFreeBytes     uint64
	// OwnerGrace overrides DefaultOwnerGrace; tests shorten it.
	OwnerGrace time.Duration
	freeBytes  func(string) (uint64, error)
}

type LogInfo struct {
	RequestID     string `json:"requestId"`
	TotalBytes    int64  `json:"totalBytes"`
	StartOffset   int64  `json:"startOffset"`
	RetainedBytes int64  `json:"retainedBytes"`
	Truncated     bool   `json:"truncated"`
	Expired       bool   `json:"expired"`
	Error         string `json:"error,omitempty"`
}

type LogChunk struct {
	LogInfo
	Offset     int64  `json:"offset"`
	NextOffset int64  `json:"nextOffset"`
	Text       string `json:"text"`
}

type LogSearch struct {
	LogInfo
	Matches    []LogMatch `json:"matches"`
	NextOffset int64      `json:"nextOffset"`
	Done       bool       `json:"done"`
}

type LogMatch struct {
	Offset int64  `json:"offset"`
	Text   string `json:"text"`
}

type logStore struct {
	options   Options
	temporary bool
	mu        sync.Mutex
	active    map[string]*jobLog
}

type jobLog struct {
	mu              sync.Mutex
	file            *os.File
	capacity        int64
	total, retained int64
	lost            bool
	dirty           bool
	checked         time.Time
	checkedBytes    int64
	writable        bool
	options         Options
}

func newLogStore(o Options) (*logStore, error) {
	if o.MaxJobBytes == 0 {
		o.MaxJobBytes = DefaultMaxJobBytes
	}
	if o.MaxRetainedBytes == 0 {
		o.MaxRetainedBytes = DefaultMaxRetainedBytes
	}
	if o.MinFreeBytes == 0 {
		o.MinFreeBytes = 256 << 20
	}
	if o.MaxJobBytes < 1 || o.MaxJobBytes > DefaultMaxJobBytes || o.MaxRetainedBytes < o.MaxJobBytes {
		return nil, errors.New("remote logs: invalid capacity")
	}
	if o.freeBytes == nil {
		o.freeBytes = func(path string) (uint64, error) {
			stat, err := disk.Usage(path)
			if err != nil {
				return 0, err
			}
			return stat.Free, nil
		}
	}
	temporary := o.LogDir == ""
	var err error
	if temporary {
		o.LogDir, err = os.MkdirTemp("", "ao-remote-logs-")
	} else {
		err = os.MkdirAll(o.LogDir, 0700)
	}
	if err != nil {
		return nil, err
	}
	// A crashed interpreter must not leave its potentially sensitive script
	// indefinitely. Only this directory's fixed script prefix is disposable.
	entries, err := os.ReadDir(o.LogDir)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "script-") {
			if err := os.Remove(filepath.Join(o.LogDir, entry.Name())); err != nil {
				return nil, err
			}
		}
	}
	return &logStore{options: o, temporary: temporary, active: make(map[string]*jobLog)}, nil
}

// Reserve the full capacity of every active writer before admission. Completed
// logs expire oldest-first; no active log is ever removed to admit another job.
func (s *logStore) create(id string) (*jobLog, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Start calls create only after SQLite proved this ID unaccepted. A log
	// left by a crash between file creation and acceptance is safe to replace.
	if err := os.Remove(s.path(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	entries, err := os.ReadDir(s.options.LogDir)
	if err != nil {
		return nil, err
	}
	type candidate struct {
		path string
		size int64
		when time.Time
	}
	var files []candidate
	budget := int64(len(s.active)+1) * (s.options.MaxJobBytes + logHeaderBytes)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".log") || !entityid.Valid(strings.TrimSuffix(name, ".log")) {
			continue
		}
		if _, active := s.active[strings.TrimSuffix(name, ".log")]; active {
			continue
		}
		info, e := entry.Info()
		if e != nil {
			return nil, e
		}
		if !info.Mode().IsRegular() {
			continue
		}
		files = append(files, candidate{filepath.Join(s.options.LogDir, name), info.Size(), info.ModTime()})
		budget += info.Size()
	}
	limit := s.options.MaxRetainedBytes + int64(len(s.active)+1)*logHeaderBytes
	sort.Slice(files, func(i, j int) bool { return files[i].when.Before(files[j].when) })
	for _, old := range files {
		if budget <= limit {
			break
		}
		if err := os.Remove(old.path); err != nil {
			return nil, err
		}
		budget -= old.size
	}
	if budget > limit {
		return nil, errors.New("remote logs: active log reservations exceed retained capacity")
	}
	free, err := s.options.freeBytes(s.options.LogDir)
	if err != nil {
		return nil, err
	}
	if free < s.options.MinFreeBytes {
		return nil, errors.New("remote logs: insufficient free disk space")
	}
	file, err := os.OpenFile(s.path(id), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	writer := &jobLog{file: file, capacity: s.options.MaxJobBytes, writable: true, options: s.options, checked: time.Now()}
	if err = writer.saveHeader(); err != nil {
		file.Close()
		os.Remove(file.Name())
		return nil, err
	}
	s.active[id] = writer
	return writer, nil
}

func (l *jobLog) saveHeader() error {
	var header [logHeaderBytes]byte
	copy(header[:8], "AOLOG001")
	binary.LittleEndian.PutUint64(header[8:16], uint64(l.capacity))
	binary.LittleEndian.PutUint64(header[16:24], uint64(l.total))
	binary.LittleEndian.PutUint64(header[24:32], uint64(l.retained))
	if l.lost {
		header[32] = 1
	}
	if l.dirty {
		header[33] = 1
	}
	_, err := l.file.WriteAt(header[:], 0)
	return err
}

// Write always drains the child, including disk-full and permission failures.
// The file is a fixed-capacity byte ring; memory does not grow with output.
func (l *jobLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	count := len(p)
	if time.Since(l.checked) >= 2*time.Second || l.total-l.checkedBytes >= 1<<20 {
		free, err := l.options.freeBytes(l.options.LogDir)
		l.writable = err == nil && free >= l.options.MinFreeBytes
		l.checked = time.Now()
		l.checkedBytes = l.total
	}
	start := l.total
	l.total += int64(count)
	if !l.writable {
		l.lost = true
		l.retained = 0
		_ = l.saveHeader()
		return count, nil
	}
	if int64(len(p)) > l.capacity {
		p = p[len(p)-int(l.capacity):]
		start = l.total - int64(len(p))
	}
	// Mark an in-progress overwrite before changing ring contents. A process
	// crash in this window makes the log explicitly unreadable rather than
	// presenting overwritten bytes at the old offsets after recovery.
	l.dirty = true
	err := l.saveHeader()
	pos := start % l.capacity
	first := min(int64(len(p)), l.capacity-pos)
	if err == nil {
		_, err = l.file.WriteAt(p[:first], logHeaderBytes+pos)
	}
	if err == nil && first < int64(len(p)) {
		_, err = l.file.WriteAt(p[first:], logHeaderBytes)
	}
	if err == nil {
		l.retained = min(l.capacity, l.retained+int64(len(p)))
		l.dirty = false
		err = l.saveHeader()
	}
	if err != nil {
		log.Printf("remote log write failed: %v", err)
		l.writable = false
		l.checked = time.Now()
		l.lost = true
		l.retained = 0
		l.dirty = false
		_ = l.saveHeader()
	}
	return count, nil
}

func (s *logStore) finish(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if writer := s.active[id]; writer != nil {
		writer.mu.Lock()
		// Final metadata write also stamps completion recency for retention,
		// including quiet jobs whose last output was much earlier.
		err := writer.saveHeader()
		if err == nil {
			err = writer.file.Sync()
		}
		if err != nil {
			log.Printf("remote log sync failed: %v", err)
			writer.lost = true
			_ = writer.saveHeader()
		}
		writer.file.Close()
		writer.mu.Unlock()
		delete(s.active, id)
	}
}

func (l *jobLog) info(id string) LogInfo {
	info := LogInfo{RequestID: id, TotalBytes: l.total, StartOffset: l.total - l.retained, RetainedBytes: l.retained, Truncated: l.total > l.retained || l.lost}
	if l.lost {
		info.Error = "Some command output could not be saved because destination log storage was unavailable. The command continued; the retained range is shown."
	}
	return info
}

// withLog holds the active writer during reads; settled files are immutable.
// Holding the store lock also keeps retention from removing a file mid-read on
// Windows, where unlinking an open file is not portable.
func (s *logStore) withLog(id string, fn func(*jobLog) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if writer := s.active[id]; writer != nil {
		writer.mu.Lock()
		defer writer.mu.Unlock()
		return fn(writer)
	}
	file, err := os.Open(s.path(id))
	if err != nil {
		return err
	}
	defer file.Close()
	l, err := readSettledHeader(file)
	if err != nil {
		return err
	}
	return fn(l)
}

func (s *logStore) path(id string) string { return filepath.Join(s.options.LogDir, id+".log") }

// logReadError leaves a public verdict alone; only an unclassified I/O failure
// earns the retry advice.
func logReadError(err error) error {
	if _, _, public := errorsx.PublicDetails(err); public {
		return err
	}
	return errorsx.Public("remote_log_unavailable", "The destination could not read this job's log. Retry after checking destination storage; do not rerun the command to recover output.", err)
}

func (m *Manager) Log(ownerID, id string) (LogInfo, error) {
	if _, err := m.Get(ownerID, id); err != nil {
		return LogInfo{}, err
	}
	var info LogInfo
	err := m.logs.withLog(id, func(l *jobLog) error { info = l.info(id); return nil })
	if errors.Is(err, os.ErrNotExist) {
		return LogInfo{RequestID: id, Expired: true}, nil
	}
	if err != nil {
		return LogInfo{}, logReadError(err)
	}
	return info, nil
}

func validateRead(maxBytes int) error {
	if maxBytes < 1 || maxBytes > MaxLogReadBytes {
		return errorsx.Public("remote_invalid_request", fmt.Sprintf("max_bytes must be between 1 and %d.", MaxLogReadBytes), nil)
	}
	return nil
}

// ReadLog uses absolute byte offsets. A negative offset reads the newest tail;
// an expired prefix advances to StartOffset, explicitly returned to the caller.
func (m *Manager) ReadLog(ownerID, id string, offset int64, maxBytes int) (LogChunk, error) {
	if _, err := m.Get(ownerID, id); err != nil {
		return LogChunk{}, err
	}
	if err := validateRead(maxBytes); err != nil {
		return LogChunk{}, err
	}
	var result LogChunk
	err := m.logs.withLog(id, func(l *jobLog) error {
		result.LogInfo = l.info(id)
		if offset < 0 {
			offset = l.total - int64(maxBytes)
		}
		offset = max(result.StartOffset, min(offset, l.total))
		result.Offset = offset
		size := min(int64(maxBytes), l.total-offset)
		data := make([]byte, size)
		if size > 0 {
			pos := offset % l.capacity
			first := min(size, l.capacity-pos)
			if _, err := l.file.ReadAt(data[:first], logHeaderBytes+pos); err != nil {
				return err
			}
			if first < size {
				if _, err := l.file.ReadAt(data[first:], logHeaderBytes); err != nil {
					return err
				}
			}
		}
		result.Text = string(data)
		result.NextOffset = offset + size
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return LogChunk{LogInfo: LogInfo{RequestID: id, Expired: true}}, nil
	}
	if err != nil {
		return LogChunk{}, logReadError(err)
	}
	return result, nil
}

// SearchLog scans at most maxBytes per call and returns bounded contexts. It is
// a literal byte search (not a regex engine over untrusted gigabyte outputs).
// NextOffset preserves overlap so a match spanning pages is found next time.
func (m *Manager) SearchLog(ownerID, id, query string, offset int64, maxBytes int) (LogSearch, error) {
	if len(query) == 0 || len(query) > 4096 || len(query) > maxBytes {
		return LogSearch{}, errorsx.Public("remote_invalid_request", "query must contain 1–4096 bytes and fit within max_bytes. Search is literal and case-sensitive.", nil)
	}
	chunk, err := m.ReadLog(ownerID, id, offset, maxBytes)
	if err != nil {
		return LogSearch{}, err
	}
	result := LogSearch{LogInfo: chunk.LogInfo, Matches: []LogMatch{}, NextOffset: chunk.NextOffset, Done: chunk.Expired || chunk.NextOffset >= chunk.TotalBytes}
	for pos := 0; pos < len(chunk.Text); {
		index := strings.Index(chunk.Text[pos:], query)
		if index < 0 {
			break
		}
		at := pos + index
		result.Matches = append(result.Matches, LogMatch{Offset: chunk.Offset + int64(at), Text: chunk.Text[max(0, at-80):min(len(chunk.Text), at+len(query)+80)]})
		pos = at + len(query)
		if len(result.Matches) == 100 {
			result.NextOffset = chunk.Offset + int64(pos)
			result.Done = result.NextOffset >= chunk.TotalBytes
			return result, nil
		}
	}
	if !result.Done {
		result.NextOffset = max(chunk.Offset+1, chunk.NextOffset-int64(len(query)-1))
	}
	return result, nil
}

// SettledLog is a read handle on a finished command's retained output. Settled
// files are immutable, so the handle reads outside the store lock; a running
// command's log is refused because a transfer needs a fixed size and digest.
type SettledLog struct {
	Receipt  store.RemoteJob
	Info     LogInfo
	file     *os.File
	capacity int64
}

// OpenSettledLog authorizes through the receipt owner, then opens the saved
// file directly. Callers must Close the handle.
func (m *Manager) OpenSettledLog(ownerID, id string) (*SettledLog, error) {
	receipt, err := m.Get(ownerID, id)
	if err != nil {
		return nil, err
	}
	if receipt.State == "running" {
		return nil, errorsx.Public("remote_log_running", "The command is still running, so its log has no final size yet. Read it with remote_read_log, or wait for the command to finish before fetching the whole log.", nil)
	}
	m.logs.mu.Lock()
	_, active := m.logs.active[id]
	m.logs.mu.Unlock()
	if active {
		return nil, errorsx.Public("remote_log_running", "The command's log is still being written. Retry once its receipt is settled.", nil)
	}
	file, err := os.Open(m.logs.path(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, errorsx.Public("remote_log_expired", "The saved log has expired on the destination; it cannot be fetched. Rerun the command if its output is still needed.", err)
	}
	if err != nil {
		return nil, logReadError(err)
	}
	log, err := readSettledHeader(file)
	if err != nil {
		file.Close()
		return nil, logReadError(err)
	}
	return &SettledLog{Receipt: receipt, Info: log.info(id), file: file, capacity: log.capacity}, nil
}

func readSettledHeader(file *os.File) (*jobLog, error) {
	var h [logHeaderBytes]byte
	if _, err := io.ReadFull(file, h[:]); err != nil {
		return nil, err
	}
	l := &jobLog{file: file, capacity: int64(binary.LittleEndian.Uint64(h[8:16])), total: int64(binary.LittleEndian.Uint64(h[16:24])), retained: int64(binary.LittleEndian.Uint64(h[24:32])), lost: h[32] != 0}
	if h[33] != 0 {
		return nil, errorsx.Public("remote_log_unavailable", "This job's log cannot be read: the destination stopped during a log overwrite, so its byte offsets are unreliable. The output is unrecoverable; do not rerun the command to recover it.", nil)
	}
	if string(h[:8]) != "AOLOG001" || l.capacity < 1 || l.capacity > DefaultMaxJobBytes || l.total < 0 || l.retained < 0 || l.retained > l.capacity || l.retained > l.total {
		return nil, errorsx.Public("remote_log_unavailable", "This job's log cannot be read: its saved file is damaged. The output is unrecoverable; do not rerun the command to recover it.", nil)
	}
	return l, nil
}

// ReadAt fills p from the retained output starting at position off, counted
// from the retained start rather than the absolute stream offset.
func (l *SettledLog) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off > l.Info.RetainedBytes {
		return 0, io.EOF
	}
	size := min(int64(len(p)), l.Info.RetainedBytes-off)
	if size == 0 {
		return 0, io.EOF
	}
	pos := (l.Info.StartOffset + off) % l.capacity
	first := min(size, l.capacity-pos)
	if _, err := l.file.ReadAt(p[:first], logHeaderBytes+pos); err != nil {
		return 0, err
	}
	if first < size {
		if _, err := l.file.ReadAt(p[first:size], logHeaderBytes); err != nil {
			return 0, err
		}
	}
	return int(size), nil
}

func (l *SettledLog) Close() error { return l.file.Close() }
