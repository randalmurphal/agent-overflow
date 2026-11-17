package jira

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"agent-overflow/internal/tracker/trackerapi"
)

// Attachment is a file attached to an issue.
type Attachment struct {
	ID       string
	Filename string
	MimeType string
	Size     int64
	Created  time.Time
	Author   *User
}

type attachmentJSON struct {
	ID       json.RawMessage `json:"id"`
	Filename string          `json:"filename"`
	MimeType string          `json:"mimeType"`
	Size     int64           `json:"size"`
	Created  string          `json:"created"`
	Author   *userJSON       `json:"author"`
}

func (a attachmentJSON) attachment() Attachment {
	out := Attachment{ID: idText(a.ID), Filename: a.Filename, MimeType: a.MimeType, Size: a.Size, Author: a.Author.userPtr()}
	if t, err := parseTime(a.Created); err == nil {
		out.Created = t
	}
	return out
}

// Attachments returns an issue's attachments.
func (c *Client) Attachments(ctx context.Context, issue string) ([]Attachment, error) {
	if err := c.admit(ctx); err != nil {
		return nil, err
	}
	is, err := c.GetIssue(ctx, issue, "attachment")
	if err != nil {
		return nil, err
	}
	if p := problemOf(is, "attachment"); p != nil {
		return nil, fmt.Errorf("jira: attachments of %s: %w", issue, p)
	}
	return is.Attachments, nil
}

// problemOf is the decode error of field on is, or nil.
func problemOf(is *Issue, field string) error {
	for _, p := range is.Problems {
		if p.Field == field {
			return p.Err
		}
	}
	return nil
}

// File is one file to attach.
type File struct {
	Name    string
	Content io.Reader
}

// Attach uploads files to an issue in one request and returns the
// attachments Jira stored. The request is bounded by
// Options.MaxUploadBytes; Jira refuses more than 60 files at once. An
// upload Jira stored whose answer did not decode is a *StepError whose
// Applied reports it.
func (c *Client) Attach(ctx context.Context, issue string, files ...File) ([]Attachment, error) {
	if err := c.admit(ctx); err != nil {
		return nil, err
	}
	out, err := c.attach(ctx, issue, files...)
	if _, ok := errors.AsType[*AnswerError](err); ok {
		return out, &StepError{Step: StepUpload, Err: err}
	}
	return out, err
}

// attach is Attach without the admission guard and the step error, for
// the writes that upload images as one of their steps.
func (c *Client) attach(ctx context.Context, issue string, files ...File) ([]Attachment, error) {
	path, err := issuePath(issue)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, notSent("attach to %s: no files", issue)
	}
	parts := make([]trackerapi.Part, len(files))
	for i, f := range files {
		if strings.TrimSpace(f.Name) == "" || strings.ContainsFunc(f.Name, isControl) || f.Content == nil {
			return nil, notSent("attach to %s: file %d needs a name and content", issue, i)
		}
		parts[i] = trackerapi.Part{FieldName: "file", FileName: f.Name, Content: f.Content}
	}
	body, contentType, err := trackerapi.Multipart(ctx, parts, c.maxUpload)
	if err != nil {
		return nil, notSentErr(fmt.Errorf("jira: attach to %s: %w", issue, err))
	}
	var stored []attachmentJSON
	w, err := c.write(ctx, trackerapi.Request{
		Method: http.MethodPost, Path: apiPath + path + "/attachments",
		// Jira blocks an upload without it, as a cross-site request.
		Header:   http.Header{"X-Atlassian-Token": {"no-check"}},
		Body:     body,
		Transfer: true, ContentType: contentType,
	}, &stored)
	if err != nil {
		// What was stored is known by ID even when the rest is not read.
		var known []Attachment
		for _, id := range w.ids {
			known = append(known, Attachment{ID: id})
		}
		return known, fmt.Errorf("jira: attach to %s: %w", issue, err)
	}
	out := make([]Attachment, len(stored))
	for i, a := range stored {
		out[i] = a.attachment()
	}
	return out, nil
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

// DeleteAttachment deletes an attachment.
func (c *Client) DeleteAttachment(ctx context.Context, id string) error {
	if err := c.admit(ctx); err != nil {
		return err
	}
	if err := checkID("attachment", id); err != nil {
		return err
	}
	if err := c.send(ctx, http.MethodDelete, "attachment/"+url.PathEscape(id), nil, nil, nil); err != nil {
		return fmt.Errorf("jira: delete attachment %s: %w", id, err)
	}
	return nil
}

// DownloadAttachment copies an attachment's content to dst, at most
// limit bytes; a larger file fails with *trackerapi.BodyTooLargeError
// after limit bytes reached dst.
func (c *Client) DownloadAttachment(ctx context.Context, id string, dst io.Writer, limit int64) error {
	if err := c.admit(ctx); err != nil {
		return err
	}
	if err := checkID("attachment", id); err != nil {
		return err
	}
	_, err := c.api.Stream(ctx, trackerapi.Request{
		Path: apiPath + "attachment/content/" + url.PathEscape(id),
		// Served by Jira itself, so the credential never leaves the site.
		Query:    url.Values{"redirect": {"false"}},
		Transfer: true,
	}, dst, limit)
	if err != nil {
		return fmt.Errorf("jira: download attachment %s: %w", id, c.explain(ctx, err))
	}
	return nil
}

// AttachmentMediaID returns the media file ID of an attachment, which an
// ADF media node names it by. Jira redirects the attachment's content to
// the media service at .../file/<media ID>/binary; the redirect is read,
// not followed.
func (c *Client) AttachmentMediaID(ctx context.Context, id string) (string, error) {
	if err := c.admit(ctx); err != nil {
		return "", err
	}
	return c.mediaID(ctx, id)
}

func (c *Client) mediaID(ctx context.Context, id string) (string, error) {
	if err := checkID("attachment", id); err != nil {
		return "", err
	}
	loc, err := c.api.Location(ctx, trackerapi.Request{Path: apiPath + "attachment/content/" + url.PathEscape(id)})
	if err != nil {
		return "", fmt.Errorf("jira: media ID of attachment %s: %w", id, c.explain(ctx, err))
	}
	mediaID, err := mediaIDOf(loc)
	if err != nil {
		return "", fmt.Errorf("jira: media ID of attachment %s: %w", id, err)
	}
	return mediaID, nil
}

// mediaIDRE is a media file ID in the one form measured: a canonical
// UUID. Anything else is refused, so no other text of a response header
// reaches a document or a caller.
var mediaIDRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// mediaIDOf reads the media ID from a media service URL's path: the
// segment after "file". Its errors quote nothing of the URL.
func mediaIDOf(loc string) (string, error) {
	u, err := url.Parse(loc)
	if err != nil {
		return "", errors.New("the redirect target is not a URL")
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i, s := range segs {
		if s == "file" && i+1 < len(segs) && mediaIDRE.MatchString(segs[i+1]) {
			return segs[i+1], nil
		}
	}
	return "", errors.New("the redirect names no media file")
}
