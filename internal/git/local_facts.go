package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/attachment"
	"agent-overflow/internal/gitdiff"
	"agent-overflow/internal/stringsx"
)

// Local facts: commit:, image: and lines: keys, read from the
// workspace's repository without touching the network
// (GIT_NO_LAZY_FETCH=1, no fetch): an object that is not local answers
// unavailable. One call's misses cost one `git cat-file --batch-check`
// for every key's object, one `git cat-file --batch` for the commits'
// subjects and the lines ranges' blobs (streamed: no blob is held whole),
// one header read per image blob the blob image cache lacks, and, only
// for a path its commit's tree may lack, one `git ls-tree` per commit.
// Every answer is content addressed (a full commit id and a path name
// one blob forever), so found and absent facts are held without expiry.

const (
	// localFactsTimeout bounds each git read of a call, within the
	// method's default budget beside the forge reads.
	localFactsTimeout = 15 * time.Second
	// The lines: bounds: at most maxFactLines lines and maxFactLinesBytes
	// of text from a blob of at most maxFactLinesBlobBytes, which is
	// checked from its size before it is read; git's binary test reads
	// the first factBinaryProbe bytes.
	maxFactLines          = 200
	maxFactLinesBytes     = 64 << 10
	maxFactLinesBlobBytes = 2 << 20
	factBinaryProbe       = 8000
	// factImageHead is the prefix an image's type is sniffed from (the
	// svg sniff reads 4096 bytes), and factImageHeaderMax the most of an
	// image blob a header read reads before it stops git.
	factImageHead      = 4096
	factImageHeaderMax = 1 << 20
	// factImageReads bounds the header reads one call runs at once.
	factImageReads = 4
	// factCommitBytes is how much of a commit object is read for its
	// subject.
	factCommitBytes = 64 << 10
	// The local facts cache: at most localFactCacheEntries facts and
	// localFactCacheBytes estimated bytes (a lines range holds up to
	// 64 KiB), least recently used evicted first.
	localFactCacheEntries = 2048
	localFactCacheBytes   = 8 << 20
	// The blob image cache: one entry per blob, at most
	// blobImageCacheEntries of them, least recently used evicted first.
	blobImageCacheEntries = 4096
	blobImageCacheBytes   = 2 << 20
)

// BlobImage is what a blob is as an image: its id and size, and its
// sniffed type and declared pixel size when a browser displays it (Width
// and Height zero for svg, ico and avif), or LFS for a Git LFS pointer.
// A blob that is neither has neither set.
type BlobImage struct {
	OID    string
	Size   int64
	MIME   string
	Width  int
	Height int
	LFS    bool
}

// Displayable is whether a browser displays the blob as an image.
func (b BlobImage) Displayable() bool { return b.MIME != "" }

func blobImageBytes(key string, b BlobImage) int64 {
	return int64(128 + 2*len(key) + len(b.OID) + len(b.MIME))
}

// newLocalFactCaches makes a Core's local fact caches.
func newLocalFactCaches() (*factCache[ForgeFact], *factCache[BlobImage]) {
	return newFactCache(localFactCacheEntries, localFactCacheBytes, factBytes),
		newFactCache(blobImageCacheEntries, blobImageCacheBytes, blobImageBytes)
}

// localFactKey is a parsed local key: a commit id prefix (commit:), or a
// full commit id and a path, with a line range for lines:.
type localFactKey struct {
	key, kind  string
	hex        string
	commit     string
	path       string
	start, end int
	// cacheKey names the answer: the kind, the full commit id head or
	// base named, the path and the range.
	cacheKey string
}

