package git

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"slices"
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
// one header read per image blob the blob image cache lacks, one lstat per
// Git LFS pointer blob (local_lfs.go) and one `git config` when the
// workspace's LFS store directory is not held, and, only for a path its
// commit's tree may lack, one `git ls-tree` per batch of paths of a
// commit. An answer keyed by full object ids is content addressed (a full
// commit id and a path name one blob forever), so its found and absent
// facts are held without expiry. A found abbreviated commit id can change
// (a fetch makes the prefix ambiguous): it is held for changingFactTTL,
// the forge facts' freshness. An image answer read through an LFS pointer
// changes with the local store (a `git lfs pull` or prune) and is never
// held: each call reaches the pointer's lstat.

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
	// The blob image cache: one entry per blob, LFS object and LFS store
	// (local_lfs.go), at most blobImageCacheEntries of them, least recently
	// used evicted first.
	blobImageCacheEntries = 4096
	blobImageCacheBytes   = 2 << 20
	// changingFactTTL is how long a local answer that can change is held:
	// the forge facts' freshness.
	changingFactTTL = forgeFactFoundTTL
)

// BlobImage is what a blob is as an image: its id and size, and its
// sniffed type and how an <img> of it is sized when a browser displays it
// (attachment.DisplaySize: Width and Height its pixels, or, when Fluid,
// only its ratio; zero when unknown), or LFS
// for a Git LFS pointer whose object is not in the local LFS store. A
// pointer whose object is local is that object's image: Size and the
// header are the object's, OID stays the pointer blob's, and ReadImage
// reads the object. A blob that is neither has neither set.
type BlobImage struct {
	OID    string
	Size   int64
	MIME   string
	Width  int
	Height int
	Fluid  bool
	LFS    bool
	// pointer is set only on the blob image cache's record of a pointer
	// blob whose object can be shown (a parsed pointer within
	// attachment.DisplayImageMaxBytes); such a record is no answer:
	// lfsBlobImage answers it.
	pointer gitdiff.LFSPointer
	// lfsPath and lfsOID are a local LFS object's file and sha256.
	lfsPath, lfsOID string
}

// Displayable is whether a browser displays the blob as an image.
func (b BlobImage) Displayable() bool { return b.MIME != "" }

// LFSObject is the sha256 of the local LFS object ReadImage reads for a
// pointer blob, "" for a blob read as itself. The image's bytes are that
// object's, not the blob's: a holder keys them by it, never by OID.
func (b BlobImage) LFSObject() string { return b.lfsOID }

