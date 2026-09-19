package git

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"agent-overflow/internal/forgeattach"
)

// Forge-hosted attachment downloads.
//
// A PR body or a review comment references media the forge holds behind
// the same login the browser uses: GitLab's /uploads/<secret>/<name>,
// GitHub's user-attachments assets. The user's own gh / glab session is
// the credential, so the fetch is one more `api` subcommand rather than
// an HTTP client of ours with a token we would have to find.
//
// Nothing here echoes the argument vector into an error. A GitLab upload
// path carries the file's 32-hex secret and a GitHub asset URL can
// redirect through a signed one, and both would otherwise land in a
// message the review pane shows.

// forgeAttachmentTimeout bounds one download. The package default (45s)
// is sized for a metadata call; a video at the transport's minimum
// sustained transfer rate needs minutes, and cutting it at 45s would read
// as a broken forge rather than a slow one.
const forgeAttachmentTimeout = 10 * time.Minute

// githubEscapeSequencesFlag disables gh's refusal to print a body
// containing terminal escape bytes. Media contains 0x1b by chance, so
// without it a perfectly good video is a failed download. Older gh does
// not know the flag; githubForge.FetchAttachment retries once without it.
const githubEscapeSequencesFlag = "--allow-escape-sequences"

// FetchAttachment resolves one attachment reference found in a PR/MR and
// downloads its bytes through the owning forge's CLI.
//
// The reference is parsed BEFORE anything is dispatched, so a href that
// is not a forge attachment costs no subprocess.
func (c *Core) FetchAttachment(cwd string, ref PRReference, href string, maxBytes int64) (data []byte, filename string, err error) {
	target, err := forgeattach.ParseReference(ref.Forge, ref.Project(), href)
	if err != nil {
		return nil, "", err
	}
	data, err = c.ForgeByID(ref.Forge).FetchAttachment(cwd, target, maxBytes)
	if err != nil {
		return nil, "", err
	}
	return data, target.Filename, nil
}

func (f *gitlabForge) FetchAttachment(cwd string, target forgeattach.Target, maxBytes int64) ([]byte, error) {
	if target.Forge != "gitlab" || target.Request == "" {
		return nil, errors.New("attachment reference is not a GitLab upload")
	}
	// glab copies a non-JSON response body to stdout verbatim, which is
	// what the uploads endpoint answers with.
	body, result, err := f.core.runAttachmentDownload("glab", cwd, maxBytes,
		"api", target.Request)
	if err != nil {
		return nil, normalizeGitLabCLIError(err)
	}
	if result.exitCode != 0 {
		return nil, gitlabCommandFailure("glab api upload download failed", result)
	}
	return body, nil
}

func (f *githubForge) FetchAttachment(cwd string, target forgeattach.Target, maxBytes int64) ([]byte, error) {
	if target.Forge != "github" || !strings.Contains(target.Request, "://") {
		return nil, errors.New("attachment reference is not a GitHub attachment URL")
	}
	// An argument containing "://" is used by gh as the request URL
	// as-is. It attaches the user's token for github.com and follows the
	// redirect to the signed asset host, which carries its own admission
	// in the query — Go's client drops Authorization across hosts, which
	// is exactly right here.
	args := []string{"api", target.Request, "-H", "Accept: */*", githubEscapeSequencesFlag}
	body, result, err := f.core.runAttachmentDownload("gh", cwd, maxBytes, args...)
	if err != nil {
		return nil, normalizeGitHubCLIError(err)
	}
	if result.exitCode != 0 && mentionsUnknownFlag(result, githubEscapeSequencesFlag) {
		body, result, err = f.core.runAttachmentDownload("gh", cwd, maxBytes, args[:len(args)-1]...)
		if err != nil {
			return nil, normalizeGitHubCLIError(err)
		}
	}
	if result.exitCode != 0 {
		return nil, fmt.Errorf("gh api attachment download failed: %s", commandOutputMessage(result.stdout, result.stderr))
	}
	return body, nil
}

