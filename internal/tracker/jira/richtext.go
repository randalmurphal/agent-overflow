package jira

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"agent-overflow/internal/tracker/adf"
)

// pendingPrefix starts the media ID a document holds for an image not
// uploaded yet.
const pendingPrefix = "pending-upload:"

// uploadFraming is the room an upload body keeps for its multipart
// boundaries and headers.
const uploadFraming = 4 << 10

// Image is a local image a Markdown body embeds, read and checked when
// planned and uploaded to the issue when applied.
type Image struct {
	// Ref is the reference as the Markdown first wrote it; Name the file
	// name it is attached under.
	Ref  string
	Name string
	Size int64

	path        string // the name inside the image folder, normalized
	sum         [sha256.Size]byte
	placeholder string
}

// imageStage collects the images one write embeds. They are read from
// dir through an *os.Root, which refuses a name that leaves the folder,
// through ".." or a symbolic link alike; the root is opened for each
// pass over the images and closed before it returns, so no descriptor
// outlives a call. A plan keeps the folder's path and each image's hash,
// and its apply reads every image again before its first write. Together
// the images of one write hold at most limit bytes, the upload limit
// less its multipart framing, which bounds what an apply holds in memory.
type imageStage struct {
	dir    string
	limit  int64
	total  int64
	byPath map[string]*Image
	list   []*Image
}

func newImageStage(dir string, maxUpload int64) *imageStage {
	return &imageStage{dir: dir, limit: maxUpload - uploadFraming, byPath: map[string]*Image{}}
}

// imagePath normalizes ref to a name inside the image folder, so every
// spelling of one file is one image: one leading "/" means the folder,
// and the rest is cleaned. A name that leaves the folder is refused.
func imagePath(ref string) (string, error) {
	name := path.Clean(strings.TrimPrefix(ref, "/"))
	if !fs.ValidPath(name) || name == "." {
		return "", fmt.Errorf("image %s: not a path inside the image folder", ref)
	}
	return name, nil
}

// openRoot opens the image folder, which must be an absolute path.
func (s *imageStage) openRoot() (*os.Root, error) {
	if s.dir == "" {
		return nil, errors.New("no folder to read local images from")
	}
	if !filepath.IsAbs(s.dir) {
		return nil, fmt.Errorf("the image folder %s is not an absolute path", s.dir)
	}
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		return nil, fmt.Errorf("the image folder: %w", err)
	}
	return root, nil
}

// stage reads the image ref names and checks it is an image within the
// limit; one file is staged once however it is spelled.
func (s *imageStage) stage(ctx context.Context, ref string) (*Image, error) {
	name, err := imagePath(ref)
	if err != nil {
		return nil, err
	}
	if img, ok := s.byPath[name]; ok {
		return img, nil
	}
	root, err := s.openRoot()
	if err != nil {
		return nil, fmt.Errorf("image %s: %w", ref, err)
	}
	defer func() { _ = root.Close() }()
	content, err := readImage(ctx, root, name, s.limit-s.total)
	if err != nil {
		return nil, fmt.Errorf("image %s: %w", ref, err)
	}
	if kind := http.DetectContentType(content); !strings.HasPrefix(kind, "image/") {
		return nil, fmt.Errorf("%s is %s, not an image; attach other files with Attach", ref, kind)
	}
	img := &Image{
		Ref: ref, Name: path.Base(name), Size: int64(len(content)), path: name,
		sum: sha256.Sum256(content), placeholder: pendingPrefix + strconv.Itoa(len(s.list)),
	}
	s.total += img.Size
	s.byPath[name] = img
	s.list = append(s.list, img)
	return img, nil
}

// readImage reads name from root whole, refusing more than limit bytes.
func readImage(ctx context.Context, root *os.Root, name string, limit int64) ([]byte, error) {
	f, err := root.Open(filepath.FromSlash(name))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, io.LimitReader(ctxReader{ctx, f}, limit+1)); err != nil {
		return nil, err
	}
	if int64(buf.Len()) > limit {
		return nil, fmt.Errorf("the images of one write are over the %d-byte upload limit", limit)
	}
	return buf.Bytes(), nil
}

// hold reads imgs again, before the first write of the call that uploads
// them, and returns their content by image. An image that changed since
// it was staged is a *StaleError; any failure is the call's refusal
// before it sent anything.
func (s *imageStage) hold(ctx context.Context, imgs []*Image) (map[*Image][]byte, error) {
	if len(imgs) == 0 {
		return nil, nil
	}
	root, err := s.openRoot()
	if err != nil {
		return nil, notSentErr(err)
	}
	defer func() { _ = root.Close() }()
	out := make(map[*Image][]byte, len(imgs))
	for _, img := range imgs {
		content, err := readImage(ctx, root, img.path, img.Size)
		switch {
		case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
			return nil, err
		case err != nil || sha256.Sum256(content) != img.sum:
			return nil, notSentErr(&StaleError{What: "image " + img.Ref})
		}
		out[img] = content
	}
	return out, nil
}