func blobImageBytes(key string, b BlobImage) int64 {
	return int64(128 + 2*len(key) + len(b.OID) + len(b.MIME) + len(b.pointer.OID) + len(b.lfsPath) + len(b.lfsOID))
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
	var holds map[string]localHold
	facts := resolveCached(ctx, c.localFacts, c.nowFn, parsed,
		func(k localFactKey) string { return prefix + k.cacheKey },
		func(k localFactKey) string { return k.key },
		func(owned []localFactKey) []ForgeFact {
			var facts []ForgeFact
			facts, holds = c.readLocalFacts(ctx, cwd, owned)
			return facts
		},
		func(fact ForgeFact, _ time.Time) (bool, time.Time) {
			// A commit id prefix that is missing or ambiguous now may
			// resolve after a fetch; it is unavailable, never held.
			hold := holds[fact.Key]
			return !hold.never && (fact.Status == FactFound || fact.Status == FactAbsent), hold.expires
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

// localHold is how long a local fact may be held: never, until expires,
// or (both zero) without expiry.
type localHold struct {
	expires time.Time
	never   bool
}

// readLocalFacts reads keys from cwd's repository: one fact per key, and
// how long each fact that can change may be held, by key (absent: without
// expiry).
func (c *Core) readLocalFacts(ctx context.Context, cwd string, keys []localFactKey) ([]ForgeFact, map[string]localHold) {
	ctx, cancel := context.WithTimeout(ctx, localFactsTimeout)
	defer cancel()
	now := c.nowFn()
	facts := make([]ForgeFact, len(keys))
	holds := map[string]localHold{}
	lfs := c.lfsStore(ctx, cwd)
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
		return facts, holds
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
				if len(k.hex) < len(obj.oid) {
					// A later fetch can make an abbreviation ambiguous.
					holds[k.key] = localHold{expires: now.Add(changingFactTTL)}
				}
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
				// A held pointer record is resolved by the reads below.
				if held, ok := c.blobImages.get(obj.oid, now); ok && held.pointer.OID == "" {
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
					image, throughPointer, err := c.blobImage(ctx, cwd, oid, imageAt[oid], lfs)
					mu.Lock()
					defer mu.Unlock()
					if err != nil {
						log.Printf("local facts: image header %s: %v", oid, err)
						return
					}
					for _, i := range at {
						facts[i] = imageFact(keys[i].key, image)
						if throughPointer {
							holds[keys[i].key] = localHold{never: true}
						}
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
	return facts, holds
}

func imageFact(key string, image BlobImage) ForgeFact {
	switch {
	case image.LFS:
		return ForgeFact{Key: key, Status: FactFound, OID: image.OID, LFS: true}
	case image.Displayable():
		return ForgeFact{Key: key, Status: FactFound, OID: image.OID, Size: image.Size, Width: image.Width, Height: image.Height, Fluid: image.Fluid, MIME: image.MIME}
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
// runs of at most gitdiff.MaxEntryPaths paths totaling at most
// gitdiff.MaxEntryPathBytes (the paths ride argv), which read trees and
// never blobs.
func (c *Core) treePaths(ctx context.Context, cwd, commit string, paths []string) (map[string]bool, error) {
	present := make(map[string]bool, len(paths))
	for start := 0; start < len(paths); {
		end, total := start, 0
		// A path is at most 4096 bytes (gitdiff.CleanPath), so each run
		// takes at least one.
		for end < len(paths) && end-start < gitdiff.MaxEntryPaths && (end == start || total+len(paths[end]) <= gitdiff.MaxEntryPathBytes) {
			total += len(paths[end])
			end++
		}
		batch := paths[start:end]
		start = end
		args := append([]string{"ls-tree", "-z", "--name-only", "--full-tree", commit, "--"}, batch...)
		result, err := c.factGit(ctx, cwd, commandSpec{extraEnv: []string{"GIT_LITERAL_PATHSPECS=1"}, args: args})
		if err != nil {
			return nil, err
		}
		for name := range strings.SplitSeq(strings.TrimSuffix(result.stdout, "\x00"), "\x00") {
			if name != "" {
				present[name] = true
			}
		}
	}
	return present, nil
}

// errFactHeaderRead stops a header read's git once the header is read.
var errFactHeaderRead = errors.New("the image header is read")

// blobImage answers blob oid of size bytes as an image, and whether the
// answer was read through a Git LFS pointer (it changes with lfs, cwd's
// local LFS store). What the blob is, its classification or its parsed
// pointer, is held in the blob image cache by blob id without expiry
// (content addressed) or else read (readBlobImage); a pointer is resolved
// against lfs on every call (lfsBlobImage).
func (c *Core) blobImage(ctx context.Context, cwd, oid string, size int64, lfs *lfsStore) (BlobImage, bool, error) {
	record, ok := c.blobImages.get(oid, c.nowFn())
	if !ok {
		var err error
		if record, err = c.readBlobImage(ctx, cwd, oid, size); err != nil {
			return BlobImage{}, false, err
		}
		c.blobImages.put(oid, record, time.Time{})
	}
	return resolveBlobImage(record, lfs)
}

// resolveBlobImage answers record, a blob image cache record, as an
// image: itself, or for a pointer its object in lfs as it is now.
func resolveBlobImage(record BlobImage, lfs *lfsStore) (BlobImage, bool, error) {
	if record.pointer.OID == "" {
		return record, false, nil
	}
	image, err := lfs.lfsBlobImage(record)
	if err != nil {
		return BlobImage{}, false, err
	}
	return image, true, nil
}

// readBlobImage classifies blob oid of size bytes as an image from its
// header (readBlobImageCounted). A Git LFS pointer is LFS, with its parsed
// pointer set when its object can be shown: it parses and its size is
// within attachment.DisplayImageMaxBytes.
func (c *Core) readBlobImage(ctx context.Context, cwd, oid string, size int64) (BlobImage, error) {
	image, head, _, err := c.readBlobImageCounted(ctx, cwd, oid, size)
	if err != nil || !image.LFS {
		return image, err
	}
	if pointer, ok := gitdiff.ParseLFSPointer(head); ok && pointer.Size <= attachment.DisplayImageMaxBytes {
		image.pointer = pointer
	}
	return image, nil
}

// readBlobImageCounted classifies blob oid of size bytes as an image from
// its header: git streams it into a pipe the classification reads only as
// far as the header (at most factImageHeaderMax bytes), then git is
// stopped. Never more than that is read of a large image. It answers the
// pointer's bytes for an LFS pointer, and the bytes it read.
func (c *Core) readBlobImageCounted(ctx context.Context, cwd, oid string, size int64) (BlobImage, []byte, int64, error) {
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		_, err := c.factGit(ctx, cwd, commandSpec{output: writer, outputLimit: size, args: []string{"cat-file", "blob", oid}})
		writer.CloseWithError(errors.Join(err, io.EOF))
		done <- err
	}()
	counted := &countingReader{r: reader}
	image, head, err := classifyImageReader(counted, size)
	// Closing the pipe fails git's next write, which stops git; what git
	// answers after the header was read is not the read's outcome.
	reader.CloseWithError(errFactHeaderRead)
	gitErr := <-done
	var readErr headReadError
	switch {
	case errors.As(err, &readErr):
		return BlobImage{}, nil, counted.n, errors.Join(readErr.err, gitErr)
	case err != nil:
		return BlobImage{}, nil, counted.n, err
	case ctx.Err() != nil:
		return BlobImage{}, nil, counted.n, ctx.Err()
	}
	image.OID = oid
	if !image.LFS {
		head = nil
	}
	return image, head, counted.n, nil
}

// headReadError is a classification that could not read its first bytes.
type headReadError struct{ err error }

func (e headReadError) Error() string { return e.err.Error() }

// classifyImageReader classifies size bytes that r reads as an image from
// their header, reading at most factImageHeaderMax of them: the image's
// size, type and pixel size, LFS for a Git LFS pointer, neither for bytes
// that are no image, or an error when no header is found within the
// bound. It answers the first bytes it read (the whole of a pointer).
func classifyImageReader(r *countingReader, size int64) (BlobImage, []byte, error) {
	head := make([]byte, min(size, factImageHead))
	if _, err := io.ReadFull(r, head); err != nil {
		return BlobImage{}, nil, headReadError{err}
	}
	image := BlobImage{Size: size}
	mime, displaySize, err := classifyDisplayImage(head, io.MultiReader(bytes.NewReader(head), io.LimitReader(r, factImageHeaderMax-int64(len(head)))))
	switch {
	case err == nil:
		image.MIME, image.Width, image.Height, image.Fluid = mime, displaySize.Width, displaySize.Height, displaySize.Fluid
	case errors.Is(err, ErrLFSPointer):
		image.LFS = true
	case errors.Is(err, ErrNotDisplayImage) && r.n < factImageHeaderMax:
	default:
		return BlobImage{}, nil, fmt.Errorf("no image header in the first %d bytes", factImageHeaderMax)
	}
	return image, head, nil
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
// blob is binary (a NUL in its first factBinaryProbe bytes). Its cost is
// one newline scan of the blob up to the last range's end, plus one visit
// per range for each line segment it captures: ranges are taken in order
// of their start, and only those holding the current line are visited.
type linesSink struct {
	ranges []*lineRange
	// next is the first range of ranges, sorted by start once the stream
	// starts, not yet reached; active are the ranges holding the line.
	next   int
	active []*lineRange
	line   int
	seen   int64
	last   byte
	binary bool
	// started is set by the first write; passed once every range ended
	// at a newline within the blob, after which lines are not counted.
	started, passed bool
	// visits counts range visits, for tests.
	visits int
}

func (s *linesSink) write(p []byte) {
	if !s.started {
		s.started, s.line = true, 1
		slices.SortFunc(s.ranges, func(a, b *lineRange) int { return cmp.Compare(a.start, b.start) })
	}
	if s.seen < factBinaryProbe {
		probe := p[:min(int64(len(p)), factBinaryProbe-s.seen)]
		if bytes.IndexByte(probe, 0) >= 0 {
			s.binary = true
		}
	}
	s.seen += int64(len(p))
	for len(p) > 0 && !s.passed {
		for s.next < len(s.ranges) && s.ranges[s.next].start == s.line {
			s.active = append(s.active, s.ranges[s.next])
			s.next++
		}
		i := bytes.IndexByte(p, '\n')
		segment := p
		if i >= 0 {
			segment = p[:i+1]
		}
		kept := s.active[:0]
		for _, r := range s.active {
			s.visits++
			if len(r.buf)+len(segment) > maxFactLinesBytes+1 {
				r.tooLarge = true
				r.buf = nil
				continue
			}
			r.buf = append(r.buf, segment...)
			if i < 0 || r.end > s.line {
				kept = append(kept, r)
			}
		}
		clear(s.active[len(kept):])
		s.active = kept
		s.last = segment[len(segment)-1]
		if i < 0 {
			break
		}
		s.line++
		p = p[i+1:]
		s.passed = s.next == len(s.ranges) && len(s.active) == 0
	}
}

// text is r's lines, or false when the blob is binary or the range is
// past its end or over the text cap.
func (s *linesSink) text(r *lineRange) (string, bool) {
	total := s.line
	if s.last == '\n' {
		total--
	}
	if s.binary || r.tooLarge || (!s.passed && r.end > total) {
		return "", false
	}
	text := strings.ReplaceAll(strings.TrimSuffix(string(r.buf), "\n"), "\r\n", "\n")
	text = strings.TrimSuffix(text, "\r")
	if len(text) > maxFactLinesBytes || strings.IndexByte(text, 0) >= 0 {
		return "", false
	}
	return text, true
}

// BlobImage answers blob oid of cwd's repository as an image, without the
// network: what the blob is from the blob image cache, or else its size
// and type (one `git cat-file --batch-check`) and its header
// (readBlobImage). A Git LFS pointer answers as its object in the local
// LFS store as that store is now (lfsBlobImage: one lstat). An oid that is
// no full object id, no local blob, or one over
// attachment.DisplayImageMaxBytes is an error naming none of its content.
func (c *Core) BlobImage(ctx context.Context, cwd, oid string) (BlobImage, error) {
	if !gitdiff.IsFullOID(oid) {
		return BlobImage{}, errors.New("not a full object ID")
	}
	ctx, cancel := context.WithTimeout(ctx, localFactsTimeout)
	defer cancel()
	lfs := c.lfsStore(ctx, cwd)
	if record, ok := c.blobImages.get(oid, c.nowFn()); ok {
		image, _, err := resolveBlobImage(record, lfs)
		return image, err
	}
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
	image, _, err := c.blobImage(ctx, cwd, oid, obj.size, lfs)
	return image, err
}

var (
	// ErrNoLocalBlob is an object id that names no blob present in the
	// repository.
	ErrNoLocalBlob = errors.New("no such blob in this repository")
	// ErrBlobTooLarge is a blob over the display image cap.
	ErrBlobTooLarge = errors.New("the blob is larger than an image shown")
)

// ReadImage fills buf with the bytes image, an answer of BlobImage for
// cwd's repository, shows: exactly image.Size bytes, or an error. A blob's
// bytes come from git, whose output goes into buf and never past it; a
// Git LFS pointer's from its local object, verified against the pointer's
// sha256 as it is read (ErrLFSObjectMismatch). Both under ctx, without the
// network.
func (c *Core) ReadImage(ctx context.Context, cwd string, image BlobImage, buf []byte) error {
	if int64(len(buf)) != image.Size {
		return fmt.Errorf("a %d byte buffer for a %d byte image", len(buf), image.Size)
	}
	if image.lfsOID != "" {
		return c.readLFSObject(ctx, image, buf)
	}
	return c.readBlob(ctx, cwd, image.OID, buf)
}

// readBlob fills buf with blob oid of cwd's repository: exactly len(buf)
// bytes, or an error.
func (c *Core) readBlob(ctx context.Context, cwd, oid string, buf []byte) error {
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
