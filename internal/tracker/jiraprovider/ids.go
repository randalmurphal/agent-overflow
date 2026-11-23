package jiraprovider

import (
	"fmt"
	"strconv"

	"agent-overflow/internal/tracker"
)

// Jira's IDs are positive decimal numbers: projects, issues, boards,
// sprints, comments, worklogs, links. A neutral ID is that number as
// text, and the provider refuses any other text before it reaches a
// path, a query or JQL. A key (WEB-1) is never an ID.

// invalidf is an input the provider refuses, wrapping
// tracker.ErrInvalidInput.
func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: jiraprovider: "+format, append([]any{tracker.ErrInvalidInput}, args...)...)
}

// numericID reads id, a positive decimal written as Jira writes it (no
// sign, no leading zero); what names it in the error.
func numericID(what, id string) (int64, error) {
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil || n <= 0 || strconv.FormatInt(n, 10) != id {
		return 0, invalidf("%q is not a Jira %s ID", id, what)
	}
	return n, nil
}

func decimal(n int64) string { return strconv.FormatInt(n, 10) }

func boardNumber(b tracker.BoardID) (int64, error) { return numericID("board", string(b)) }

func sprintNumber(s tracker.SprintID) (int64, error) { return numericID("sprint", string(s)) }

func checkProject(p tracker.ProjectID) error {
	_, err := numericID("project", string(p))
	return err
}

func checkIssue(id tracker.IssueID) error {
	_, err := numericID("issue", string(id))
	return err
}

// checkIssues refuses any of ids that is not an issue ID.
func checkIssues(ids []tracker.IssueID) error {
	for _, id := range ids {
		if err := checkIssue(id); err != nil {
			return err
		}
	}
	return nil
}
