package tracker

import "time"

// MaxEvidenceIssues is the most issues one Evidence read names.
const MaxEvidenceIssues = 100

// EvidenceQuery names the issues an Evidence read reads.
type EvidenceQuery struct {
	Issues []EvidenceIssue
}

// EvidenceIssue is an issue of an EvidenceQuery and what is read of it:
// its comments, its worklogs, or both. WorklogsFrom bounds its worklogs
// to those started at or after it, zero reading every one: a long-running
// issue (a support "bucket") can hold years of them.
type EvidenceIssue struct {
	Issue              IssueID
	Comments, Worklogs bool
	WorklogsFrom       time.Time
}

// Check refuses a query of no issue, more than MaxEvidenceIssues, an empty
// issue ID, an issue named twice, or one that reads neither source.
func (q EvidenceQuery) Check() error {
	if len(q.Issues) == 0 || len(q.Issues) > MaxEvidenceIssues {
		return invalidf("an evidence read names 1 to %d issues, not %d", MaxEvidenceIssues, len(q.Issues))
	}
	seen := make(map[IssueID]bool, len(q.Issues))
	for _, in := range q.Issues {
		if in.Issue == "" || seen[in.Issue] || !in.Comments && !in.Worklogs {
			return invalidf("evidence issue %q is empty, named twice, or reads nothing", in.Issue)
		}
		seen[in.Issue] = true
	}
	return nil
}

// EvidenceSource is what of an issue an evidence read reads.
type EvidenceSource int

const (
	EvidenceComments EvidenceSource = iota + 1
	EvidenceWorklogs
)

// Evidence is what an Evidence read found: each source asked of each issue
// either read whole (Read) or failed for that issue alone (Failed), and
// the users the acts name.
type Evidence struct {
	Read   []SourceEvidence
	Failed []SourceFailure
	Catalog
}

// SourceEvidence is one issue's comments or worklogs, whole as the read
// found them (worklogs from the query's bound), with the issue's update
// time the read returned. Only acts of a person's account are listed: a
// comment or worklog of an app, of a customer, or of no account is no
// one's work credit counts.
type SourceEvidence struct {
	Issue   IssueID
	Source  EvidenceSource
	Updated time.Time
	Acts    []Act
}

// SourceFailure is a source of an issue the read could not read for that
// issue alone, and why: the tracker did not return the issue or reported
// it failed, an answer was too large even alone, or an author's account
// could not be confirmed as a person's. Err says what a person can act on.
type SourceFailure struct {
	Issue  IssueID
	Source EvidenceSource
	Err    error
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