func (nullForge) FetchAttachment(string, forgeattach.Target, int64) ([]byte, error) {
	return nil, ErrUnsupportedForge
}

// runAttachmentDownload streams one forge-CLI response body into memory
// under a hard cap.
//
// Streamed rather than captured as stdout because the shared runner
// keeps stdout as a string: a 100 MiB video would be buffered, copied
// into a string and copied back out. It also means an over-cap body
// cancels the child on the chunk that crosses the line instead of being
// drained in full and then rejected.
func (c *Core) runAttachmentDownload(binary, cwd string, maxBytes int64, args ...string) ([]byte, commandResult, error) {
	var body bytes.Buffer
	result, err := c.runSpec(commandSpec{
		binary:  binary,
		cwd:     cwd,
		args:    args,
		timeout: forgeAttachmentTimeout,
		output:  &body,
		// One byte of headroom: the cap is crossed by a body LARGER than
		// the limit, not by one that exactly reaches it.
		outputLimit: maxBytes + 1,
	})
	if err != nil {
		if errors.Is(err, errOutputLimitExceeded) {
			return nil, result, attachmentTooLargeError(maxBytes)
		}
		// runSpec names the command it ran, and that name carries the
		// GitLab upload secret or a signed GitHub URL. The cause is
		// kept for errors.Is / errors.As — a missing binary is still
		// recognized as exec.Error downstream — and only the text the
		// review pane shows is redacted.
		return nil, result, &redactedCommandError{
			message: redactForgeRequest(err.Error(), args),
			err:     err,
		}
	}
	if int64(body.Len()) > maxBytes {
		return nil, result, attachmentTooLargeError(maxBytes)
	}
	if result.exitCode != 0 {
		// The response body is where both CLIs put a forge's own error
		// JSON, and stdout is empty here because the body was streamed.
		// Hand the readable part of it back so the failure says what the
		// forge said rather than "command failed".
		result.stdout = errorBodyText(body.Bytes())
	}
	return body.Bytes(), result, nil
}

func attachmentTooLargeError(maxBytes int64) error {
	if maxBytes >= 1<<20 {
		return fmt.Errorf("attachment is larger than %d MiB", maxBytes>>20)
	}
	return fmt.Errorf("attachment is larger than %d bytes", maxBytes)
}

// redactedCommandError is a run failure with the request taken out of
// its message and left in its cause.
type redactedCommandError struct {
	message string
	err     error
}

func (e *redactedCommandError) Error() string { return e.message }
func (e *redactedCommandError) Unwrap() error { return e.err }

// redactForgeRequest removes the request arguments from a message.
//
// By VALUE rather than by position, because the message was formatted by
// a shared runner this package does not own: matching what it printed is
// a guess, while replacing the exact strings that were passed in is not.
// Both spellings, since the runner quotes an argument containing a space.
func redactForgeRequest(message string, args []string) string {
	const elision = "<attachment>"
	for _, arg := range args {
		if len(arg) < 8 {
			continue
		}
		message = strings.ReplaceAll(message, fmt.Sprintf("%q", arg), elision)
		message = strings.ReplaceAll(message, arg, elision)
	}
	return message
}

// mentionsUnknownFlag reports whether the CLI refused the named flag,
// which is how an older gh answers one it does not know.
func mentionsUnknownFlag(result commandResult, flag string) bool {
	message := strings.ToLower(result.stderr + "\n" + result.stdout)
	return strings.Contains(message, "unknown flag") && strings.Contains(message, strings.ToLower(strings.TrimPrefix(flag, "--")))
}

// errorBodyText is the readable prefix of a failed response body. Binary
// is dropped rather than shown: a truncated video in an error toast is
// noise, and the exit status already carries the fact of the failure.
func errorBodyText(body []byte) string {
	const maxErrorBodyBytes = 2 << 10
	if len(body) > maxErrorBodyBytes {
		body = body[:maxErrorBodyBytes]
	}
	if !utf8.Valid(body) {
		return ""
	}
	return strings.TrimSpace(string(body))
}
