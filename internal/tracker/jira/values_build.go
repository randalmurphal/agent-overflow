package jira

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// The builders of rules, one per kind. Each takes texts already checked
// for count and blanks.

func buildText(_ context.Context, _ *builder, _ FieldMeta, in Input, v *Value) (any, error) {
	v.Display = in.texts[0]
	return in.texts[0], nil
}

func buildNumber(_ context.Context, _ *builder, f FieldMeta, in Input, v *Value) (any, error) {
	n, err := strconv.ParseFloat(strings.TrimSpace(in.texts[0]), 64)
	if err != nil || math.IsInf(n, 0) || math.IsNaN(n) {
		return nil, fmt.Errorf("jira: %s takes a number, not %q", f.Name, in.texts[0])
	}
	v.Display = strconv.FormatFloat(n, 'f', -1, 64)
	return n, nil
}

func buildDate(_ context.Context, _ *builder, f FieldMeta, in Input, v *Value) (any, error) {
	d, err := ParseDate(in.texts[0])
	if err != nil {
		return nil, fmt.Errorf("jira: %s: %w", f.Name, err)
	}
	v.Display = d.String()
	return d.String(), nil
}

func buildDateTime(_ context.Context, _ *builder, f FieldMeta, in Input, v *Value) (any, error) {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(in.texts[0]))
	if err != nil {
		return nil, fmt.Errorf("jira: %s takes an RFC 3339 time like 2026-11-16T14:30:00+01:00, not %q", f.Name, in.texts[0])
	}
	v.Display = t.Format(time.RFC3339)
	return t.Format(jiraTimeLayout), nil
}

type accountRef struct {
	AccountID string `json:"accountId"`
}

func buildUser(ctx context.Context, b *builder, f FieldMeta, in Input, v *Value) (any, error) {
	u, err := b.user(ctx, f, in.texts[0])
	if err != nil {
		return nil, err
	}
	v.Display = u.DisplayName
	return accountRef{u.AccountID}, nil
}

func buildUsers(ctx context.Context, b *builder, f FieldMeta, in Input, v *Value) (any, error) {
	out := make([]accountRef, 0, len(in.texts))
	names := make([]string, 0, len(in.texts))
	for _, who := range in.texts {
		u, err := b.user(ctx, f, who)
		if err != nil {
			return nil, err
		}
		out = append(out, accountRef{u.AccountID})
		names = append(names, u.DisplayName)
	}
	v.Display = strings.Join(names, ", ")
	return out, nil
}

// user resolves who for f, however it is named, to an active user who is
// not an app; for the assignee, one Jira says can be assigned the issue
// (or in the project, when creating).
func (b *builder) user(ctx context.Context, f FieldMeta, who string) (User, error) {
	if f.Schema.System != "assignee" || (b.opts.Issue == "" && b.opts.Project == "") {
		return b.c.findUser(ctx, who, nil)
	}
	q := AssignableQuery{Issue: b.opts.Issue}
	if q.Issue == "" {
		q.Project = b.opts.Project
	}
	return b.c.findUser(ctx, who, &q)
}

func buildLabels(ctx context.Context, b *builder, f FieldMeta, in Input, v *Value) (any, error) {
	for _, l := range in.texts {
		if strings.ContainsFunc(l, unicode.IsSpace) {
			return nil, fmt.Errorf("jira: %s: the label %q holds a space, which Jira refuses", f.Name, l)
		}
	}
	return buildStrings(ctx, b, f, in, v)
}

func buildStrings(_ context.Context, _ *builder, _ FieldMeta, in Input, v *Value) (any, error) {
	v.Display = strings.Join(in.texts, ", ")
	return in.texts, nil
}

type idRef struct {
	ID string `json:"id"`
}

// allowedCandidates are f's allowed values that can be chosen.
func allowedCandidates(vals []AllowedValue) []Candidate {
	out := make([]Candidate, 0, len(vals))
	for _, a := range vals {
		if !a.Disabled {
			out = append(out, Candidate{ID: a.ID, Name: a.Name})
		}
	}
	return out
}

// pick finds text among f's allowed values.
func pick(f FieldMeta, vals []AllowedValue, text string) (Candidate, error) {
	return match(f.Name+" value", strings.TrimSpace(text), allowedCandidates(vals))
}

func buildOption(_ context.Context, _ *builder, f FieldMeta, in Input, v *Value) (any, error) {
	hit, err := pick(f, f.Allowed, in.texts[0])
	if err != nil {
		return nil, err
	}
	v.Display = hit.Name
	return idRef{hit.ID}, nil
}

func buildOptions(_ context.Context, _ *builder, f FieldMeta, in Input, v *Value) (any, error) {
	out := make([]idRef, 0, len(in.texts))
	names := make([]string, 0, len(in.texts))
	for _, t := range in.texts {
		hit, err := pick(f, f.Allowed, t)
		if err != nil {
			return nil, err
		}
		out = append(out, idRef{hit.ID})
		names = append(names, hit.Name)
	}
	v.Display = strings.Join(names, ", ")
	return out, nil
}

