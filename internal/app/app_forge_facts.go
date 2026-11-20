package app

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"agent-overflow/internal/attachment"
	gitops "agent-overflow/internal/git"
)

// ResolveForgeFacts answers the facts a PR's bodies and comments name
// (gitops.Core.ResolveFacts): the forge kinds from pr's forge as
// background requests, the local kinds from ws's repository at rev
// without the network. rev may be zero. One fact per key, in keys'
// order; a fact nobody could read is unavailable, and the call fails only
// on arguments it cannot run with (more than 256 keys, an invalid PR or
// revision, a workspace that does not resolve).
//
//ao:scope git:operate
func (a *App) ResolveForgeFacts(ctx context.Context, ws WorkspaceRef, pr gitops.PRReference, rev gitops.PRRevision, keys []string) (gitops.ForgeFacts, error) {
	const action = "resolve forge facts"
	if a.shuttingDown.Load() {
		return gitops.ForgeFacts{}, ErrShuttingDown
	}
	workspace, err := a.reviewWorkspace(action, ws)
	if err != nil {
		return gitops.ForgeFacts{}, err
	}
	return a.gitCore().ResolveFacts(ctx, workspace, pr, rev, keys)
}

// GetGitBlobImage serves blob oid of ws's repository for display, as
// GetLocalImage serves a file: only a blob that is local, at most the
// display image cap and an image a browser displays (any other id is an
// error that names none of its content), through the review image store
// the review diff's images use, under the content's own key, so a blob is
// read once while it is held and a new head that keeps it costs nothing.
// A positive maxWidth serves a derivative at the next ladder width
// (attachment.Derive), held in the same store under the blob and tier;
// concurrent calls share one read and one derivation. The caller's
// context ends the git reads.
//
//ao:scope files:read
func (a *App) GetGitBlobImage(ctx context.Context, ws WorkspaceRef, oid string, maxWidth int) (LocalImage, error) {
	const action = "read git blob image"
	workspace, info, err := a.displayGitBlob(ctx, action, ws, oid)
	if err != nil {
		return LocalImage{}, err
	}
	core := a.gitCore()
	images := a.reviewDiffImages()
	key := "blob\x00" + oid
	original, err := images.Load(ctx, key, info.Size, func(buf []byte) (string, error) {
		if hook := a.gitBlobHook; hook != nil {
			hook(ctx, "read")
		}
		if err := core.ReadBlob(ctx, workspace, oid, buf); err != nil {
			return "", err
		}
		return gitops.DisplayImageMIME(buf)
	})
	if err != nil {
		return LocalImage{}, fmt.Errorf("%s: %w", action, err)
	}
	answer := LocalImage{
		MimeType: original.MimeType, Width: info.Width, Height: info.Height,
		OriginalWidth: info.Width, OriginalHeight: info.Height, OriginalBytes: original.Size,
	}
	served := original
	if tier := attachment.DeriveTier(maxWidth); tier > 0 {
		derived, err := images.LoadWithin(ctx, key+"\x00"+strconv.Itoa(tier), reviewDiffImageMaxBytes, func(take func(int64) ([]byte, error)) (string, error) {
			var mime string
			found, err := images.View(original.ID, func(data []byte) error {
				if hook := a.gitBlobHook; hook != nil {
					hook(ctx, "derive")
				}
				out, err := attachment.Derive(key, data, original.MimeType, maxWidth)
				if err != nil {
					return err
				}
				if !out.Derived {
					return errServeOriginal
				}
				buf, err := take(int64(len(out.Data)))
				if err != nil {
					return err
				}
				copy(buf, out.Data)
				mime = out.MimeType
				return nil
			})
			if !found {
				return "", errors.New("the image left the cache while it was derived; try again")
			}
			return mime, err
		})
		switch {
		case errors.Is(err, errServeOriginal):
		case err != nil:
			return LocalImage{}, fmt.Errorf("%s: %w", action, err)
		default:
			// A held derivative's size is its header's: Derive may step
			// down the ladder for a source over its pixel cap.
			found, err := images.View(derived.ID, func(data []byte) error {
				var err error
				answer.Width, answer.Height, err = attachment.ImageDimensions(data)
				return err
			})
			switch {
			case err != nil:
				return LocalImage{}, fmt.Errorf("%s: %w", action, err)
			case !found:
				return LocalImage{}, fmt.Errorf("%s: the image left the cache while it was read; try again", action)
			}
			served, answer.MimeType, answer.Derived = derived, derived.MimeType, true
		}
	}
	server := a.transportServer.Load()
	if server == nil {
		return LocalImage{}, fmt.Errorf("%s: transport is not serving", action)
	}
	if answer.URL, err = server.MintLocalImageTicket(served.ID); err != nil {
		return LocalImage{}, fmt.Errorf("%s: %w", action, err)
	}
	return answer, nil
}

// SaveGitBlobImage writes blob oid of ws's repository into the Downloads
// directory on this computer under name, as SaveLocalImage writes a file
// (saveDownload: the name cut to its last segment and sanitized, never
// overwriting a file already there), and returns the path it wrote. It
// saves only what GetGitBlobImage would display: a full id of a local
// blob within the display image cap whose bytes are an image a browser
// displays and not a Git LFS pointer, read without the network.
//
//ao:scope files:read
func (a *App) SaveGitBlobImage(ctx context.Context, ws WorkspaceRef, oid string, name string) (string, error) {
	const action = "save git blob image"
	workspace, info, err := a.displayGitBlob(ctx, action, ws, oid)
	if err != nil {
		return "", err
	}
	data := make([]byte, info.Size)
	if err := a.gitCore().ReadBlob(ctx, workspace, oid, data); err != nil {
		return "", fmt.Errorf("%s: %w", action, err)
	}
	mime, err := gitops.DisplayImageMIME(data)
	if err != nil {
		return "", fmt.Errorf("%s: %w", action, err)
	}
	return a.saveDownload(name, mime, data)
}

// displayGitBlob resolves ws and answers blob oid of its repository when it
// is one GetGitBlobImage serves, else an error under action.
func (a *App) displayGitBlob(ctx context.Context, action string, ws WorkspaceRef, oid string) (string, gitops.BlobImage, error) {
	if a.shuttingDown.Load() {
		return "", gitops.BlobImage{}, ErrShuttingDown
	}
	workspace, err := a.reviewWorkspace(action, ws)
	if err != nil {
		return "", gitops.BlobImage{}, err
	}
	info, err := a.gitCore().BlobImage(ctx, workspace, oid)
	switch {
	case err != nil:
		return "", gitops.BlobImage{}, fmt.Errorf("%s: %w", action, err)
	case info.LFS:
		return "", gitops.BlobImage{}, fmt.Errorf("%s: %w", action, gitops.ErrLFSPointer)
	case !info.Displayable():
		return "", gitops.BlobImage{}, fmt.Errorf("%s: %w", action, gitops.ErrNotDisplayImage)
	}
	return workspace, info, nil
}

// errServeOriginal is a derivation that keeps the source: the tier would
// not shrink it, or it is a kind Derive passes through.
var errServeOriginal = errors.New("the original serves this width")
