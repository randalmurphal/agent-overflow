package git

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"sync"

	"agent-overflow/internal/forgeapi"
)

// GitLab CI: the MR's head pipeline is one pipeline of staged jobs.
// The pipeline comes from the MR read, its jobs from
// /pipelines/:id/jobs; per-job traces from /jobs/:id/trace. Verified shapes (2026-07): jobs carry id, name,
// stage, status, duration, web_url, allow_failure, started_at; the
// jobs list is ordered newest-first, so stage order is recovered by
// first-seen over ascending job id.

// gitlabCIJobsMaxPages bounds the jobs pages read of one pipeline. A
// pipeline past it shows its newest gitlabCIJobsMaxPages*gitlabPageSize
// jobs.
const gitlabCIJobsMaxPages = 5

type gitlabCIJobRaw struct {
	ID           int64    `json:"id"`
	Name         string   `json:"name"`
	Stage        string   `json:"stage"`
	Status       string   `json:"status"`
	Duration     *float64 `json:"duration"`
	WebURL       string   `json:"web_url"`
	AllowFailure bool     `json:"allow_failure"`
	StartedAt    *string  `json:"started_at"`
}

// readGitLabPipeline reads the jobs of a pipeline already read: the merge
// request's head pipeline, which the MR read named, or ReadPipeline's. A
// merge request with no pipeline costs no request.
func readGitLabPipeline(ctx context.Context, client *forgeapi.Client, repo RepoReference, pipeline *gitlabPipelineRaw) (CIPipeline, error) {
	if pipeline == nil || pipeline.ID <= 0 {
		return CIPipeline{}, nil
	}
	request := forgeapi.Request{
		Path:  gitlabProjectPath(repo.Project()) + "/pipelines/" + strconv.FormatInt(pipeline.ID, 10) + "/jobs",
		Query: url.Values{"per_page": {strconv.Itoa(gitlabPageSize)}},
	}
	var jobs []gitlabCIJobRaw
	page, pages := 1, 0
	err := client.Pages(ctx, request, func(resp *forgeapi.Response) (bool, error) {
		var pageJobs []gitlabCIJobRaw
		if err := json.Unmarshal(resp.Body, &pageJobs); err != nil {
			return false, fmt.Errorf("GitLab pipeline %d jobs: decode response: %w", pipeline.ID, err)
		}
		jobs = append(jobs, pageJobs...)
		pages++
		return len(pageJobs) == gitlabPageSize && pages < gitlabCIJobsMaxPages && advanceGitLabPage(resp, &page), nil
	})
	if err != nil {
		return CIPipeline{}, err
	}
	return CIPipeline{
		ID:     strconv.FormatInt(pipeline.ID, 10),
		Status: NormalizeCIStatus(pipeline.Status, ""),
		URL:    pipeline.WebURL,
		Ref:    pipeline.Ref,
		SHA:    pipeline.SHA,
		Stages: groupGitLabJobsByStage(jobs),
	}, nil
}

func groupGitLabJobsByStage(raw []gitlabCIJobRaw) []CIStage {
	// The API returns jobs newest-first; ascending job id recovers
	// creation order, which follows stage order.
	sort.Slice(raw, func(i, j int) bool { return raw[i].ID < raw[j].ID })

	var stages []CIStage
	indexByStage := make(map[string]int)
	for _, job := range raw {
		duration := 0.0
		if job.Duration != nil {
			duration = *job.Duration
		}
		ci := CIJob{
			ID:              strconv.FormatInt(job.ID, 10),
			Name:            job.Name,
			Status:          NormalizeCIStatus(job.Status, ""),
			DurationSeconds: duration,
			URL:             job.WebURL,
			AllowFailure:    job.AllowFailure,
			// A trace exists once the job has started; created/manual/
			// skipped jobs 404 on the trace endpoint.
			LogsAvailable: job.StartedAt != nil && *job.StartedAt != "",
		}
		index, ok := indexByStage[job.Stage]
		if !ok {
			index = len(stages)
			indexByStage[job.Stage] = index
			stages = append(stages, CIStage{Name: job.Stage})
		}
		stages[index].Jobs = append(stages[index].Jobs, ci)
	}
	for i := range stages {
		statuses := make([]string, len(stages[i].Jobs))
		for j, job := range stages[i].Jobs {
			statuses[j] = job.Status
		}
		stages[i].Status = AggregateCIStatus(statuses)
	}
	return stages
}

