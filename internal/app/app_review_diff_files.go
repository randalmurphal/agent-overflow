package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"agent-overflow/internal/attachment"
	"agent-overflow/internal/contentcache"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/gitdiff"
	"agent-overflow/internal/transport"
)

// The files of a git-backed review diff, read at the two revisions the diff
// compares: their sizes for every binary file the patch names, and the image
// bytes of a file the pane renders. The caller restates which diff it shows
// (ReviewDiffSource) and names a file by what its patch printed; every read
// recomputes the diff's sides and serves only blobs that diff changes, at
// the object IDs the patch's index line named.

// ReviewDiffSource names the diff a file read belongs to, in the terms the
// Open*Diff call that read it took.
type ReviewDiffSource struct {
	// Scope is "workspace" (OpenWorkspaceDiff), "branch"
	// (OpenBranchBaseDiff), "commit" (OpenCommitDiff) or "pr" (OpenPRDiff,
	// OpenPRCommitDiff).
	Scope string `json:"scope"`
	// BaseBranch is the branch scope's base.
	BaseBranch string `json:"baseBranch,omitempty"`
	// CommitSHA is the commit scope's commit.
	CommitSHA string `json:"commitSha,omitempty"`
	// Revision is the pr scope's pinned revision.
	Revision *gitops.PRRevision `json:"revision,omitempty"`
	// FirstSHA and LastSHA are the pr scope's commit run (OpenPRCommitDiff);
	// both empty is the whole PR (OpenPRDiff).
	FirstSHA string `json:"firstSha,omitempty"`
	LastSHA  string `json:"lastSha,omitempty"`
}

// ReviewDiffFileRef names one file of a review diff by its patch: its
// paths, and the two abbreviated blob IDs of its `index <old>..<new>` line,
// all zeros for a side that does not exist. A file whose patch has no index
// line (a pure rename, a mode change) leaves both IDs empty; its two sides
// then must hold the same blob.
type ReviewDiffFileRef struct {
	// Path is the file's path on the new side, or a deleted file's path.
	Path string `json:"path"`
	// OldPath is a renamed file's old path; empty when it is Path.
	OldPath string `json:"oldPath,omitempty"`
	OldOID  string `json:"oldOid"`
	NewOID  string `json:"newOid"`
}

// ReviewDiffFileSide is one side of a review diff file.
type ReviewDiffFileSide struct {
	Exists bool  `json:"exists"`
	Size   int64 `json:"size"`
}

// ReviewDiffFileSizes answers one ReviewDiffFileRef. Error is set, and the
// sides zero, when this file could not be read; the other files of the
// same request are still answered.
type ReviewDiffFileSizes struct {
	Path    string             `json:"path"`
	OldPath string             `json:"oldPath,omitempty"`
	Old     ReviewDiffFileSide `json:"old"`
	New     ReviewDiffFileSide `json:"new"`
	Error   string             `json:"error,omitempty"`
}

