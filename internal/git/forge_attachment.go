package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"agent-overflow/internal/forgeapi"
	"agent-overflow/internal/forgeattach"
)

// Forge-hosted attachment downloads.
//
// A PR body or a review comment references media the forge holds behind
// the same login the browser uses: GitLab's /uploads/<secret>/<name>,
// GitHub's user-attachments assets. The user's own gh / glab login is
// the credential, and each fetch is a forge API transport request
// (Request.Attachment): GitHub's for the absolute URL, whose token goes
// only to the forge's own hosts, GitLab's for the project's uploads path
// on its REST API.
//
// A GitLab upload path carries the file's 32-hex secret and a GitHub
// asset URL can redirect through a signed one. The transport redacts a
// signed query from the URLs it reports but keeps the path, so a GitLab
// fetch's error has its request elided before the review pane shows it.

// forgeAttachmentTimeout bounds one download. The transport default (45s)
// is sized for a metadata call; a video at the transport's minimum
// sustained transfer rate needs minutes, and cutting it at 45s would read
// as a broken forge rather than a slow one.
const forgeAttachmentTimeout = 10 * time.Minute

// FetchAttachment resolves one attachment reference found in a PR/MR and
// downloads its bytes through the owning forge's login.
//
// The reference is parsed BEFORE anything is dispatched, so a href that
// is not a forge attachment costs no request.
func (c *Core) FetchAttachment(ctx context.Context, ref PRReference, href string, maxBytes int64) (data []byte, filename string, err error) {
	if err := ref.Validate(); err != nil {
		return nil, "", err
	}
	target, err := forgeattach.ParseReference(ref.Forge, ref.Project(), href)
	if err != nil {
		return nil, "", err
	}
	data, err = c.ForgeByID(ref.Forge).FetchAttachment(ctx, ref, target, maxBytes)
	if err != nil {
		return nil, "", err
	}
	return data, target.Filename, nil
}

func (f *gitlabForge) FetchAttachment(ctx context.Context, ref PRReference, target forgeattach.Target, maxBytes int64) ([]byte, error) {
	if target.Forge != "gitlab" || target.Request == "" || strings.Contains(target.Request, "://") {
		return nil, errors.New("attachment reference is not a GitLab upload")
	}
	client, err := f.core.gitlabAPI(ref.Host)
	if err != nil {
		return nil, err
	}
	var body bytes.Buffer
	_, err = client.Stream(ctx, forgeapi.Request{
		Path:       target.Request,
		Attachment: true,
		Header:     http.Header{"Accept": {"*/*"}},
		Timeout:    forgeAttachmentTimeout,
	}, &body, maxBytes)
	if errors.Is(err, forgeapi.ErrBodyTooLarge) {
		return nil, attachmentTooLargeError(maxBytes)
	}
	if err != nil {
		return nil, redactGitLabUpload(err, target.Request)
	}
	return body.Bytes(), nil
}

func (f *githubForge) FetchAttachment(ctx context.Context, ref PRReference, target forgeattach.Target, maxBytes int64) ([]byte, error) {
	if target.Forge != "github" || !strings.Contains(target.Request, "://") {
		return nil, errors.New("attachment reference is not a GitHub attachment URL")
	}
	client, err := f.core.githubAPI(ref.Host)
	if err != nil {
		return nil, err
	}
	// The absolute URL is requested as given. The token goes to github.com
	// only (forgeattach admits github.com attachment hosts, and the client
	// authorizes its own hosts); the signed asset host a github.com URL
	// redirects to carries its own admission in the query and gets no
	// token.
	var body bytes.Buffer
	_, err = client.Stream(ctx, forgeapi.Request{
		Path:       target.Request,
		Attachment: true,
		Header:     http.Header{"Accept": {"*/*"}},
		Timeout:    forgeAttachmentTimeout,
	}, &body, maxBytes)
	if errors.Is(err, forgeapi.ErrBodyTooLarge) {
		return nil, attachmentTooLargeError(maxBytes)
	}
	if err != nil {
		return nil, err
	}
	return body.Bytes(), nil
}

func (nullForge) FetchAttachment(context.Context, PRReference, forgeattach.Target, int64) ([]byte, error) {
	return nil, ErrUnsupportedForge
}

func attachmentTooLargeError(maxBytes int64) error {
	if maxBytes >= 1<<20 {
		return fmt.Errorf("attachment is larger than %d MiB", maxBytes>>20)
	}
	return fmt.Errorf("attachment is larger than %d bytes", maxBytes)
}

// redactedError is a failure whose message has a secret taken out of it
// and whose cause keeps it, for errors.Is and errors.As.
type redactedError struct {
	message string
	err     error
}

func (e *redactedError) Error() string { return e.message }
func (e *redactedError) Unwrap() error { return e.err }

// redactGitLabUpload elides a GitLab upload request, and its secret in
// any spelling the message carries it, from err's message. An error that
// does not carry it is returned as it is.
func redactGitLabUpload(err error, request string) error {
	const elision = "<attachment>"
	message := err.Error()
	redacted := strings.ReplaceAll(message, request, elision)
	segments := strings.Split(request, "/")
	if len(segments) >= 2 {
		if secret := segments[len(segments)-2]; secret != "" {
			redacted = strings.ReplaceAll(redacted, secret, elision)
		}
	}
	if redacted == message {
		return err
	}
	return &redactedError{message: redacted, err: err}
}
