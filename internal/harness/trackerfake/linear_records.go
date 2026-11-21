package trackerfake

import (
	"slices"
	"strings"
	"time"
)

func (m mutationRoot) createComment(r *gqlRun, args map[string]any) (any, *gqlErr) {
	w := m.w
	in := r.input(args, "CommentCreateInput")
	issue, _ := in.get("issueId")
	body, _ := in.get("body")
	parent, _ := in.get("parentId")
	in.done()
	is := w.issue(str(issue))
	if is == nil {
		return nil, notFound("Issue")
	}
	if strings.TrimSpace(str(body)) == "" {
		return nil, inputErr("Comment body is empty")
	}
	if p := str(parent); p != "" {
		if pc := w.comment(p); pc == nil || pc.Issue != is.ID {
			return nil, notFound("Comment")
		}
	}
	w.Comments = append(w.Comments, LinearComment{
		ID: linearID(r.e), Issue: is.ID, Parent: str(parent), User: w.Viewer, Body: str(body), CreatedAt: r.now, UpdatedAt: r.now,
	})
	return commentObj{w, &w.Comments[len(w.Comments)-1]}, nil
}

func (m mutationRoot) updateComment(r *gqlRun, args map[string]any) (any, *gqlErr) {
	w := m.w
	c := w.comment(idArg(args))
	if c == nil {
		return nil, notFound("Comment")
	}
	in := r.input(args, "CommentUpdateInput")
	body, ok := in.get("body")
	in.done()
	if ok {
		if strings.TrimSpace(str(body)) == "" {
			return nil, inputErr("Comment body is empty")
		}
		c.Body, c.EditedAt, c.UpdatedAt = str(body), r.now, r.now
	}
	return commentObj{w, c}, nil
}

// deleteComment answers the deleted comment's ID as the payload's
// entityId (documented: DeletePayload.entityId).
func (m mutationRoot) deleteComment(r *gqlRun, args map[string]any) (any, *gqlErr) {
	id := idArg(args)
	if m.w.comment(id) == nil {
		return nil, notFound("Comment")
	}
	m.w.Comments = slices.DeleteFunc(m.w.Comments, func(c LinearComment) bool { return c.ID == id })
	return id, nil
}

func (m mutationRoot) createRelation(r *gqlRun, args map[string]any) (any, *gqlErr) {
	w := m.w
	in := r.input(args, "IssueRelationCreateInput")
	issue, _ := in.get("issueId")
	related, _ := in.get("relatedIssueId")
	typ, _ := in.get("type")
	in.done()
	a, b := w.issue(str(issue)), w.issue(str(related))
	if a == nil || b == nil {
		return nil, notFound("Issue")
	}
	if a.ID == b.ID {
		return nil, inputErr("An issue cannot be related to itself")
	}
	w.Relations = append(w.Relations, LinearRelation{ID: linearID(r.e), Type: str(typ), Issue: a.ID, Related: b.ID})
	return relationObj{w, &w.Relations[len(w.Relations)-1]}, nil
}

func (m mutationRoot) deleteRelation(r *gqlRun, args map[string]any) (any, *gqlErr) {
	id := idArg(args)
	if m.w.relation(id) == nil {
		return nil, notFound("IssueRelation")
	}
	m.w.Relations = slices.DeleteFunc(m.w.Relations, func(x LinearRelation) bool { return x.ID == id })
	return id, nil // the payload's entityId, as for a comment
}

// createAttachment links a URL to an issue; a URL the issue already has
// updates that attachment (documented).
func (m mutationRoot) createAttachment(r *gqlRun, args map[string]any) (any, *gqlErr) {
	w := m.w
	in := r.input(args, "AttachmentCreateInput")
	issue, _ := in.get("issueId")
	url, _ := in.get("url")
	title, _ := in.get("title")
	subtitle, _ := in.get("subtitle")
	in.done()
	is := w.issue(str(issue))
	if is == nil {
		return nil, notFound("Issue")
	}
	if str(url) == "" || str(title) == "" {
		return nil, inputErr("An attachment needs a URL and a title")
	}
	for i := range w.Attachments {
		if a := &w.Attachments[i]; a.Issue == is.ID && a.URL == str(url) {
			a.Title, a.Subtitle, a.UpdatedAt = str(title), str(subtitle), r.now
			return attachmentObj{w, a}, nil
		}
	}
	w.Attachments = append(w.Attachments, LinearAttachment{
		ID: linearID(r.e), Issue: is.ID, Title: str(title), Subtitle: str(subtitle), URL: str(url), SourceType: "api",
		Creator: w.Viewer, CreatedAt: r.now, UpdatedAt: r.now,
	})
	return attachmentObj{w, &w.Attachments[len(w.Attachments)-1]}, nil
}

// startCycle starts the team's upcoming cycle at midnight today in the
// team's time zone and completes its active one, whose unfinished issues
// roll over to the started cycle (documented).
func (m mutationRoot) startCycle(r *gqlRun, args map[string]any) (any, *gqlErr) {
	w := m.w
	c := w.cycle(idArg(args))
	if c == nil {
		return nil, notFound("Cycle")
	}
	next := cycleObj{w, c}
	if !next.neighbor(true) {
		return nil, inputErr("Only the upcoming cycle can be started")
	}
	loc, err := time.LoadLocation(w.team(c.Team).Timezone)
	if err != nil {
		loc = time.UTC
	}
	local := r.now.In(loc)
	start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc).UTC()
	var old *LinearCycle
	for i := range w.Cycles {
		if o := (cycleObj{w, &w.Cycles[i]}); o.c.Team == c.Team && o.active() {
			old = o.c
		}
	}
	length := c.EndsAt.Sub(c.StartsAt)
	c.StartsAt, c.EndsAt = start, start.Add(length)
	if old != nil {
		old.CompletedAt, old.EndsAt = r.now, start
		for i := range w.Issues {
			is := &w.Issues[i]
			if t := w.state(is.State).Type; is.Cycle == old.ID && t != "completed" && t != "canceled" {
				before := *is
				is.Cycle = c.ID
				w.record(r.e, &before, is, "", r.now)
			}
		}
	}
	return cycleObj{w, c}, nil
}