// parseLocalFactKey reads key at rev. A malformed key, and a commitish
// rev cannot name, answer unavailable; a path that is not a clean
// repository path, and a range past the line cap, answer absent; a path
// git's line protocol cannot carry (a newline) answers unavailable. Each
// comes back as the status, with no git run.
func parseLocalFactKey(key string, rev PRRevision) (localFactKey, string) {
	kind, rest, _ := strings.Cut(key, ":")
	k := localFactKey{key: key, kind: kind}
	switch kind {
	case "commit":
		if len(rest) < 7 || len(rest) > 64 || !lowerHex(rest) {
			return k, FactUnavailable
		}
		k.hex, k.cacheKey = rest, key
		return k, ""
	case "image":
		commitish, path, ok := strings.Cut(rest, ":")
		if !ok {
			return k, FactUnavailable
		}
		switch commitish {
		case "head":
			k.commit = rev.HeadSHA
		case "base":
			k.commit = rev.BaseSHA
		default:
			if !gitdiff.IsFullOID(commitish) {
				return k, FactUnavailable
			}
			k.commit = commitish
		}
		if k.commit == "" {
			return k, FactUnavailable
		}
		k.path = path
	case "lines":
		sha, rest, ok := strings.Cut(rest, ":")
		i := strings.LastIndex(rest, ":")
		if !ok || i < 0 || !gitdiff.IsFullOID(sha) {
			return k, FactUnavailable
		}
		first, last, ok := strings.Cut(rest[i+1:], "-")
		start, okStart := factNumber(first)
		end, okEnd := factNumber(last)
		if !ok || !okStart || !okEnd || end < start {
			return k, FactUnavailable
		}
		k.commit, k.path, k.start, k.end = sha, rest[:i], start, end
		if end-start+1 > maxFactLines {
			return k, FactAbsent
		}
	default:
		return k, FactUnavailable
	}
	if _, err := gitdiff.CleanPath(k.path); err != nil || strings.HasPrefix(k.path, "-") {
		return k, FactAbsent
	}
	if strings.ContainsAny(k.path, "\n\r") {
		return k, FactUnavailable
	}
	k.cacheKey = kind + ":" + k.commit + ":" + k.path
	if kind == "lines" {
		k.cacheKey += ":" + strconv.Itoa(k.start) + "-" + strconv.Itoa(k.end)
	}
	return k, ""
}

func lowerHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// resolveLocalFacts answers keys, each a local kind's, from the
// repository at cwd at rev: one fact per key, in keys' order.
func (c *Core) resolveLocalFacts(ctx context.Context, cwd string, rev PRRevision, keys []string) []ForgeFact {
	out := make([]ForgeFact, 0, len(keys))
	parsed := make([]localFactKey, 0, len(keys))
	for _, key := range keys {
		k, status := parseLocalFactKey(key, rev)
		if status != "" {
			out = append(out, ForgeFact{Key: key, Status: status})
			continue
		}
		parsed = append(parsed, k)
	}
	if len(parsed) == 0 {
		return out
	}
	prefix := cwd + "\x00"
	facts := resolveCached(ctx, c.localFacts, c.nowFn, parsed,
		func(k localFactKey) string { return prefix + k.cacheKey },
		func(k localFactKey) string { return k.key },
		func(owned []localFactKey) []ForgeFact { return c.readLocalFacts(ctx, cwd, owned) },
		func(fact ForgeFact, _ time.Time) (bool, time.Time) {
			// A commit id prefix that is missing or ambiguous now may
			// resolve after a fetch; it is unavailable, never held.
			return fact.Status == FactFound || fact.Status == FactAbsent, time.Time{}
		})
	// A cached fact is another key's spelling of the same answer (head
	// and its sha); it answers under this key.
	for i := range facts {
		facts[i].Key = parsed[i].key
	}
	return append(out, facts...)
}

// objectInfo is one batch-check answer.
type objectInfo struct {
	oid, kind string
	size      int64
	missing   bool
}