// gitlabMergeRequestRef is the ref of a merge request pipeline: the
// merge request's head, its merged result or its merge train entry.
var gitlabMergeRequestRef = regexp.MustCompile(`^refs/merge-requests/(\d+)/(?:head|merge|train)$`)

// gitlabTagScopeMaxPages bounds the tag scope pages one ListPipelines
// reads: the latest pipelines of that many hundred tags, newer than the
// oldest listed pipeline.
const gitlabTagScopeMaxPages = 10

// gitlabPipelineTagReadConcurrency bounds the single pipeline reads one
// ListPipelines has in flight to classify its rows.
const gitlabPipelineTagReadConcurrency = 4

// ListPipelines lists the project's pipelines, the most recent first: one
// list request, the tag scope reads that say which refs are tags
// (gitlabTagPipelines) and the single pipeline reads that classify a
// pipeline the scope leaves ambiguous (gitlabPipelineTags.kinds).
func (f *gitlabForge) ListPipelines(ctx context.Context, repo RepoReference, filter PipelineListFilter) ([]PipelineSummary, error) {
	status, err := pipelineStatusFilter("gitlab", filter.Status)
	if err != nil {
		return nil, err
	}
	client, err := f.core.gitlabAPI(repo.Host)
	if err != nil {
		return nil, err
	}
	query := url.Values{"per_page": {strconv.Itoa(filter.Limit)}}
	if filter.Ref != "" {
		query.Set("ref", filter.Ref)
	}
	if status != "" {
		query.Set("status", status)
	}
	path := gitlabProjectPath(repo.Project()) + "/pipelines"
	var raw []gitlabPipelineRaw
	if _, err := client.JSON(ctx, forgeapi.Request{Path: path, Query: query}, &raw); err != nil {
		return nil, err
	}
	tagRefs, tagged, err := gitlabTagPipelines(ctx, client, path, filter.Ref, raw)
	if err != nil {
		return nil, err
	}
	// The rows are the newest filter.Limit of the list and the tag scope's
	// pipelines the list did not hold, the status filter applied to those.
	// A listed page is the newest pipelines, so a scope pipeline survives
	// only when it is newer than a listed one (created between the reads).
	scope := make(map[int64]bool, len(tagged))
	for _, pipeline := range tagged {
		scope[pipeline.ID] = true
	}
	listed := make(map[int64]bool, len(raw))
	for _, pipeline := range raw {
		listed[pipeline.ID] = true
	}
	for _, pipeline := range tagged {
		if !listed[pipeline.ID] && (status == "" || pipeline.Status == status) {
			raw = append(raw, pipeline)
			listed[pipeline.ID] = true
		}
	}
	sort.SliceStable(raw, func(i, j int) bool { return raw[i].ID > raw[j].ID })
	raw = raw[:min(len(raw), filter.Limit)]
	kinds, err := f.pipelineTags.kinds(ctx, client, repo, raw, tagRefs, scope)
	if err != nil {
		return nil, err
	}
	rows := make([]PipelineSummary, 0, len(raw))
	for _, pipeline := range raw {
		kind, ok := kinds[pipeline.ID]
		if !ok {
			continue
		}
		row := PipelineSummary{
			ID:        strconv.FormatInt(pipeline.ID, 10),
			Status:    NormalizeCIStatus(pipeline.Status, ""),
			Ref:       pipeline.Ref,
			RefKind:   kind,
			SHA:       pipeline.SHA,
			Source:    pipeline.Source,
			CreatedAt: pipeline.CreatedAt,
			UpdatedAt: pipeline.UpdatedAt,
			URL:       pipeline.WebURL,
		}
		if m := gitlabMergeRequestRef.FindStringSubmatch(pipeline.Ref); m != nil {
			row.MRNumber, _ = strconv.Atoi(m[1])
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// gitlabPipelineTags remembers, per repository, whether each listed
// pipeline on a tag's ref name ran on the tag, so the single pipeline read
// that classifies an ambiguous one is made once while the repository's
// listings hold it: a pipeline's tag flag never changes. Concurrent
// listings share one read per pipeline. A finishing listing prunes the
// memory to the pipelines it and every listing that overlapped it in time
// answered, its own alone when none did.
type gitlabPipelineTags struct {
	mu    sync.Mutex
	repos map[string]*gitlabRepoTags
}

// gitlabRepoTags is one repository's memory, under gitlabPipelineTags.mu.
// tags is each remembered pipeline's flag and reads the single reads in
// flight. listings holds the pipeline ids of each listing in flight by its
// start, and recent those of the finished listings that overlapped one
// still in flight; start and end stamps come from next. The entry is
// deleted once it holds nothing.
type gitlabRepoTags struct {
	tags     map[int64]bool
	reads    map[int64]*gitlabTagRead
	listings map[uint64][]int64
	recent   []gitlabFinishedListing
	next     uint64
}

type gitlabFinishedListing struct {
	end uint64
	ids []int64
}

// gitlabTagRead is one single pipeline read in flight; tag and err are
// set before done closes.
type gitlabTagRead struct {
	done chan struct{}
	tag  bool
	err  error
}

// beginListing registers a listing of pipelines ids under key and
// returns its repository memory, its start and the flags remembered for
// those ids.
func (c *gitlabPipelineTags) beginListing(key string, ids []int64) (*gitlabRepoTags, uint64, map[int64]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	state := c.repos[key]
	if state == nil {
		state = &gitlabRepoTags{tags: map[int64]bool{}, reads: map[int64]*gitlabTagRead{}, listings: map[uint64][]int64{}}
		if c.repos == nil {
			c.repos = map[string]*gitlabRepoTags{}
		}
		c.repos[key] = state
	}
	start := state.next
	state.next++
	state.listings[start] = ids
	known := map[int64]bool{}
	for _, id := range ids {
		if tag, ok := state.tags[id]; ok {
			known[id] = tag
		}
	}
	return state, start, known
}

// endListing remembers the flags a listing learned (learned) and prunes
// the memory to the ids of that listing, the listings in flight and the
// finished ones that overlapped those.
func (c *gitlabPipelineTags) endListing(key string, state *gitlabRepoTags, start uint64, learned map[int64]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	maps.Copy(state.tags, learned)
	ids := state.listings[start]
	delete(state.listings, start)
	keep := make(map[int64]bool, len(ids))
	for _, id := range ids {
		keep[id] = true
	}
	for _, held := range state.listings {
		for _, id := range held {
			keep[id] = true
		}
	}
	for _, finished := range state.recent {
		for _, id := range finished.ids {
			keep[id] = true
		}
	}
	for id := range state.tags {
		if !keep[id] {
			delete(state.tags, id)
		}
	}
	// A finished listing matters while a listing it overlapped is in
	// flight: one that started before it ended.
	end := state.next
	state.next++
	oldest := state.next
	for listing := range state.listings {
		oldest = min(oldest, listing)
	}
	state.recent = slices.DeleteFunc(append(state.recent, gitlabFinishedListing{end: end, ids: ids}), func(finished gitlabFinishedListing) bool {
		return finished.end < oldest
	})
	if len(state.tags) == 0 && len(state.reads) == 0 && len(state.listings) == 0 {
		delete(c.repos, key)
	}
}

// readTag answers pipeline id's tag flag for a listing registered on
// state: remembered, the read another listing has in flight, or its own
// read in one of slots. A joined read that failed for other than a 404
// (its listing's admission, its cancellation) is tried once more.
func (c *gitlabPipelineTags) readTag(ctx context.Context, client *forgeapi.Client, project string, state *gitlabRepoTags, id int64, slots chan struct{}) (bool, error) {
	for attempt := 0; ; attempt++ {
		c.mu.Lock()
		if tag, ok := state.tags[id]; ok {
			c.mu.Unlock()
			return tag, nil
		}
		read, joined := state.reads[id]
		if !joined {
			read = &gitlabTagRead{done: make(chan struct{})}
			state.reads[id] = read
		}
		c.mu.Unlock()
		if !joined {
			slots <- struct{}{}
			read.tag, read.err = readGitLabPipelineTag(ctx, client, project, id)
			<-slots
			c.mu.Lock()
			delete(state.reads, id)
			if read.err == nil {
				state.tags[id] = read.tag
			}
			c.mu.Unlock()
			close(read.done)
			return read.tag, read.err
		}
		select {
		case <-read.done:
		case <-ctx.Done():
			return false, ctx.Err()
		}
		if read.err == nil || errors.Is(read.err, forgeapi.ErrNotFound) || attempt > 0 {
			return read.tag, read.err
		}
	}
}

// kinds answers each pipeline's RefKind. A merge request ref is a merge
// request's; a pipeline in the tag scope (the latest tag pipeline of its
// ref, which GitLab selects among pipelines flagged tag) is a tag's; one
// on a ref name the scope does not name is a branch's. One on a ref name
// the scope names but not in it is an older run of the tag or a run of a
// branch of the same name, and its kind is the single pipeline's `tag`,
// remembered, joined or read with ctx's admission (readTag). A pipeline
// that read answers 404 for (deleted since the list) has no kind and is
// left out; any other failure fails the listing.
func (c *gitlabPipelineTags) kinds(ctx context.Context, client *forgeapi.Client, repo RepoReference, pipelines []gitlabPipelineRaw, tagRefs map[string]bool, scope map[int64]bool) (map[int64]string, error) {
	key := repo.Host + " " + repo.Project()
	ids := make([]int64, len(pipelines))
	for i, pipeline := range pipelines {
		ids[i] = pipeline.ID
	}
	state, start, known := c.beginListing(key, ids)
	learned := map[int64]bool{}
	defer func() { c.endListing(key, state, start, learned) }()

	kinds := make(map[int64]string, len(pipelines))
	var ambiguous []int64
	for _, pipeline := range pipelines {
		switch {
		case gitlabMergeRequestRef.MatchString(pipeline.Ref):
			kinds[pipeline.ID] = PipelineRefMergeRequest
		case scope[pipeline.ID]:
			known[pipeline.ID] = true
		case !tagRefs[pipeline.Ref]:
			kinds[pipeline.ID] = PipelineRefBranch
		default:
			if _, ok := known[pipeline.ID]; !ok {
				ambiguous = append(ambiguous, pipeline.ID)
			}
		}
	}
	tags := make([]bool, len(ambiguous))
	errs := make([]error, len(ambiguous))
	slots := make(chan struct{}, gitlabPipelineTagReadConcurrency)
	var wg sync.WaitGroup
	for i, id := range ambiguous {
		wg.Go(func() {
			tags[i], errs[i] = c.readTag(ctx, client, repo.Project(), state, id, slots)
		})
	}
	wg.Wait()
	// A deleted pipeline (404) stays unknown and so has no kind.
	var failure error
	for i, id := range ambiguous {
		switch {
		case errs[i] == nil:
			known[id] = tags[i]
		case errors.Is(errs[i], forgeapi.ErrNotFound):
		case failure == nil:
			failure = errs[i]
		}
	}

	for _, pipeline := range pipelines {
		tag, ok := known[pipeline.ID]
		if !ok || kinds[pipeline.ID] != "" {
			continue
		}
		learned[pipeline.ID] = tag
		kinds[pipeline.ID] = PipelineRefBranch
		if tag {
			kinds[pipeline.ID] = PipelineRefTag
		}
	}
	if failure != nil {
		return nil, failure
	}
	return kinds, nil
}

// readGitLabPipelineTag reads one pipeline's tag flag, which GitLab's
// list does not carry.
func readGitLabPipelineTag(ctx context.Context, client *forgeapi.Client, project string, id int64) (bool, error) {
	var pipeline struct {
		ID  int64 `json:"id"`
		Tag bool  `json:"tag"`
	}
	if _, err := client.JSON(ctx, forgeapi.Request{Path: gitlabProjectPath(project) + "/pipelines/" + strconv.FormatInt(id, 10)}, &pipeline); err != nil {
		return false, err
	}
	if pipeline.ID != id {
		return false, fmt.Errorf("GitLab answered pipeline %d for pipeline %d", pipeline.ID, id)
	}
	return pipeline.Tag, nil
}

// gitlabTagPipelines answers the tag names among the listed pipelines'
// refs and the tag scope pipelines read to learn them. GitLab's list does
// not say (only the single pipeline carries `tag`), and its tag scope
// answers only the latest tag pipeline of each tag, so a listed pipeline
// on a ref the scope names but not in it is an older run of the tag or a
// branch of the same name (gitlabPipelineTags.kinds). The scope
// is read without the status filter, since a tag whose latest pipeline
// has another status is a tag still. It is ordered by id, newest first,
// and the latest pipeline of a listed pipeline's tag is no older than
// that pipeline, so the pages stop at the oldest listed id, a short page,
// or gitlabTagScopeMaxPages. A list of one ref reads that ref's scope
// alone (one row at most); a merge request ref, or an empty list, needs
// no read.
func gitlabTagPipelines(ctx context.Context, client *forgeapi.Client, path, ref string, listed []gitlabPipelineRaw) (map[string]bool, []gitlabPipelineRaw, error) {
	if len(listed) == 0 || gitlabMergeRequestRef.MatchString(ref) {
		return nil, nil, nil
	}
	oldest := listed[0].ID
	for _, pipeline := range listed {
		oldest = min(oldest, pipeline.ID)
	}
	query := url.Values{"per_page": {strconv.Itoa(gitlabPageSize)}, "scope": {"tags"}}
	if ref != "" {
		query.Set("ref", ref)
	}
	refs := map[string]bool{}
	var tagged []gitlabPipelineRaw
	page, pages := 1, 0
	err := client.Pages(ctx, forgeapi.Request{Path: path, Query: query}, func(resp *forgeapi.Response) (bool, error) {
		var pagePipelines []gitlabPipelineRaw
		if err := json.Unmarshal(resp.Body, &pagePipelines); err != nil {
			return false, fmt.Errorf("GitLab tag pipelines: decode response: %w", err)
		}
		for _, pipeline := range pagePipelines {
			refs[pipeline.Ref] = true
		}
		tagged = append(tagged, pagePipelines...)
		pages++
		full := len(pagePipelines) == gitlabPageSize
		return full && pagePipelines[len(pagePipelines)-1].ID >= oldest && pages < gitlabTagScopeMaxPages && advanceGitLabPage(resp, &page), nil
	})
	if err != nil {
		return nil, nil, err
	}
	return refs, tagged, nil
}

// ReadPipeline reads one pipeline and its jobs, as ReadPR reads a merge
// request's head pipeline. prev and stepsFor are unused: GitLab jobs have
// no steps.
func (f *gitlabForge) ReadPipeline(ctx context.Context, repo RepoReference, id string, _ *CIPipeline, _ []string) (CIPipeline, error) {
	client, err := f.core.gitlabAPI(repo.Host)
	if err != nil {
		return CIPipeline{}, err
	}
	var pipeline gitlabPipelineRaw
	if _, err := client.JSON(ctx, forgeapi.Request{Path: gitlabProjectPath(repo.Project()) + "/pipelines/" + id}, &pipeline); err != nil {
		return CIPipeline{}, err
	}
	if strconv.FormatInt(pipeline.ID, 10) != id {
		return CIPipeline{}, fmt.Errorf("GitLab answered pipeline %d for pipeline %s", pipeline.ID, id)
	}
	return readGitLabPipeline(ctx, client, repo, &pipeline)
}

// StreamCIJobLog reads a job's raw trace into the writer begin returns
// (see Core.StreamCIJobLog); internal/cilog cleans it. A 404 (the job has
// not started, or does not exist) is ErrCIJobLogNotFound. GitLab honors
// If-None-Match on a trace, running or completed, and Range only on a
// completed one: a running trace answers a range with 200 and the whole
// body.
func (f *gitlabForge) StreamCIJobLog(ctx context.Context, repo RepoReference, req CIJobLogRequest, begin func(CIJobLogStart) (io.Writer, error)) (bool, error) {
	if err := ValidateCIJobID(req.JobID); err != nil {
		return false, err
	}
	client, err := f.core.gitlabAPI(repo.Host)
	if err != nil {
		return false, err
	}
	request := ciLogRequest(gitlabProjectPath(repo.Project())+"/jobs/"+req.JobID+"/trace", req)
	// The trace is plain text.
	request.Header.Set("Accept", "*/*")
	return streamCILog(ctx, client, request, begin)
}

// CILogStreams is true: the trace endpoint returns what the runner
// has uploaded so far for a running job.
func (f *gitlabForge) CILogStreams() bool { return true }
