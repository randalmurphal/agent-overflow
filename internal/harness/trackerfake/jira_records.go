package trackerfake

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"slices"
	"strconv"
)

// commentJSON is a comment as Jira reads it back.
func (s *jiraSite) commentJSON(is *JiraIssue, cm *JiraComment) map[string]any {
	out := map[string]any{
		"self": s.self("issue/" + is.ID + "/comment/" + cm.ID), "id": cm.ID,
		"author": s.userJSON(cm.Author), "updateAuthor": s.userJSON(orString(cm.UpdateAuthor, cm.Author)),
		"body": cm.Body, "created": jiraTime(cm.Created), "updated": jiraTime(cm.Updated), "jsdPublic": !cm.Internal,
	}
	if len(cm.Visibility) > 0 {
		out["visibility"] = cm.Visibility
	}
	return out
}

func orString(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// commentPageJSON is a page of is's comments, oldest first.
func (s *jiraSite) commentPageJSON(is *JiraIssue, start, size int) map[string]any {
	list := make([]any, len(is.Comments))
	for i := range is.Comments {
		list[i] = s.commentJSON(is, &is.Comments[i])
	}
	page := listPage("comments", list, start, size)
	page["self"] = s.self("issue/" + is.ID + "/comment")
	return page
}

// addComment stores a comment by author and returns it; internal holds
// only on a service desk project.
func (s *jiraSite) addComment(e *Engine, is *JiraIssue, author string, body, visibility json.RawMessage, internal bool) *JiraComment {
	now := e.now()
	is.Comments = append(is.Comments, JiraComment{
		ID: strconv.FormatInt(e.nextID(), 10), Author: author, UpdateAuthor: author, Body: slices.Clone(body),
		Created: now, Updated: now, Visibility: slices.Clone(visibility),
		Internal: internal && s.project(is.Project).TypeKey == "service_desk",
	})
	is.Updated = now
	return &is.Comments[len(is.Comments)-1]
}

func listComments(c *jiraCall) answer {
	if ob := c.query.Get("orderBy"); ob != "" && ob != "created" && ob != "+created" {
		return unhandledf("orderBy=%q", ob)
	}
	is, nf := c.found()
	if nf != nil {
		return *nf
	}
	start, size, err := c.pageWindow(5000, 5000)
	if err != nil {
		return unhandledf("%v", err)
	}
	return jsonAnswer(http.StatusOK, c.s.commentPageJSON(is, start, size))
}

// commentProperty is the one comment property implemented: the service
// desk's internal note marker.
type commentProperty struct {
	Key   string `json:"key"`
	Value struct {
		Internal bool `json:"internal"`
	} `json:"value"`
}

func addComment(c *jiraCall) answer {
	var in struct {
		Body       json.RawMessage   `json:"body"`
		Properties []commentProperty `json:"properties"`
		Visibility json.RawMessage   `json:"visibility"`
	}
	if err := c.decode(&in, false); err != nil {
		return unhandledf("%v", err)
	}
	internal := false
	for _, p := range in.Properties {
		if p.Key != "sd.public.comment" {
			return unhandledf("comment property %q", p.Key)
		}
		internal = p.Value.Internal
	}
	is, nf := c.found()
	if nf != nil {
		return *nf
	}
	if !validDoc(in.Body) {
		return jiraFieldErrors(http.StatusBadRequest, map[string]string{"comment": "Comment body can not be empty!"})
	}
	cm := c.s.addComment(c.e, is, c.viewer, in.Body, in.Visibility, internal)
	return jsonAnswer(http.StatusCreated, c.s.commentJSON(is, cm))
}

// foundComment is the comment {id} names on the issue {issue} names; a
// comment of another issue is not found (measured for reads; the writes
// that use it refuse the same way, unverified).
func (c *jiraCall) foundComment() (*JiraIssue, *JiraComment, *answer) {
	is, nf := c.found()
	if nf != nil {
		return nil, nil, nf
	}
	cm := is.comment(c.params["id"])
	if cm == nil {
		a := jiraErrors(http.StatusNotFound, "Can not find a comment for the id: "+c.params["id"]+".")
		return nil, nil, &a
	}
	return is, cm, nil
}

func getComment(c *jiraCall) answer {
	is, cm, nf := c.foundComment()
	if nf != nil {
		return *nf
	}
	return jsonAnswer(http.StatusOK, c.s.commentJSON(is, cm))
}

func editComment(c *jiraCall) answer {
	var in struct {
		Body       json.RawMessage `json:"body"`
		Visibility json.RawMessage `json:"visibility"`
	}
	if err := c.decode(&in, false); err != nil {
		return unhandledf("%v", err)
	}
	is, cm, nf := c.foundComment()
	if nf != nil {
		return *nf
	}
	if !validDoc(in.Body) {
		return jiraFieldErrors(http.StatusBadRequest, map[string]string{"comment": "Comment body can not be empty!"})
	}
	now := c.touch(is)
	cm.Body, cm.Visibility, cm.UpdateAuthor, cm.Updated = slices.Clone(in.Body), slices.Clone(in.Visibility), c.viewer, now
	return jsonAnswer(http.StatusOK, c.s.commentJSON(is, cm))
}

func deleteComment(c *jiraCall) answer {
	is, cm, nf := c.foundComment()
	if nf != nil {
		return *nf
	}
	id := cm.ID
	is.Comments = slices.DeleteFunc(is.Comments, func(x JiraComment) bool { return x.ID == id })
	c.touch(is)
	return emptyAnswer(http.StatusNoContent)
}

// linkChange is the changelog item a link adds to or removes from an
// issue. Measured: field Link with no fieldId, the other issue's key in
// to when added and in from when removed, the other side null; the
// wording of the text is the fake's (unverified).
func (s *jiraSite) linkChange(other *JiraIssue, phrase string, added bool) JiraChangeItem {
	text := "This issue " + phrase + " " + other.Key
	item := JiraChangeItem{Field: "Link", FieldType: "jira"}
	if added {
		item.To, item.ToString = &other.Key, &text
	} else {
		item.From, item.FromString = &other.Key, &text
	}
	return item
}

func linkIssues(c *jiraCall) answer {
	var in struct {
		Type         jiraRef         `json:"type"`
		OutwardIssue jiraRef         `json:"outwardIssue"`
		InwardIssue  jiraRef         `json:"inwardIssue"`
		Comment      json.RawMessage `json:"comment"`
	}
	if err := c.decode(&in, false); err != nil {
		return unhandledf("%v", err)
	}
	if in.Comment != nil {
		return unhandledf("a link's comment")
	}
	t := c.s.linkType(in.Type.ID + in.Type.Name)
	if t == nil {
		return jiraErrors(http.StatusNotFound, "No issue link type with name '"+in.Type.Name+in.Type.ID+"' found.")
	}
	out, inw := c.s.issue(in.OutwardIssue.ID+in.OutwardIssue.Key), c.s.issue(in.InwardIssue.ID+in.InwardIssue.Key)
	if out == nil || inw == nil {
		return jiraErrors(http.StatusNotFound, issueNotFound)
	}
	if slices.ContainsFunc(c.s.Links, func(l JiraLink) bool { return l.Type == t.ID && l.Outward == out.ID && l.Inward == inw.ID }) {
		return emptyAnswer(http.StatusCreated)
	}
	c.s.Links = append(c.s.Links, JiraLink{ID: strconv.FormatInt(c.e.nextID(), 10), Type: t.ID, Outward: out.ID, Inward: inw.ID})
	c.changed(out, []JiraChangeItem{c.s.linkChange(inw, t.Outward, true)})
	c.changed(inw, []JiraChangeItem{c.s.linkChange(out, t.Inward, true)})
	return emptyAnswer(http.StatusCreated)
}

func unlinkIssues(c *jiraCall) answer {
	i := slices.IndexFunc(c.s.Links, func(l JiraLink) bool { return l.ID == c.params["id"] })
	if i < 0 {
		return jiraErrors(http.StatusNotFound, "No issue link with id '"+c.params["id"]+"' exists.")
	}
	l := c.s.Links[i]
	c.s.Links = slices.Delete(c.s.Links, i, i+1)
	t, out, inw := c.s.linkType(l.Type), c.s.issue(l.Outward), c.s.issue(l.Inward)
	c.changed(out, []JiraChangeItem{c.s.linkChange(inw, t.Outward, false)})
	c.changed(inw, []JiraChangeItem{c.s.linkChange(out, t.Inward, false)})
	return emptyAnswer(http.StatusNoContent)
}

func (s *jiraSite) remoteLinkJSON(is *JiraIssue, l JiraRemoteLink) map[string]any {
	out := map[string]any{
		"id": idNumber(l.ID), "self": s.self("issue/" + is.ID + "/remotelink/" + l.ID),
		"object": map[string]any{"url": l.URL, "title": l.Title, "summary": l.Summary, "icon": map[string]any{}, "status": map[string]any{"resolved": false, "icon": map[string]any{}}},
	}
	if l.GlobalID != "" {
		out["globalId"] = l.GlobalID
	}
	if l.Relationship != "" {
		out["relationship"] = l.Relationship
	}
	if l.Application != "" {
		out["application"] = map[string]any{"name": l.Application}
	}
	return out
}

func listRemoteLinks(c *jiraCall) answer {
	is, nf := c.found()
	if nf != nil {
		return *nf
	}
	out := []any{}
	for _, l := range is.RemoteLinks {
		out = append(out, c.s.remoteLinkJSON(is, l))
	}
	return jsonAnswer(http.StatusOK, out)
}

func addRemoteLink(c *jiraCall) answer {
	var in struct {
		GlobalID     string `json:"globalId"`
		Relationship string `json:"relationship"`
		Application  *struct {
			Name string `json:"name"`
		} `json:"application"`
		Object struct {
			URL     string `json:"url"`
			Title   string `json:"title"`
			Summary string `json:"summary"`
		} `json:"object"`
	}
	if err := c.decode(&in, false); err != nil {
		return unhandledf("%v", err)
	}
	is, nf := c.found()
	if nf != nil {
		return *nf
	}
	if in.Object.URL == "" || in.Object.Title == "" {
		return jiraFieldErrors(http.StatusBadRequest, map[string]string{"object": "'url' and 'title' are required."})
	}
	l := JiraRemoteLink{GlobalID: in.GlobalID, Relationship: in.Relationship, URL: in.Object.URL, Title: in.Object.Title, Summary: in.Object.Summary}
	if in.Application != nil {
		l.Application = in.Application.Name
	}
	status := http.StatusCreated
	if i := slices.IndexFunc(is.RemoteLinks, func(r JiraRemoteLink) bool { return in.GlobalID != "" && r.GlobalID == in.GlobalID }); i >= 0 {
		l.ID, status = is.RemoteLinks[i].ID, http.StatusOK
		is.RemoteLinks[i] = l
	} else {
		l.ID = strconv.FormatInt(c.e.nextID(), 10)
		is.RemoteLinks = append(is.RemoteLinks, l)
	}
	return jsonAnswer(status, map[string]any{"id": idNumber(l.ID), "self": c.s.self("issue/" + is.ID + "/remotelink/" + l.ID)})
}

func removeRemoteLink(c *jiraCall) answer {
	is, nf := c.found()
	if nf != nil {
		return *nf
	}
	i := slices.IndexFunc(is.RemoteLinks, func(r JiraRemoteLink) bool { return r.ID == c.params["id"] })
	if i < 0 {
		return jiraErrors(http.StatusNotFound, "Remote link with id "+c.params["id"]+" does not exist.")
	}
	is.RemoteLinks = slices.Delete(is.RemoteLinks, i, i+1)
	return emptyAnswer(http.StatusNoContent)
}

func getWatchers(c *jiraCall) answer {
	is, nf := c.found()
	if nf != nil {
		return *nf
	}
	users := []any{}
	for _, id := range is.Watchers {
		users = append(users, c.s.userJSON(id))
	}
	return jsonAnswer(http.StatusOK, map[string]any{
		"self": c.s.self("issue/" + is.Key + "/watchers"), "isWatching": slices.Contains(is.Watchers, c.viewer),
		"watchCount": len(is.Watchers), "watchers": users,
	})
}

func addWatcher(c *jiraCall) answer {
	account := c.viewer
	if err := c.decode(&account, true); err != nil {
		return unhandledf("%v", err)
	}
	is, nf := c.found()
	if nf != nil {
		return *nf
	}
	if u := c.s.user(account); u == nil || u.Inactive {
		return jiraErrors(http.StatusNotFound, "The user \""+account+"\" does not exist.")
	}
	if !slices.Contains(is.Watchers, account) {
		is.Watchers = append(is.Watchers, account)
	}
	return emptyAnswer(http.StatusNoContent)
}

func removeWatcher(c *jiraCall) answer {
	is, nf := c.found()
	if nf != nil {
		return *nf
	}
	account := c.query.Get("accountId")
	if c.s.user(account) == nil {
		return jiraErrors(http.StatusNotFound, "The user \""+account+"\" does not exist.")
	}
	is.Watchers = slices.DeleteFunc(is.Watchers, func(id string) bool { return id == account })
	return emptyAnswer(http.StatusNoContent)
}

// attachmentChange is the changelog item of an attachment added or
// deleted. Measured: fieldId attachment, the ID in to when added and in
// from when removed, the other side null.
func attachmentChange(a JiraAttachment, added bool) JiraChangeItem {
	item := JiraChangeItem{FieldID: "attachment", Field: "Attachment", FieldType: "jira"}
	id, name := a.ID, a.Filename
	if added {
		item.To, item.ToString = &id, &name
	} else {
		item.From, item.FromString = &id, &name
	}
	return item
}

func attach(c *jiraCall) answer {
	if c.x.r.Header.Get("X-Atlassian-Token") != "no-check" {
		return answer{status: http.StatusForbidden, header: http.Header{"Content-Type": {"text/plain"}}, body: []byte("XSRF check failed")}
	}
	files, err := multipartFiles(c.x.r.Header.Get("Content-Type"), c.x.body)
	if err != nil {
		return unhandledf("%v", err)
	}
	is, nf := c.found()
	if nf != nil {
		return *nf
	}
	now := c.e.now()
	out := []any{}
	var items []JiraChangeItem
	for _, f := range files {
		a := JiraAttachment{
			ID: strconv.FormatInt(c.e.nextID(), 10), Filename: f.name, MimeType: f.mimeType, Content: f.content,
			Author: c.viewer, Created: now, MediaID: mediaID(c.e.nextID()),
		}
		is.Attachments = append(is.Attachments, a)
		out = append(out, c.s.attachmentJSON(a))
		items = append(items, attachmentChange(a, true))
	}
	c.changed(is, items)
	return jsonAnswer(http.StatusOK, out)
}

type uploaded struct {
	name, mimeType string
	content        []byte
}

// multipartFiles reads an upload's file parts; a part not named file is
// not implemented.
func multipartFiles(contentType string, body []byte) ([]uploaded, error) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "multipart/form-data" {
		return nil, errors.New("an attachment upload that is not multipart/form-data")
	}
	r := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	var out []uploaded
	for {
		part, err := r.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if part.FormName() != "file" || part.FileName() == "" {
			return nil, errors.New("upload part " + part.FormName() + " is not a file part named file")
		}
		content, err := io.ReadAll(part)
		if err != nil {
			return nil, err
		}
		mt := part.Header.Get("Content-Type")
		if mt == "" || mt == "application/octet-stream" {
			mt = http.DetectContentType(content)
		}
		out = append(out, uploaded{name: part.FileName(), mimeType: mt, content: content})
	}
	if len(out) == 0 {
		return nil, errors.New("an upload with no file part")
	}
	return out, nil
}