// readLocalFacts reads keys from cwd's repository: one fact per key.
func (c *Core) readLocalFacts(ctx context.Context, cwd string, keys []localFactKey) []ForgeFact {
	ctx, cancel := context.WithTimeout(ctx, localFactsTimeout)
	defer cancel()
	facts := make([]ForgeFact, len(keys))
	var queries []string
	seen := map[string]bool{}
	ask := func(query string) {
		if !seen[query] {
			seen[query] = true
			queries = append(queries, query)
		}
	}
	for _, k := range keys {
		if k.kind == "commit" {
			ask(k.hex + "^{commit}")
			continue
		}
		ask(k.commit + "^{commit}")
		ask(k.commit + ":" + k.path)
	}
	objects, err := c.batchCheck(ctx, cwd, queries)
	if err != nil {
		log.Printf("local facts: cat-file --batch-check: %d keys unavailable: %v", len(keys), err)
		for i, k := range keys {
			facts[i] = unavailableFact(k.key)
		}
		return facts
	}
	var (
		commits = map[string]*commitSink{}
		lines   = map[string]*linesSink{}
		images  = map[string][]int{}
		missing = map[string][]int{}
		imageAt = map[string]int64{}
		order   []string
	)
	for i, k := range keys {
		facts[i] = unavailableFact(k.key)
		if k.kind == "commit" {
			obj := objects[k.hex+"^{commit}"]
			if obj.kind == "commit" && strings.HasPrefix(obj.oid, k.hex) {
				facts[i] = ForgeFact{Key: k.key, Status: FactFound, OID: obj.oid}
				if commits[obj.oid] == nil {
					commits[obj.oid] = &commitSink{}
					order = append(order, obj.oid)
				}
			}
			continue
		}
		if objects[k.commit+"^{commit}"].kind != "commit" {
			continue
		}
		obj := objects[k.commit+":"+k.path]
		switch {
		case obj.missing:
			missing[k.commit] = append(missing[k.commit], i)
			continue
		case obj.kind != "blob":
			facts[i] = absentFact(k.key)
			continue
		}
		switch k.kind {
		case "image":
			switch {
			case obj.size > attachment.DisplayImageMaxBytes:
			case obj.size == 0:
				facts[i] = absentFact(k.key)
			default:
				if held, ok := c.blobImages.get(obj.oid, c.nowFn()); ok {
					facts[i] = imageFact(k.key, held)
					continue
				}
				images[obj.oid] = append(images[obj.oid], i)
				imageAt[obj.oid] = obj.size
			}
		case "lines":
			switch {
			case obj.size > maxFactLinesBlobBytes:
			case obj.size == 0:
				facts[i] = absentFact(k.key)
			default:
				sink := lines[obj.oid]
				if sink == nil {
					sink = &linesSink{}
					lines[obj.oid] = sink
					order = append(order, obj.oid)
				}
				sink.ranges = append(sink.ranges, &lineRange{at: i, start: k.start, end: k.end})
				facts[i] = ForgeFact{Key: k.key, Status: FactFound, OID: obj.oid, Path: k.path}
			}
		}
	}
	var wg sync.WaitGroup
	if len(order) > 0 {
		wg.Go(func() {
			sinks := make(map[string]objectSink, len(order))
			for oid, sink := range commits {
				sinks[oid] = sink
			}
			for oid, sink := range lines {
				sinks[oid] = sink
			}
			if err := c.batchObjects(ctx, cwd, order, sinks); err != nil {
				log.Printf("local facts: cat-file --batch: %d objects unread: %v", len(order), err)
				// A commit without its subject is not held as found.
				for i, k := range keys {
					if k.kind == "commit" && facts[i].Status == FactFound {
						facts[i] = unavailableFact(k.key)
					}
				}
				for _, sink := range lines {
					for _, r := range sink.ranges {
						facts[r.at] = unavailableFact(keys[r.at].key)
					}
				}
				return
			}
			for i, k := range keys {
				if k.kind == "commit" && facts[i].Status == FactFound {
					facts[i].Title = commits[facts[i].OID].subject()
				}
			}
			for _, sink := range lines {
				for _, r := range sink.ranges {
					text, ok := sink.text(r)
					if !ok {
						facts[r.at] = absentFact(keys[r.at].key)
						continue
					}
					facts[r.at].Text = text
				}
			}
		})
	}
	if len(images) > 0 {
		wg.Go(func() {
			var mu sync.Mutex
			slots := make(chan struct{}, factImageReads)
			var reads sync.WaitGroup
			for oid, at := range images {
				reads.Go(func() {
					slots <- struct{}{}
					defer func() { <-slots }()
					image, err := c.readBlobImage(ctx, cwd, oid, imageAt[oid])
					mu.Lock()
					defer mu.Unlock()
					if err != nil {
						log.Printf("local facts: image header %s: %v", oid, err)
						return
					}
					c.blobImages.put(oid, image, time.Time{})
					for _, i := range at {
						facts[i] = imageFact(keys[i].key, image)
					}
				})
			}
			reads.Wait()
		})
	}
	if len(missing) > 0 {
		wg.Go(func() {
			for commit, at := range missing {
				paths := make([]string, len(at))
				for j, i := range at {
					paths[j] = keys[i].path
				}
				present, err := c.treePaths(ctx, cwd, commit, paths)
				if err != nil {
					log.Printf("local facts: ls-tree: %d paths unavailable: %v", len(paths), err)
					continue
				}
				for _, i := range at {
					// A path its tree holds whose object is not local
					// (a partial clone) stays unavailable.
					if !present[keys[i].path] {
						facts[i] = absentFact(keys[i].key)
					}
				}
			}
		})
	}
	wg.Wait()
	return facts
}