// buildCascade takes a parent option, and optionally a child option of
// it as the second text.
func buildCascade(_ context.Context, _ *builder, f FieldMeta, in Input, v *Value) (any, error) {
	parent, err := pick(f, f.Allowed, in.texts[0])
	if err != nil {
		return nil, err
	}
	v.Display = parent.Name
	if len(in.texts) == 1 {
		return idRef{parent.ID}, nil
	}
	var children []AllowedValue
	for _, a := range f.Allowed {
		if a.ID == parent.ID {
			children = a.Children
		}
	}
	child, err := match(f.Name+" value under "+parent.Name, strings.TrimSpace(in.texts[1]), allowedCandidates(children))
	if err != nil {
		return nil, err
	}
	v.Display += " / " + child.Name
	return struct {
		ID    string `json:"id"`
		Child idRef  `json:"child"`
	}{parent.ID, idRef{child.ID}}, nil
}

// buildNamed picks one of the allowed values by name or ID: a priority,
// resolution, issue type, security level, version or component. A
// priority field without allowed values picks among the site's
// priorities.
func buildNamed(ctx context.Context, b *builder, f FieldMeta, in Input, v *Value) (any, error) {
	allowed := f.Allowed
	if allowed == nil && f.Schema.Type == "priority" {
		ps, err := b.c.Priorities(ctx)
		if err != nil {
			return nil, err
		}
		for _, p := range ps {
			allowed = append(allowed, AllowedValue{ID: p.ID, Name: p.Name})
		}
	}
	hit, err := pick(f, allowed, in.texts[0])
	if err != nil {
		return nil, err
	}
	v.Display = hit.Name
	return idRef{hit.ID}, nil
}

// buildNamedList picks components or versions. When the metadata lists
// no allowed values, the names go unchecked for Jira to resolve, and the
// value says so.
func buildNamedList(_ context.Context, _ *builder, f FieldMeta, in Input, v *Value) (any, error) {
	v.Display = strings.Join(in.texts, ", ")
	if f.Allowed == nil {
		v.Warnings = append(v.Warnings, f.Name+": the metadata lists no values, so Jira checks the names")
		out := make([]map[string]string, len(in.texts))
		for i, t := range in.texts {
			out[i] = map[string]string{"name": strings.TrimSpace(t)}
		}
		return out, nil
	}
	out := make([]idRef, 0, len(in.texts))
	names := make([]string, 0, len(in.texts))
	for _, t := range in.texts {
		hit, err := pick(f, f.Allowed, t)
		if err != nil {
			return nil, err
		}
		out = append(out, idRef{hit.ID})
		names = append(names, hit.Name)
	}
	v.Display = strings.Join(names, ", ")
	return out, nil
}

// buildIssue reads the issue named by key or ID, so a typo fails before
// anything is sent and the display names it, and sends the ID Jira
// returned, which stays the issue's when it moves project and its key
// changes.
func buildIssue(ctx context.Context, b *builder, f FieldMeta, in Input, v *Value) (any, error) {
	ref := strings.TrimSpace(in.texts[0])
	if err := checkIssue(ref); err != nil {
		return nil, err
	}
	is, err := b.c.GetIssue(ctx, ref, "summary")
	if err != nil {
		return nil, fmt.Errorf("jira: %s: %w", f.Name, err)
	}
	if !isNumeric(is.ID) {
		return nil, &AnswerError{Message: fmt.Sprintf("issue %s came without its numeric ID", ref)}
	}
	v.Display = is.Key + " " + is.Summary
	return map[string]string{"id": is.ID}, nil
}

// buildSprint takes a sprint's numeric ID, or the name of an open sprint
// on the boards of the issue's project (sprintByName).
func buildSprint(ctx context.Context, b *builder, f FieldMeta, in Input, v *Value) (any, error) {
	text := strings.TrimSpace(in.texts[0])
	id, err := strconv.ParseInt(text, 10, 64)
	if err != nil || id <= 0 {
		return b.sprintByName(ctx, f, text, v)
	}
	v.Display = "sprint " + text
	return id, nil
}

// sprintByName resolves text among the active and future sprints of the
// scrum boards of the project the value is for: the issue's project on
// an edit, ValueOptions.Project on a create.
func (b *builder) sprintByName(ctx context.Context, f FieldMeta, text string, v *Value) (any, error) {
	project := b.opts.Project
	if b.opts.Issue != "" {
		is, err := b.c.GetIssue(ctx, b.opts.Issue, "project")
		if err != nil {
			return nil, fmt.Errorf("jira: %s: %w", f.Name, err)
		}
		if is.Project == nil {
			return nil, fmt.Errorf("jira: %s: cannot tell which project %s is in, so not which sprints it can join", f.Name, b.opts.Issue)
		}
		project = is.Project.Key
	}
	if project == "" {
		return nil, fmt.Errorf("jira: %s: a sprint named %q needs the issue or project it is for", f.Name, text)
	}
	s, err := b.c.openSprintNamed(ctx, project, text)
	if err != nil {
		return nil, fmt.Errorf("jira: %s: %w", f.Name, err)
	}
	v.Display = s.Name + " (sprint " + strconv.FormatInt(s.ID, 10) + ")"
	return s.ID, nil
}

// buildTeam takes an Atlassian team's ID, which Jira checks.
func buildTeam(_ context.Context, _ *builder, _ FieldMeta, in Input, v *Value) (any, error) {
	id := strings.TrimSpace(in.texts[0])
	v.Display = "team " + id
	return id, nil
}
