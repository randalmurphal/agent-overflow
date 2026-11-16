package forgefake

import (
	"cmp"
	"net/http"
	"slices"
	"strconv"
)

// GitLab merge trains (Premium, Ultimate; RepoMerge.MergeTrains): the
// merge trains API (doc/api/merge_trains.md and lib/api/merge_trains.rb
// at v18.0.0-ee). A car is a merge request whose auto-merge strategy is
// merge_train, ordered by its id as GitLab orders a train (active cars
// by id). Cars do not run or merge in the fake.

// gitlabTrainCars are the active cars on target's train, in train order.
func gitlabTrainCars(r *Repo, target string) []*Pull {
	var cars []*Pull
	for i := range r.Pulls {
		p := &r.Pulls[i]
		if armed := p.Readiness.AutoMerge; armed != nil && armed.Strategy == gitlabStrategyMergeTrain && p.State == "open" && (target == "" || p.BaseRef == target) {
			cars = append(cars, p)
		}
	}
	slices.SortFunc(cars, func(a, b *Pull) int { return cmp.Compare(a.Readiness.AutoMerge.car, b.Readiness.AutoMerge.car) })
	return cars
}

// gitlabCarJSON is a car as the merge trains API answers it.
func (e *Engine) gitlabCarJSON(r *Repo, p *Pull) map[string]any {
	armed := p.Readiness.AutoMerge
	var pipeline any
	if p.CI != nil {
		pipeline = map[string]any{"id": p.CI.ID, "sha": p.HeadSHA, "ref": "refs/merge-requests/" + strconv.Itoa(p.Number) + "/train", "status": p.CI.Status}
	}
	return map[string]any{
		"id": armed.car,
		"merge_request": map[string]any{
			"id": r.ID*1000 + int64(p.Number), "iid": p.Number, "project_id": r.ID, "title": p.Title,
			"state": gitlabStates[p.State], "web_url": gitlabMRURL(r, p),
		},
		"user":          gitlabUser(armed.By, armed.ByName),
		"pipeline":      pipeline,
		"created_at":    armed.At,
		"updated_at":    armed.At,
		"target_branch": p.BaseRef,
		"status":        "idle",
		"merged_at":     nil,
		"duration":      nil,
	}
}

func gitlabCarsJSON(e *Engine, r *Repo, cars []*Pull) []map[string]any {
	out := []map[string]any{}
	for _, p := range cars {
		out = append(out, e.gitlabCarJSON(r, p))
	}
	return out
}

// glabMergeTrainProject resolves the project and refuses one without
// merge trains with GitLab's 403.
func (e *Engine) glabMergeTrainProject(c *call, m []string) (*Repo, *Pull, response, bool) {
	r, p, fail, ok := e.glabMRFrom(c, m)
	if !ok {
		return nil, nil, fail, false
	}
	if !r.Merge.MergeTrains {
		return nil, nil, glabStatus(http.StatusForbidden, "403 Forbidden"), false
	}
	return r, p, response{}, true
}

// glabMergeTrainCar answers `GET projects/P/merge_trains/merge_requests/N`:
// the merge request's car, 404 when it is on no train.
func glabMergeTrainCar(e *Engine, c *call, m []string) response {
	r, p, fail, ok := e.glabMergeTrainProject(c, m)
	if !ok {
		return fail
	}
	if !slices.Contains(gitlabTrainCars(r, p.BaseRef), p) {
		return glabStatus(http.StatusNotFound, "404 Merge Train Merge Request Not Found")
	}
	return glabOK(jsonResponse(e.gitlabCarJSON(r, p)))
}

// glabMergeTrains answers `GET projects/P/merge_trains[?scope&sort&per_page&page]`:
// every train's cars, mixed, by id (desc by default). The fake keeps no
// merged cars, so scope=complete is empty.
func glabMergeTrains(e *Engine, c *call, m []string) response {
	r, fail, ok := e.glabProject(c, m[1])
	if !ok {
		return fail
	}
	if !r.Merge.MergeTrains {
		return glabStatus(http.StatusForbidden, "403 Forbidden")
	}
	query, perPage, page, err := glabPage(m[2], "scope", "sort")
	if err != nil {
		return unhandled("merge trains list: %v", err)
	}
	cars := gitlabTrainCars(r, "")
	switch query.Get("scope") {
	case "", "active":
	case "complete":
		cars = nil
	default:
		return glabStatus(http.StatusBadRequest, "400 Bad request - scope does not have a valid value")
	}
	switch query.Get("sort") {
	case "", "desc":
		slices.Reverse(cars)
	case "asc":
	default:
		return glabStatus(http.StatusBadRequest, "400 Bad request - sort does not have a valid value")
	}
	items, next := paginate(gitlabCarsJSON(e, r, cars), perPage, page)
	return glabOK(jsonResponse(items), "X-Next-Page: "+next, "X-Page: "+strconv.Itoa(page), "X-Total: "+strconv.Itoa(len(cars)))
}

// glabAddToMergeTrain answers `POST projects/P/merge_trains/merge_requests/N`
// with `{sha, squash, auto_merge}`: a viewer who cannot merge is 401, a
// moved head or an armed auto-merge 409 ("Merge request is already set
// to Auto-Merge"). auto_merge sets add_to_merge_train_when_checks_pass,
// available while the merge request is not mergeable yet (202);
// without it the merge request joins the train, which needs it
// mergeable (201). A strategy that is not available is 400 "Failed to
// merge". The answer is the target branch's train.
func glabAddToMergeTrain(e *Engine, c *call, m []string) response {
	var in struct {
		SHA       string `json:"sha"`
		Squash    *bool  `json:"squash"`
		AutoMerge bool   `json:"auto_merge"`
	}
	if err := decodeRESTBody(c, &in, true); err != nil {
		return unhandled("%v", err)
	}
	r, p, fail, ok := e.glabMergeTrainProject(c, m)
	if !ok {
		return fail
	}
	switch {
	case gitlabRoles[r.Merge.Permission] < gitlabRoles["developer"]:
		return glabStatus(http.StatusUnauthorized, "401 Unauthorized")
	case in.SHA != "" && in.SHA != p.HeadSHA:
		return glabSHAMismatch(p)
	case p.Readiness.AutoMerge != nil:
		return glabStatus(http.StatusConflict, "Merge request is already set to Auto-Merge")
	}
	mergeable := e.gitlabDetailedStatus(r, p) == "mergeable"
	if p.State != "open" || p.CI == nil || in.AutoMerge == (mergeable && !p.CI.live()) {
		return glabStatus(http.StatusBadRequest, "Failed to merge")
	}
	if in.Squash != nil && *in.Squash {
		p.squash = true
	}
	now := glabNow()
	armed := &AutoMerge{Method: gitlabArmMethod(r, p.squash), By: e.viewer, At: now, Strategy: gitlabStrategyAddWhenReady}
	event := Event{ID: e.ids.next(), Kind: "auto_merge_set", Actor: e.viewer, At: now, Train: gitlabTrainScheduled}
	status := http.StatusAccepted
	if !in.AutoMerge {
		armed.Strategy, armed.car = gitlabStrategyMergeTrain, e.ids.next()
		event.Train, event.Ref = gitlabTrainAdded, strconv.Itoa(len(gitlabTrainCars(r, p.BaseRef))+1)
		status = http.StatusCreated
	}
	p.Readiness.AutoMerge = armed
	p.Events = append(p.Events, event)
	resp := glabOK(jsonResponse(gitlabCarsJSON(e, r, gitlabTrainCars(r, p.BaseRef))))
	resp.status = status
	return resp
}