func imageFact(key string, image BlobImage) ForgeFact {
	switch {
	case image.LFS:
		return ForgeFact{Key: key, Status: FactFound, OID: image.OID, LFS: true}
	case image.Displayable():
		return ForgeFact{Key: key, Status: FactFound, OID: image.OID, Size: image.Size, Width: image.Width, Height: image.Height, MIME: image.MIME}
	}
	return absentFact(key)
}

// factGit runs one read-only git command of the facts reads: under ctx
// and localFactsTimeout, with lazy fetching off.
func (c *Core) factGit(ctx context.Context, cwd string, spec commandSpec) (commandResult, error) {
	spec.binary, spec.cwd, spec.ctx = "git", cwd, ctx
	spec.extraEnv = append(spec.extraEnv, "GIT_NO_LAZY_FETCH=1")
	if spec.timeout == 0 {
		spec.timeout = localFactsTimeout
	}
	if hook := c.beforeFactGit; hook != nil {
		hook(spec.args)
	}
	result, err := c.runSpec(spec)
	if err == nil && result.exitCode != 0 {
		err = commandFailure("git "+spec.args[0], result)
	}
	return result, err
}

// batchCheck answers each query's object in one `git cat-file
// --batch-check`: a query git cannot resolve (missing, ambiguous, or not
// local) answers missing.
func (c *Core) batchCheck(ctx context.Context, cwd string, queries []string) (map[string]objectInfo, error) {
	result, err := c.factGit(ctx, cwd, commandSpec{
		stdin:    strings.Join(queries, "\n") + "\n",
		maxBytes: int64(len(queries))*(gitdiff.MaxEntryPathBytes/16) + 1<<20,
		args:     []string{"cat-file", "--batch-check=%(objectname) %(objecttype) %(objectsize)"},
	})
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSuffix(result.stdout, "\n"), "\n")
	if len(lines) != len(queries) {
		return nil, fmt.Errorf("cat-file --batch-check answered %d of %d objects", len(lines), len(queries))
	}
	objects := make(map[string]objectInfo, len(queries))
	for i, line := range lines {
		query := queries[i]
		if line == query+" missing" || line == query+" ambiguous" {
			objects[query] = objectInfo{missing: true}
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 || !gitdiff.IsFullOID(fields[0]) {
			return nil, fmt.Errorf("cat-file --batch-check: malformed answer %q", stringsx.ClipRunes(line, 200))
		}
		size, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || size < 0 {
			return nil, fmt.Errorf("cat-file --batch-check: malformed size %q", fields[2])
		}
		objects[query] = objectInfo{oid: fields[0], kind: fields[1], size: size}
	}
	return objects, nil
}

