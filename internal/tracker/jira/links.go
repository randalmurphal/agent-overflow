package jira

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// LinkInput links two issues: Outward relates to Inward by the type's
// Outward phrase ("WEB-1 blocks WEB-2").
type LinkInput struct {
	// Type is the link type's name or ID.
	Type    string
	Outward string
	Inward  string
}

// LinkIssues links two issues. Jira returns nothing about the link it
// stores, and answers a link it already has as made; read the issue's
// Links for its ID.
func (c *Client) LinkIssues(ctx context.Context, in LinkInput) error {
	if err := c.admit(ctx); err != nil {
		return err
	}
	for _, ref := range []string{in.Outward, in.Inward} {
		if err := checkIssue(ref); err != nil {
			return err
		}
	}
	types, err := c.IssueLinkTypes(ctx)
	if err != nil {
		return err
	}
	cands := make([]Candidate, len(types))
	for i, t := range types {
		cands[i] = Candidate{ID: t.ID, Name: t.Name}
	}
	hit, err := match("issue link type", in.Type, cands)
	if err != nil {
		return notSentErr(err)
	}
	body := map[string]any{
		"type":         idRef{hit.ID},
		"outwardIssue": map[string]string{"key": in.Outward},
		"inwardIssue":  map[string]string{"key": in.Inward},
	}
	if numericRE.MatchString(in.Outward) {
		body["outwardIssue"] = idRef{in.Outward}
	}
	if numericRE.MatchString(in.Inward) {
		body["inwardIssue"] = idRef{in.Inward}
	}
	if err := c.send(ctx, http.MethodPost, "issueLink", nil, body, nil); err != nil {
		return fmt.Errorf("jira: link %s %s %s: %w", in.Outward, hit.Name, in.Inward, err)
	}
	return nil
}

// UnlinkIssues deletes an issue link by its ID.
func (c *Client) UnlinkIssues(ctx context.Context, linkID string) error {
	if err := c.admit(ctx); err != nil {
		return err
	}
	if err := checkID("issue link", linkID); err != nil {
		return err
	}
	if err := c.send(ctx, http.MethodDelete, "issueLink/"+url.PathEscape(linkID), nil, nil, nil); err != nil {
		return fmt.Errorf("jira: delete issue link %s: %w", linkID, err)
	}
	return nil
}

// RemoteLink is a link from an issue to a page outside Jira.
type RemoteLink struct {
	ID           string
	GlobalID     string
	Relationship string
	URL          string
	Title        string
	Summary      string
	Resolved     bool
	// Application names the remote system, such as a forge.
	Application string
}

type remoteLinkJSON struct {
	ID           json.RawMessage `json:"id"`
	GlobalID     string          `json:"globalId"`
	Relationship string          `json:"relationship"`
	Application  struct {
		Name string `json:"name"`
	} `json:"application"`
	Object struct {
		URL     string `json:"url"`
		Title   string `json:"title"`
		Summary string `json:"summary"`
		Status  struct {
			Resolved bool `json:"resolved"`
		} `json:"status"`
	} `json:"object"`
}

// RemoteLinks returns an issue's remote links.
func (c *Client) RemoteLinks(ctx context.Context, issue string) ([]RemoteLink, error) {
	if err := c.admit(ctx); err != nil {
		return nil, err
	}
	path, err := issuePath(issue)
	if err != nil {
		return nil, err
	}
	var found []remoteLinkJSON
	if err := c.get(ctx, path+"/remotelink", nil, &found); err != nil {
		return nil, fmt.Errorf("jira: remote links of %s: %w", issue, err)
	}
	out := make([]RemoteLink, len(found))
	for i, l := range found {
		out[i] = RemoteLink{
			ID: idText(l.ID), GlobalID: l.GlobalID, Relationship: l.Relationship,
			URL: l.Object.URL, Title: l.Object.Title, Summary: l.Object.Summary,
			Resolved: l.Object.Status.Resolved, Application: l.Application.Name,
		}
	}
	return out, nil
}

// RemoteLinkInput is a remote link to add.
type RemoteLinkInput struct {
	URL   string // http or https
	Title string
	// Summary, Relationship ("links to" when empty) and Application are
	// optional.
	Summary      string
	Relationship string
	Application  string
	// GlobalID identifies the remote item; a link with the same GlobalID
	// on the issue is replaced, every field not given set to empty.
	GlobalID string
}