// ctxReader stops reading once ctx ends.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (r ctxReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// markdownResolver serves a conversion's mentions and images: a mention
// resolves to one user (adf passes "me" for the signed-in user); an image
// is staged and named by a placeholder until it is uploaded.
type markdownResolver struct{ b *builder }

func (r markdownResolver) Mention(ctx context.Context, who string) (string, string, error) {
	var u User
	var err error
	if who == "me" {
		u, err = r.b.c.Myself(ctx)
	} else {
		u, err = r.b.c.FindUser(ctx, who)
	}
	if err != nil {
		return "", "", err
	}
	return u.AccountID, u.DisplayName, nil
}

func (r markdownResolver) Upload(ctx context.Context, ref string) (string, error) {
	img, err := r.b.stage.stage(ctx, ref)
	if err != nil {
		return "", err
	}
	return img.placeholder, nil
}

// buildRich converts Markdown: an edit of the stored document when there
// is one, so what Markdown cannot show survives where the text is
// unchanged, and a new document otherwise.
func buildRich(ctx context.Context, b *builder, f FieldMeta, in Input, v *Value) (any, error) {
	base := b.opts.Base
	if in.from != nil && !adf.Equal(in.from, base) {
		return nil, &StaleError{Issue: b.opts.Issue, What: f.Name}
	}
	doc, ws, err := b.markdown(ctx, in.texts[0], base)
	if err != nil {
		return nil, fmt.Errorf("jira: %s: %w", f.Name, err)
	}
	v.rich, v.doc, v.base, v.edits = true, doc, base, ws
	for _, w := range ws {
		v.Warnings = append(v.Warnings, f.Name+": "+w.String())
	}
	v.imgs = b.stage.in(doc)
	v.Images = imageCopies(v.imgs)
	v.Unchanged = base != nil && adf.Equal(doc, base)
	v.Display = adf.ToMarkdown(doc)
	if v.Fields, err = json.Marshal(doc); err != nil {
		return nil, fmt.Errorf("jira: %s: encode the document: %w", f.Name, err)
	}
	return nil, nil
}

// errBlank is Markdown that converts to a document showing nothing.
var errBlank = errors.New("the Markdown converts to nothing; Clear empties a field and DeleteComment removes a comment")

// markdown converts md, editing base when it is set. A document that
// shows nothing is refused.
func (b *builder) markdown(ctx context.Context, md string, base *adf.Node) (*adf.Node, []adf.Warning, error) {
	res := markdownResolver{b: b}
	var doc *adf.Node
	var ws []adf.Warning
	var err error
	if base == nil {
		doc, ws, err = adf.FromMarkdown(ctx, md, res)
	} else {
		doc, ws, err = adf.Edit(ctx, base, md, res)
	}
	if err != nil {
		return nil, nil, err
	}
	if adf.Blank(doc) {
		return nil, nil, errBlank
	}
	return doc, ws, nil
}

// imageCopies are copies of imgs, for a caller to keep.
func imageCopies(imgs []*Image) []Image {
	if len(imgs) == 0 {
		return nil
	}
	out := make([]Image, len(imgs))
	for i, img := range imgs {
		out[i] = *img
	}
	return out
}

// in lists the staged images doc names, in the order staged.
func (s *imageStage) in(doc *adf.Node) []*Image {
	named := map[string]bool{}
	walkMedia(doc, func(n *adf.Node) {
		if id, _ := n.Attrs["id"].(string); strings.HasPrefix(id, pendingPrefix) {
			named[id] = true
		}
	})
	var out []*Image
	for _, img := range s.list {
		if named[img.placeholder] {
			out = append(out, img)
		}
	}
	return out
}

// walkMedia calls fn on every media node under n.
func walkMedia(n *adf.Node, fn func(*adf.Node)) {
	if n == nil {
		return
	}
	if n.Type == "media" {
		fn(n)
	}
	for _, c := range n.Content {
		walkMedia(c, fn)
	}
}

// copyDoc is a deep copy of doc, so a write can patch media IDs without
// touching a plan or the stored document it shares nodes with.
func copyDoc(doc *adf.Node) (*adf.Node, error) {
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	var out adf.Node
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// withoutPending is a copy of doc with each image not uploaded yet
// written as a paragraph naming its file, for a create that must exist
// before its images can be attached.
func withoutPending(doc *adf.Node, images []*Image) (*adf.Node, error) {
	out, err := copyDoc(doc)
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	for _, img := range images {
		names[img.placeholder] = img.Name
	}
	replacePending(out, names)
	return out, nil
}

// replacePending swaps each mediaSingle holding a pending image for a
// paragraph naming the file.
func replacePending(n *adf.Node, names map[string]string) {
	for i, c := range n.Content {
		if c == nil {
			continue
		}
		if c.Type == "mediaSingle" {
			for _, m := range c.Content {
				id, _ := m.Attrs["id"].(string)
				if name, ok := names[id]; ok {
					n.Content[i] = &adf.Node{Type: "paragraph", Content: []*adf.Node{{Type: "text", Text: "[image: " + name + "]"}}}
					break
				}
			}
			continue
		}
		replacePending(c, names)
	}
}