// treePaths answers which of paths commit's tree holds, in `git ls-tree`
// runs of at most gitdiff.MaxEntryPaths paths (the paths ride argv),
// which read trees and never blobs.
func (c *Core) treePaths(ctx context.Context, cwd, commit string, paths []string) (map[string]bool, error) {
	present := make(map[string]bool, len(paths))
	for start := 0; start < len(paths); start += gitdiff.MaxEntryPaths {
		batch := paths[start:min(start+gitdiff.MaxEntryPaths, len(paths))]
		args := append([]string{"ls-tree", "-z", "--name-only", "--full-tree", commit, "--"}, batch...)
		result, err := c.factGit(ctx, cwd, commandSpec{extraEnv: []string{"GIT_LITERAL_PATHSPECS=1"}, args: args})
		if err != nil {
			return nil, err
		}
		for name := range strings.SplitSeq(strings.TrimSuffix(result.stdout, "\x00"), "\x00") {
			present[name] = true
		}
	}
	return present, nil
}

// errFactHeaderRead stops a header read's git once the header is read.
var errFactHeaderRead = errors.New("the image header is read")

// readBlobImage classifies blob oid of size bytes as an image from its
// header: git streams it into a pipe the classification reads only as
// far as the header (at most factImageHeaderMax bytes), then git is
// stopped. Never more than that is read of a large image.
func (c *Core) readBlobImage(ctx context.Context, cwd, oid string, size int64) (BlobImage, error) {
	image, _, err := c.readBlobImageCounted(ctx, cwd, oid, size)
	return image, err
}

// readBlobImageCounted is readBlobImage, answering the bytes it read.
func (c *Core) readBlobImageCounted(ctx context.Context, cwd, oid string, size int64) (BlobImage, int64, error) {
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		_, err := c.factGit(ctx, cwd, commandSpec{output: writer, outputLimit: size, args: []string{"cat-file", "blob", oid}})
		writer.CloseWithError(errors.Join(err, io.EOF))
		done <- err
	}()
	counted := &countingReader{r: reader}
	head := make([]byte, min(size, factImageHead))
	_, readErr := io.ReadFull(counted, head)
	image := BlobImage{OID: oid, Size: size}
	var classifyErr error
	if readErr == nil {
		var mime string
		mime, image.Width, image.Height, classifyErr = classifyDisplayImage(head, io.MultiReader(bytes.NewReader(head), io.LimitReader(counted, factImageHeaderMax-int64(len(head)))))
		switch {
		case classifyErr == nil:
			image.MIME = mime
		case errors.Is(classifyErr, ErrLFSPointer):
			image.LFS, classifyErr = true, nil
		case errors.Is(classifyErr, ErrNotDisplayImage) && counted.n < factImageHeaderMax:
			classifyErr = nil
		default:
			classifyErr = fmt.Errorf("no image header in the first %d bytes", factImageHeaderMax)
		}
	}
	// Closing the pipe fails git's next write, which stops git; what git
	// answers after the header was read is not the read's outcome.
	reader.CloseWithError(errFactHeaderRead)
	gitErr := <-done
	switch {
	case readErr != nil:
		return BlobImage{}, counted.n, errors.Join(readErr, gitErr)
	case classifyErr != nil:
		return BlobImage{}, counted.n, classifyErr
	case ctx.Err() != nil:
		return BlobImage{}, counted.n, ctx.Err()
	}
	return image, counted.n, nil
}

type countingReader struct {
	r io.Reader
	n int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.n += int64(n)
	return n, err
}

// objectSink takes one object's content from a `git cat-file --batch`
// stream.
type objectSink interface{ write(p []byte) }

// batchObjects streams oids' contents from one `git cat-file --batch`
// into their sinks, holding at most one header line of the stream.
func (c *Core) batchObjects(ctx context.Context, cwd string, oids []string, sinks map[string]objectSink) error {
	stream := &batchStream{sinks: sinks}
	limit := int64(0)
	for range oids {
		limit += 256
	}
	limit += int64(len(oids)) * maxFactLinesBlobBytes
	if _, err := c.factGit(ctx, cwd, commandSpec{stdin: strings.Join(oids, "\n") + "\n", output: stream, outputLimit: limit, args: []string{"cat-file", "--batch"}}); err != nil {
		return err
	}
	if stream.err != nil {
		return stream.err
	}
	if stream.objects != len(oids) || stream.remaining != 0 || len(stream.header) != 0 {
		return fmt.Errorf("cat-file --batch answered %d of %d objects", stream.objects, len(oids))
	}
	return nil
}

