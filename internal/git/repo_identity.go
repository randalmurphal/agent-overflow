package git

import "strings"

// RepoIdentity answers the two facts that name the REPOSITORY a checkout is
// of, rather than the directory it happens to sit in: the `origin` remote and
// the root commit of HEAD. A client attached to several backends uses the pair
// to recognise the same repository cloned on two machines as one project.
//
// remoteURL is git's own spelling, verbatim. Nothing is normalised here on
// purpose: the matching happens on the client, so the client owns the
// normalisation (scheme, user, `.git` suffix, host case, the SSH alias form),
// and a backend that pre-chewed the string would only give it a second,
// disagreeing dialect to reconcile.
//
// rootCommit is the LEXICOGRAPHICALLY SMALLEST parentless commit reachable
// from HEAD. A repository can have several roots — an orphan branch merged in,
// a history grafted from another project — and `rev-list` orders them by
// traversal, which differs with the checked-out branch. Sorting is what makes
// two machines answer the same string for the same repository.
//
// Both are "" when cwd is not a repository, HEAD is unborn, or git failed, and
// there is no error return — the same posture as OriginRemoteURL, for the same
// reason: every caller wants "the identity, if there is one", and empty is the
// answer for "not known" in the column this feeds.
func (c *Core) RepoIdentity(cwd string) (remoteURL, rootCommit string) {
	if cwd == "" {
		return "", ""
	}
	// Through originRemote, not readOriginRemote, so this shares the
	// per-repository TTL cache with Status and DetectForge: on the boot
	// backfill's path a project already probed by the status cadence costs
	// no subprocess at all.
	return c.originRemote(cwd).url, c.rootCommit(cwd)
}

// rootCommit reads HEAD's parentless commits and returns the smallest.
// Deliberately uncached: it is asked at project creation and once per boot for
// rows that have no answer yet, and a root commit that has actually changed
// (a graft, a rewritten history) must not be served from a stale window.
func (c *Core) rootCommit(cwd string) string {
	result, err := c.run(cwd, "rev-list", "--max-parents=0", "HEAD")
	if err != nil || result.exitCode != 0 {
		return ""
	}
	smallest := ""
	for _, line := range strings.Split(result.stdout, "\n") {
		root := strings.TrimSpace(line)
		if root == "" {
			continue
		}
		if smallest == "" || root < smallest {
			smallest = root
		}
	}
	return smallest
}
