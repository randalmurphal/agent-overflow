package app

import gitops "agent-overflow/internal/git"

// GitListTags lists every tag of the workspace's repository, newest first
// (creatordate descending, then name). It is separate from GitListBranches
// so a large tag set never delays the branch list.
//
//ao:scope git:operate
func (a *App) GitListTags(ws WorkspaceRef) ([]gitops.GitTag, error) {
	return a.gitApplication().ListTags(ws)
}

// GitRefreshTags fetches origin's tags with force and no pruning, so a tag
// moved upstream is updated and a tag only this repository has survives,
// then returns the fresh list. The user asked for it, so a credential
// prompt may appear.
//
//ao:scope git:operate
func (a *App) GitRefreshTags(ws WorkspaceRef) ([]gitops.GitTag, error) {
	return a.gitApplication().RefreshTags(ws)
}