// ReviewDiffImageSide is one side of a review diff image.
type ReviewDiffImageSide struct {
	Exists bool  `json:"exists"`
	Size   int64 `json:"size"`
	// URL is the relative, single-use, ticketed URL of the bytes
	// (internal/transport/localimageroutes.go); empty when the side does
	// not exist, is over the cap, or is not an image.
	URL      string `json:"url,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	// OverCap is set when the side is larger than MaxBytes: no bytes are
	// read.
	OverCap bool `json:"overCap,omitempty"`
	// NotImage is set when the bytes are not an image a browser displays.
	NotImage bool `json:"notImage,omitempty"`
}

// ReviewDiffImage answers GetReviewDiffImage: both sides of one file.
type ReviewDiffImage struct {
	Old ReviewDiffImageSide `json:"old"`
	New ReviewDiffImageSide `json:"new"`
	// MaxBytes is the per-side cap the answer was made under.
	MaxBytes int64 `json:"maxBytes"`
}

// reviewDiffImageMaxBytes caps one side's bytes: the cap a markdown image
// read for display has.
const reviewDiffImageMaxBytes = attachment.DisplayImageMaxBytes

// maxReviewDiffFileRefs bounds one GetReviewDiffFileSizes call; each file
// names at most two paths, which ride one git argument list.
const maxReviewDiffFileRefs = gitdiff.MaxChangedPaths / 2

var abbreviatedOIDPattern = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// GetReviewDiffFileSizes answers the byte size and existence of each side of
// the named files of a git-backed review diff, at most 100 per call. A
// failure to resolve the diff fails the call; a file the diff does not
// change, or whose blobs are not the ones its patch named, is answered with
// its Error.
//
//ao:scope files:read
func (a *App) GetReviewDiffFileSizes(ctx context.Context, ws WorkspaceRef, source ReviewDiffSource, files []ReviewDiffFileRef) ([]ReviewDiffFileSizes, error) {
	const action = "read review diff file sizes"
	if len(files) > maxReviewDiffFileRefs {
		return nil, fmt.Errorf("%s: %d files exceeds %d", action, len(files), maxReviewDiffFileRefs)
	}
	sides, err := a.reviewDiffSides(ctx, action, ws, source)
	if err != nil {
		return nil, err
	}
	defer func() { logReviewDiffCloseError(sides.Close()) }()
	paths := make([]string, 0, 2*len(files))
	for _, file := range files {
		paths = append(paths, file.Path)
		if file.OldPath != "" && file.OldPath != file.Path {
			paths = append(paths, file.OldPath)
		}
	}
	changed, err := sides.ChangedPaths(ctx, paths)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	answers := make([]ReviewDiffFileSizes, 0, len(files))
	for _, file := range files {
		answer := ReviewDiffFileSizes{Path: file.Path, OldPath: file.OldPath}
		old, current, err := reviewDiffFileBlobs(changed, file)
		if err != nil {
			answer.Error = err.Error()
		} else {
			answer.Old = ReviewDiffFileSide{Exists: old.Exists, Size: old.Size}
			answer.New = ReviewDiffFileSide{Exists: current.Exists, Size: current.Size}
		}
		answers = append(answers, answer)
	}
	return answers, nil
}

// GetReviewDiffImage reads both sides of one file of a git-backed review
// diff for display and mints a single-use URL for each side that is an
// image within the cap. A file the diff does not change, or whose blobs are
// not the ones its patch named, is refused. Pixel sizes are the client's to
// measure.
//
//ao:scope files:read
func (a *App) GetReviewDiffImage(ctx context.Context, ws WorkspaceRef, source ReviewDiffSource, file ReviewDiffFileRef) (ReviewDiffImage, error) {
	const action = "read review diff image"
	sides, err := a.reviewDiffSides(ctx, action, ws, source)
	if err != nil {
		return ReviewDiffImage{}, err
	}
	defer func() { logReviewDiffCloseError(sides.Close()) }()
	paths := []string{file.Path}
	if file.OldPath != "" && file.OldPath != file.Path {
		paths = append(paths, file.OldPath)
	}
	changed, err := sides.ChangedPaths(ctx, paths)
	if err != nil {
		return ReviewDiffImage{}, fmt.Errorf("%s: %w", action, err)
	}
	old, current, err := reviewDiffFileBlobs(changed, file)
	if err != nil {
		return ReviewDiffImage{}, fmt.Errorf("%s: %w", action, err)
	}
	answer := ReviewDiffImage{MaxBytes: reviewDiffImageMaxBytes}
	for _, side := range []struct {
		blob gitdiff.Blob
		into *ReviewDiffImageSide
	}{{old, &answer.Old}, {current, &answer.New}} {
		*side.into, err = a.reviewDiffImageSide(ctx, sides, side.blob)
		if err != nil {
			return ReviewDiffImage{}, fmt.Errorf("%s: %w", action, err)
		}
	}
	return answer, nil
}

// GetEditDiffImage reads the new side of one file of an edits diff for
// display: the snapshot persisted with the edit, or the thread's workspace
// file, served only when it matches the file's patch as context expansion
// requires (GetEditDiffContextLines). An edit records no old side, and an
// edit diff is text, so this serves an SVG's rendering; the answer's old
// side never exists.
//
//ao:scope files:read
func (a *App) GetEditDiffImage(threadID string, req DiffContextRequest) (ReviewDiffImage, error) {
	const action = "read edit diff image"
	if a.shuttingDown.Load() {
		return ReviewDiffImage{}, ErrShuttingDown
	}
	if req.Scope != "edits" {
		return ReviewDiffImage{}, fmt.Errorf("%s: scope %q is not an edits scope", action, req.Scope)
	}
	workspace, err := a.threadDiffWorkspace(action, threadID)
	if err != nil {
		return ReviewDiffImage{}, err
	}
	// Read past the cap so an over-cap file answers its size; past this
	// the read itself refuses.
	content, _, err := a.diffContextContent(action, workspace, threadID, req, 4*reviewDiffImageMaxBytes)
	if err != nil {
		return ReviewDiffImage{}, err
	}
	// No dedupe key: the same path holds other content at another edit.
	side, err := a.reviewDiffImageBytes([]byte(content), "")
	if err != nil {
		return ReviewDiffImage{}, fmt.Errorf("%s: %w", action, err)
	}
	return ReviewDiffImage{New: side, MaxBytes: reviewDiffImageMaxBytes}, nil
}

// reviewDiffSides resolves the two sides of the diff source names, the way
// its Open*Diff call does. A pr source reads objects already present: the
// Open call fetched them.
func (a *App) reviewDiffSides(ctx context.Context, action string, ws WorkspaceRef, source ReviewDiffSource) (*gitdiff.Sides, error) {
	if a.shuttingDown.Load() {
		return nil, ErrShuttingDown
	}
	workspace, err := a.reviewWorkspace(action, ws)
	if err != nil {
		return nil, err
	}
	if !gitdiff.IsGitRepository(ctx, workspace) {
		return nil, fmt.Errorf("%s: workspace is not a git repository", action)
	}
	var sides *gitdiff.Sides
	switch source.Scope {
	case "workspace":
		root, rootErr := a.reviewDiffSnapshotRoot()
		if rootErr != nil {
			return nil, fmt.Errorf("%s: %w", action, rootErr)
		}
		sides, err = gitdiff.WorktreeSides(ctx, workspace, root)
	case "branch":
		if strings.TrimSpace(source.BaseBranch) == "" {
			return nil, fmt.Errorf("%s: base branch is required", action)
		}
		root, rootErr := a.reviewDiffSnapshotRoot()
		if rootErr != nil {
			return nil, fmt.Errorf("%s: %w", action, rootErr)
		}
		sides, err = gitdiff.BranchBaseSides(ctx, workspace, root, source.BaseBranch)
	case "commit":
		sides, err = gitdiff.CommitSides(ctx, workspace, source.CommitSHA)
	case "pr":
		if source.Revision == nil {
			return nil, fmt.Errorf("%s: the PR revision is required", action)
		}
		rev := *source.Revision
		if err := rev.Validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", action, err)
		}
		switch {
		case source.FirstSHA == "" && source.LastSHA == "":
			sides, err = gitdiff.MergeBaseSides(ctx, workspace, rev.BaseSHA, rev.HeadSHA)
		case source.FirstSHA == "" || source.LastSHA == "":
			return nil, fmt.Errorf("%s: a commit range needs both ends", action)
		default:
			sides, err = gitdiff.CommitRangeSides(ctx, workspace, rev.BaseSHA, rev.HeadSHA, source.FirstSHA, source.LastSHA)
		}
	default:
		return nil, fmt.Errorf("%s: %q is not a git review scope", action, source.Scope)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	return sides, nil
}

// reviewDiffFileBlobs validates file against the paths the diff changes and
// answers its two sides: the old side at OldPath (or Path), the new side at
// Path, each one the diff changes, holding the blobs the patch's index line
// named.
func reviewDiffFileBlobs(changed map[string]gitdiff.ChangedPath, file ReviewDiffFileRef) (old, current gitdiff.Blob, err error) {
	oldPath := file.OldPath
	if oldPath == "" {
		oldPath = file.Path
	}
	oldEntry, oldChanged := changed[oldPath]
	newEntry, newChanged := changed[file.Path]
	if !oldChanged || !newChanged {
		return old, current, fmt.Errorf("%s is not a file this diff changes", file.Path)
	}
	old, current = oldEntry.Old, newEntry.New
	if !old.Exists && !current.Exists {
		return old, current, fmt.Errorf("%s is not a file this diff changes", file.Path)
	}
	if file.OldOID == "" && file.NewOID == "" {
		// No index line: a pure rename or a mode change, the same content
		// on both sides.
		if !old.Exists || !current.Exists || old.OID != current.OID {
			return old, current, fmt.Errorf("%s changed since the diff was read", file.Path)
		}
		return old, current, nil
	}
	for _, side := range []struct {
		blob gitdiff.Blob
		want string
	}{{old, file.OldOID}, {current, file.NewOID}} {
		if !abbreviatedOIDPattern.MatchString(side.want) {
			return old, current, fmt.Errorf("%s: %q is not an abbreviated object ID", file.Path, side.want)
		}
		absent := strings.Trim(side.want, "0") == ""
		if absent == side.blob.Exists || (!absent && !strings.HasPrefix(side.blob.OID, side.want)) {
			return old, current, fmt.Errorf("%s changed since the diff was read", file.Path)
		}
	}
	return old, current, nil
}

// reviewDiffImageSide reads one side for display. A blob's bytes are cached
// by object ID, which names the same bytes in every repository; a caller
// reaches the entry only through a diff that holds the blob.
func (a *App) reviewDiffImageSide(ctx context.Context, sides *gitdiff.Sides, blob gitdiff.Blob) (ReviewDiffImageSide, error) {
	if !blob.Exists {
		return ReviewDiffImageSide{}, nil
	}
	if !blob.IsFile() {
		return ReviewDiffImageSide{Exists: true, Size: blob.Size, NotImage: true}, nil
	}
	if blob.Size > reviewDiffImageMaxBytes {
		return ReviewDiffImageSide{Exists: true, Size: blob.Size, OverCap: true}, nil
	}
	key := "blob\x00" + blob.OID
	if held, ok := a.reviewDiffImages().cache.Lookup(key); ok {
		return a.mintReviewDiffImage(held)
	}
	data, err := sides.ReadBlob(ctx, blob, reviewDiffImageMaxBytes)
	if err != nil {
		return ReviewDiffImageSide{}, err
	}
	return a.reviewDiffImageBytes(data, key)
}

// reviewDiffImageBytes classifies bytes read for display, caches an image
// under key (none when empty), and mints its URL.
func (a *App) reviewDiffImageBytes(data []byte, key string) (ReviewDiffImageSide, error) {
	size := int64(len(data))
	if size > reviewDiffImageMaxBytes {
		return ReviewDiffImageSide{Exists: true, Size: size, OverCap: true}, nil
	}
	mime, err := attachment.DetectDisplayImageMIME(data)
	if err != nil {
		return ReviewDiffImageSide{Exists: true, Size: size, NotImage: true}, nil
	}
	if _, _, err := attachment.ValidateDisplayImage(data, mime); err != nil {
		return ReviewDiffImageSide{Exists: true, Size: size, NotImage: true}, nil
	}
	held, err := a.reviewDiffImages().cache.Put(key, reviewDiffImage{data: data, mime: mime})
	if err != nil {
		return ReviewDiffImageSide{}, err
	}
	return a.mintReviewDiffImage(held)
}

func (a *App) mintReviewDiffImage(held contentcache.Entry[reviewDiffImage]) (ReviewDiffImageSide, error) {
	server := a.transportServer.Load()
	if server == nil {
		return ReviewDiffImageSide{}, errors.New("transport is not serving")
	}
	url, err := server.MintLocalImageTicket(held.ID)
	if err != nil {
		return ReviewDiffImageSide{}, err
	}
	return ReviewDiffImageSide{
		Exists:   true,
		Size:     int64(len(held.Value.data)),
		URL:      url,
		MimeType: held.Value.mime,
	}, nil
}

// reviewDiffImage is one cached side's bytes and the image type sniffed
// from them.
type reviewDiffImage struct {
	data []byte
	mime string
}

// reviewDiffImageCache holds review diff image bytes between the bound
// method that read them and the local-image byte route that serves them.
type reviewDiffImageCache struct {
	cache *contentcache.Cache[reviewDiffImage]
}

// Bounds of the review diff image cache: room for a pane's images at the
// per-side cap, and long enough for a pane that stays open.
const (
	reviewDiffImageCacheBytes = 4 * reviewDiffImageMaxBytes
	reviewDiffImageCacheTTL   = 10 * time.Minute
)

func (a *App) reviewDiffImages() *reviewDiffImageCache {
	a.reviewDiffImageOnce.Do(func() {
		a.reviewDiffImageCache = &reviewDiffImageCache{cache: contentcache.New(contentcache.Config[reviewDiffImage]{
			MaxBytes: reviewDiffImageCacheBytes,
			TTL:      reviewDiffImageCacheTTL,
			Size:     func(image reviewDiffImage) int64 { return int64(len(image.data)) },
		})}
	})
	return a.reviewDiffImageCache
}

// open resolves a content id the cache minted, for the byte route.
func (c *reviewDiffImageCache) open(contentID string) (transport.LocalImageContent, bool) {
	held, ok := c.cache.Get(contentID)
	if !ok {
		return transport.LocalImageContent{}, false
	}
	return transport.LocalImageContent{
		MimeType: held.Value.mime,
		ModTime:  held.StoredAt,
		Content:  nopCloserReader{bytes.NewReader(held.Value.data)},
	}, true
}