// batchStream parses `git cat-file --batch` output: per object a header
// "<oid> <type> <size>\n", its content and "\n".
type batchStream struct {
	sinks     map[string]objectSink
	header    []byte
	sink      objectSink
	remaining int64
	trailer   bool
	objects   int
	err       error
}

func (s *batchStream) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 && s.err == nil {
		switch {
		case s.remaining > 0:
			take := int(min(int64(len(p)), s.remaining))
			if s.sink != nil {
				s.sink.write(p[:take])
			}
			s.remaining -= int64(take)
			p = p[take:]
			s.trailer = s.remaining == 0
		case s.trailer:
			if p[0] != '\n' {
				s.err = errors.New("cat-file --batch: an object is not followed by a newline")
				break
			}
			p, s.trailer = p[1:], false
			s.objects++
		default:
			i := bytes.IndexByte(p, '\n')
			if i < 0 {
				s.header = append(s.header, p...)
				p = nil
				if len(s.header) > 512 {
					s.err = errors.New("cat-file --batch: an object header is too long")
				}
				break
			}
			s.header = append(s.header, p[:i]...)
			p = p[i+1:]
			fields := strings.Fields(string(s.header))
			s.header = s.header[:0]
			if len(fields) != 3 {
				s.err = fmt.Errorf("cat-file --batch: object %q is missing", strings.Join(fields, " "))
				break
			}
			size, err := strconv.ParseInt(fields[2], 10, 64)
			if err != nil || size < 0 {
				s.err = fmt.Errorf("cat-file --batch: malformed size %q", fields[2])
				break
			}
			s.sink, s.remaining = s.sinks[fields[0]], size
			s.trailer = size == 0
		}
	}
	if s.err != nil {
		return 0, s.err
	}
	return n, nil
}

// commitSink keeps the first factCommitBytes of a commit object.
type commitSink struct{ buf []byte }

func (s *commitSink) write(p []byte) {
	if room := factCommitBytes - len(s.buf); room > 0 {
		s.buf = append(s.buf, p[:min(room, len(p))]...)
	}
}

// subject is the commit's subject as git's %s reads it: the message's
// first paragraph, its lines joined by spaces; empty when it is not
// within the bytes kept.
func (s *commitSink) subject() string {
	_, message, ok := bytes.Cut(s.buf, []byte("\n\n"))
	if !ok {
		return ""
	}
	message = bytes.TrimLeft(message, "\n")
	paragraph, _, _ := bytes.Cut(message, []byte("\n\n"))
	lines := strings.Fields(strings.ReplaceAll(string(paragraph), "\n", " "))
	return factTitle(strings.ToValidUTF8(strings.Join(lines, " "), "�"))
}

// lineRange is one lines: key's range and what it captured.
type lineRange struct {
	at         int
	start, end int
	buf        []byte
	tooLarge   bool
}

// linesSink captures a blob's ranges as it streams, keeping no more than
// each range's text (cut at maxFactLinesBytes), and notes whether the
// blob is binary (a NUL in its first factBinaryProbe bytes).
type linesSink struct {
	ranges  []*lineRange
	line    int
	seen    int64
	last    byte
	binary  bool
	started bool
}

