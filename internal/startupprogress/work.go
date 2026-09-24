package startupprogress

import (
	"errors"
	"io/fs"
	"log"
	"os"
	"sync"
	"time"
)

// ProcessWork is what this process has done since it started: CPU time and
// bytes moved to or from storage. IOSource names the counter the bytes came
// from; counts from different counters are not compared.
type ProcessWork struct {
	CPU      time.Duration
	IO       int64
	IOSource string
}

// The least work one heartbeat must see to count as progress, as rates over
// the interval: a twentieth of one CPU (50 ms per 1 s tick) and 64 KiB/s of
// storage I/O. An idle process answering polls and writing its log stays
// under both; a sort, a scan, a rebuild, a copy or a cold read is far over
// them. A step blocked on a lock does neither.
const (
	WorkCPUShare    = 20
	WorkIOPerSecond = 64 << 10
)

// WorkProgressed reports whether the work between two samples one interval
// apart counts as progress.
func WorkProgressed(prev, cur ProcessWork, interval time.Duration) bool {
	if cur.CPU-prev.CPU >= interval/WorkCPUShare {
		return true
	}
	minIO := int64(WorkIOPerSecond * interval.Seconds())
	return cur.IOSource == prev.IOSource && cur.IO-prev.IO >= minIO
}

// SamplerOptions configures a Sampler. Zero fields take the production
// defaults.
type SamplerOptions struct {
	// Interval is the time between samples, which scales the work
	// thresholds.
	Interval time.Duration
	// Stat reads a watched file. Default os.Stat.
	Stat func(string) (fs.FileInfo, error)
	// ReadWork samples the process's work. Default ReadProcessWork.
	ReadWork func() (ProcessWork, error)
	// Logf receives each kind of read failure once. Default log.Printf.
	Logf func(string, ...any)
}

// Sampler finds evidence that a process is making progress between two
// heartbeats: a watched file (such as a database and its WAL) changing
// size, or the process doing CPU or storage work over the thresholds. The
// backend's startup reporter and the update commands' relay use it, so one
// rule decides what progress is wherever a start is judged.
//
// Safe for concurrent use. Sample reads files and counters without holding
// the lock that Watch takes.
type Sampler struct {
	opts SamplerOptions

	mu sync.Mutex
	// watched maps each path to its size at the last look: -1 while it
	// does not exist, statUnknown until a look could read it.
	watched    map[string]int64
	statFailed map[string]bool
	work       ProcessWork
	haveWork   bool
	workFailed bool
}

// NewSampler returns a sampler that watches no files yet.
func NewSampler(opts SamplerOptions) *Sampler {
	if opts.Interval <= 0 {
		opts.Interval = time.Second
	}
	if opts.Stat == nil {
		opts.Stat = os.Stat
	}
	if opts.ReadWork == nil {
		opts.ReadWork = ReadProcessWork
	}
	if opts.Logf == nil {
		opts.Logf = log.Printf
	}
	return &Sampler{opts: opts, watched: map[string]int64{}, statFailed: map[string]bool{}}
}

// Watch adds files whose size changes count as progress. A file that does
// not exist yet counts when it appears. Its size now is the baseline.
func (s *Sampler) Watch(paths ...string) {
	sizes := make([]int64, len(paths))
	for i, path := range paths {
		sizes[i] = s.fileSize(path)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, path := range paths {
		s.watched[path] = sizes[i]
	}
}

// Sample reports whether anything progressed since the previous sample. The
// first work sample, one from a different I/O counter and the one after a
// failed read are baselines, not progress; a file that cannot be read is no
// evidence either way and keeps its last known size.
func (s *Sampler) Sample() bool {
	s.mu.Lock()
	paths := make([]string, 0, len(s.watched))
	for path := range s.watched {
		paths = append(paths, path)
	}
	s.mu.Unlock()
	sizes := make([]int64, len(paths))
	for i, path := range paths {
		sizes[i] = s.fileSize(path)
	}
	work, workErr := s.opts.ReadWork()

	s.mu.Lock()
	progressed := false
	for i, path := range paths {
		if sizes[i] == statUnknown {
			continue
		}
		if prev := s.watched[path]; prev != sizes[i] {
			s.watched[path] = sizes[i]
			if prev != statUnknown {
				progressed = true
			}
		}
	}
	logWorkErr := false
	if workErr != nil {
		// The next sample is a fresh baseline, so it is not measured
		// across the gap.
		s.haveWork = false
		logWorkErr = !s.workFailed
		s.workFailed = true
	} else {
		if s.haveWork && WorkProgressed(s.work, work, s.opts.Interval) {
			progressed = true
		}
		s.work, s.haveWork = work, true
	}
	s.mu.Unlock()
	if logWorkErr {
		s.opts.Logf("startup progress: cannot read this process's CPU and I/O, so only reports and file sizes count as progress: %v", workErr)
	}
	return progressed
}

// statUnknown is a size fileSize could not read.
const statUnknown = -2

// fileSize is path's size, -1 when it does not exist, or statUnknown when it
// cannot be read. A read failure is logged once per path.
func (s *Sampler) fileSize(path string) int64 {
	info, err := s.opts.Stat(path)
	switch {
	case err == nil:
		return info.Size()
	case errors.Is(err, fs.ErrNotExist):
		return -1
	}
	s.mu.Lock()
	first := !s.statFailed[path]
	s.statFailed[path] = true
	s.mu.Unlock()
	if first {
		s.opts.Logf("startup progress: cannot read %s, its writes will not count as progress: %v", path, err)
	}
	return statUnknown
}