func deleteAttachment(c *jiraCall) answer {
	is, i := c.s.attachment(c.params["id"])
	if is == nil {
		return jiraErrors(http.StatusNotFound, "The attachment with id '"+c.params["id"]+"' does not exist")
	}
	a := is.Attachments[i]
	is.Attachments = slices.Delete(is.Attachments, i, i+1)
	c.changed(is, []JiraChangeItem{attachmentChange(a, false)})
	return emptyAnswer(http.StatusNoContent)
}

// attachmentContent answers the file with redirect=false, and otherwise
// a 303 to the media service on another host (measured).
func attachmentContent(c *jiraCall) answer {
	redirect := c.query.Get("redirect")
	if redirect != "" && redirect != "false" && redirect != "true" {
		return unhandledf("redirect=%q", redirect)
	}
	is, i := c.s.attachment(c.params["id"])
	if is == nil {
		return jiraErrors(http.StatusNotFound, "The attachment with id '"+c.params["id"]+"' does not exist")
	}
	a := is.Attachments[i]
	if redirect == "false" {
		return answer{status: http.StatusOK, header: http.Header{"Content-Type": {a.MimeType}}, body: slices.Clone(a.Content)}
	}
	loc := "https://media.example.com/file/" + a.MediaID + "/binary?token=signed-token"
	return answer{status: http.StatusSeeOther, header: http.Header{"Location": {loc}}}
}