func (s *linesSink) write(p []byte) {
	if !s.started {
		s.started, s.line = true, 1
	}
	if s.seen < factBinaryProbe {
		probe := p[:min(int64(len(p)), factBinaryProbe-s.seen)]
		if bytes.IndexByte(probe, 0) >= 0 {
			s.binary = true
		}
	}
	s.seen += int64(len(p))
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		segment := p
		if i >= 0 {
			segment = p[:i+1]
		}
		for _, r := range s.ranges {
			if s.line < r.start || s.line > r.end || r.tooLarge {
				continue
			}
			if len(r.buf)+len(segment) > maxFactLinesBytes+1 {
				r.tooLarge = true
				r.buf = nil
				continue
			}
			r.buf = append(r.buf, segment...)
		}
		s.last = segment[len(segment)-1]
		if i < 0 {
			break
		}
		s.line++
		p = p[i+1:]
	}
}

// text is r's lines, or false when the blob is binary or the range is
// past its end or over the text cap.
func (s *linesSink) text(r *lineRange) (string, bool) {
	total := s.line
	if s.last == '\n' {
		total--
	}
	if s.binary || r.tooLarge || r.end > total {
		return "", false
	}
	text := strings.ReplaceAll(strings.TrimSuffix(string(r.buf), "\n"), "\r\n", "\n")
	text = strings.TrimSuffix(text, "\r")
	if len(text) > maxFactLinesBytes || bytes.IndexByte([]byte(text), 0) >= 0 {
		return "", false
	}
	return text, true
}

// BlobImage answers blob oid of cwd's repository as an image, from the
// blob image cache or else its size and type (one `git cat-file
// --batch-check`) and its header (readBlobImage), without the network.
// An oid that is no full object id, no local blob, or one over
// attachment.DisplayImageMaxBytes is an error naming none of its content.
func (c *Core) BlobImage(ctx context.Context, cwd, oid string) (BlobImage, error) {
	if !gitdiff.IsFullOID(oid) {
		return BlobImage{}, errors.New("not a full object ID")
	}
	if held, ok := c.blobImages.get(oid, c.nowFn()); ok {
		return held, nil
	}
	ctx, cancel := context.WithTimeout(ctx, localFactsTimeout)
	defer cancel()
	objects, err := c.batchCheck(ctx, cwd, []string{oid})
	if err != nil {
		return BlobImage{}, err
	}
	obj := objects[oid]
	switch {
	case obj.missing || obj.kind != "blob":
		return BlobImage{}, ErrNoLocalBlob
	case obj.size > attachment.DisplayImageMaxBytes:
		return BlobImage{}, fmt.Errorf("%w: %d bytes exceeds %d", ErrBlobTooLarge, obj.size, attachment.DisplayImageMaxBytes)
	case obj.size == 0:
		return BlobImage{OID: oid}, nil
	}
	image, err := c.readBlobImage(ctx, cwd, oid, obj.size)
	if err != nil {
		return BlobImage{}, err
	}
	c.blobImages.put(oid, image, time.Time{})
	return image, nil
}

var (
	// ErrNoLocalBlob is an object id that names no blob present in the
	// repository.
	ErrNoLocalBlob = errors.New("no such blob in this repository")
	// ErrBlobTooLarge is a blob over the display image cap.
	ErrBlobTooLarge = errors.New("the blob is larger than an image shown")
)

// ReadBlob fills buf with blob oid of cwd's repository, whose size the
// caller learned from BlobImage: exactly len(buf) bytes, or an error.
// git's output goes into buf and never past it, under ctx, without the
// network.
func (c *Core) ReadBlob(ctx context.Context, cwd, oid string, buf []byte) error {
	if !gitdiff.IsFullOID(oid) {
		return errors.New("not a full object ID")
	}
	out := &fixedBuffer{buf: buf}
	if _, err := c.factGit(ctx, cwd, commandSpec{output: out, outputLimit: int64(len(buf)), args: []string{"cat-file", "blob", oid}}); err != nil {
		return err
	}
	if out.n != len(buf) {
		return fmt.Errorf("blob %s is not %d bytes", oid, len(buf))
	}
	return nil
}

type fixedBuffer struct {
	buf []byte
	n   int
}

func (w *fixedBuffer) Write(p []byte) (int, error) {
	n := copy(w.buf[w.n:], p)
	w.n += n
	if n < len(p) {
		return n, io.ErrShortWrite
	}
	return n, nil
}