// AddRemoteLink adds a remote link, or replaces the one with the same
// GlobalID, and returns its ID.
func (c *Client) AddRemoteLink(ctx context.Context, issue string, in RemoteLinkInput) (string, error) {
	if err := c.admit(ctx); err != nil {
		return "", err
	}
	path, err := issuePath(issue)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(in.URL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", notSent("remote link on %s: %q is not an http or https URL", issue, in.URL)
	}
	if strings.TrimSpace(in.Title) == "" {
		return "", notSent("remote link on %s: a title is required", issue)
	}
	object := map[string]string{"url": in.URL, "title": in.Title}
	if in.Summary != "" {
		object["summary"] = in.Summary
	}
	body := map[string]any{"object": object}
	if in.Relationship != "" {
		body["relationship"] = in.Relationship
	}
	if in.Application != "" {
		body["application"] = map[string]string{"name": in.Application}
	}
	if in.GlobalID != "" {
		body["globalId"] = in.GlobalID
	}
	var stored struct {
		ID json.RawMessage `json:"id"`
	}
	err = c.send(ctx, http.MethodPost, path+"/remotelink", nil, body, &stored)
	if _, ok := errors.AsType[*AnswerError](err); ok {
		err = &StepError{Step: StepRemoteLink, Err: err}
	}
	if err != nil {
		return "", fmt.Errorf("jira: add remote link on %s: %w", issue, err)
	}
	id := idText(stored.ID)
	if !numericRE.MatchString(id) {
		return "", &StepError{Step: StepRemoteLink, Err: &AnswerError{Message: "the remote link's answer names no link ID"}}
	}
	return id, nil
}

// RemoveRemoteLink deletes a remote link by its ID.
func (c *Client) RemoveRemoteLink(ctx context.Context, issue, linkID string) error {
	if err := c.admit(ctx); err != nil {
		return err
	}
	path, err := issuePath(issue)
	if err != nil {
		return err
	}
	if err := checkID("remote link", linkID); err != nil {
		return err
	}
	if err := c.send(ctx, http.MethodDelete, path+"/remotelink/"+url.PathEscape(linkID), nil, nil, nil); err != nil {
		return fmt.Errorf("jira: delete remote link %s on %s: %w", linkID, issue, err)
	}
	return nil
}

// Watchers are an issue's watchers. Users lists only those the user may
// see; Count counts all.
type Watchers struct {
	Watching bool
	Count    int
	Users    []User
}

// Watchers returns an issue's watchers.
func (c *Client) Watchers(ctx context.Context, issue string) (Watchers, error) {
	if err := c.admit(ctx); err != nil {
		return Watchers{}, err
	}
	path, err := issuePath(issue)
	if err != nil {
		return Watchers{}, err
	}
	var v struct {
		IsWatching bool       `json:"isWatching"`
		WatchCount int        `json:"watchCount"`
		Watchers   []userJSON `json:"watchers"`
	}
	if err := c.get(ctx, path+"/watchers", nil, &v); err != nil {
		return Watchers{}, fmt.Errorf("jira: watchers of %s: %w", issue, err)
	}
	return Watchers{Watching: v.IsWatching, Count: v.WatchCount, Users: users(v.Watchers)}, nil
}

// AddWatcher makes the user with accountID watch an issue.
func (c *Client) AddWatcher(ctx context.Context, issue, accountID string) error {
	if err := c.admit(ctx); err != nil {
		return err
	}
	path, err := issuePath(issue)
	if err != nil {
		return err
	}
	if accountID == "" {
		return notSent("watch %s: an account ID is required", issue)
	}
	if err := c.send(ctx, http.MethodPost, path+"/watchers", nil, accountID, nil); err != nil {
		return fmt.Errorf("jira: add watcher to %s: %w", issue, err)
	}
	return nil
}

// RemoveWatcher stops the user with accountID watching an issue.
func (c *Client) RemoveWatcher(ctx context.Context, issue, accountID string) error {
	if err := c.admit(ctx); err != nil {
		return err
	}
	path, err := issuePath(issue)
	if err != nil {
		return err
	}
	if accountID == "" {
		return notSent("unwatch %s: an account ID is required", issue)
	}
	if err := c.send(ctx, http.MethodDelete, path+"/watchers", url.Values{"accountId": {accountID}}, nil, nil); err != nil {
		return fmt.Errorf("jira: remove watcher from %s: %w", issue, err)
	}
	return nil
}
