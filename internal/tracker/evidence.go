package tracker

import "time"

// MaxEvidenceIssues is the most issues one Evidence read names.
const MaxEvidenceIssues = 100

// EvidenceQuery names the issues an Evidence read reads.
type EvidenceQuery struct {
	Issues []EvidenceIssue
}

// EvidenceIssue is an issue of an EvidenceQuery. WorklogsFrom bounds its
// worklogs to those started at or after it, zero reading every one: a
// long-running issue (a support "bucket") can hold years of them.
type EvidenceIssue struct {
	Issue        IssueID
	WorklogsFrom time.Time
}

// Check refuses a query of no issue, more than MaxEvidenceIssues, an empty
// issue ID, or an issue named twice.
func (q EvidenceQuery) Check() error {
	if len(q.Issues) == 0 || len(q.Issues) > MaxEvidenceIssues {
		return invalidf("an evidence read names 1 to %d issues, not %d", MaxEvidenceIssues, len(q.Issues))
	}
	seen := make(map[IssueID]bool, len(q.Issues))
	for _, in := range q.Issues {
		if in.Issue == "" || seen[in.Issue] {
			return invalidf("evidence issue %q is empty or named twice", in.Issue)
		}
		seen[in.Issue] = true
	}
	return nil
}

// Evidence is what an Evidence read found: each issue the tracker returned
// with its acts, and the users they name.
type Evidence struct {
	Issues []IssueEvidence
	Catalog
}

// IssueEvidence is one issue's comments and, when Capabilities.Evidence
// reads them, its worklogs, each whole as the read found them (worklogs
// from the query's bound), with the issue's update time the read
// returned. Only acts of a person's account are listed: a comment or
// worklog of an app, of a customer, or of no account is no one's work
// credit counts.
type IssueEvidence struct {
	Issue    IssueID
	Updated  time.Time
	Comments []Act
	Worklogs []Act
}

// Act is one comment or worklog as credit counts it: its ID within the
// issue, its author, when it was made (a comment's creation, a worklog's
// start) and a worklog's time spent.
type Act struct {
	ID     string
	Author UserID
	At     time.Time
	Spent  time.Duration
}
