package git

// Body bounds (forgeapi.Request.MaxBodyBytes) of the forge list reads
// whose page can pass forgeapi's 1 MiB default. Each is sized from pages
// measured on 2026-11-11 with headroom for heavier records
// (docs/references/forge-api-measurements.md#list-page-sizes); none is
// past forgeapi's 8 MiB ceiling. A list absent here measured far under
// the default per 100 records.
const (
	// A workflow run is 13.0 to 14.2 KB (it embeds repository and
	// head_repository, 4.5 KB each); 100 runs measured 1.34 MB. 8 MiB
	// holds 100 runs of 84 KB.
	githubRunsMaxBody = 8 << 20
	// A run's job is about 0.9 KB plus 150 B per step; 4 MiB holds 100
	// jobs of about 270 steps each.
	githubRunJobsMaxBody = 4 << 20
	// A merge request in the full list view averages 19.4 KB and reached
	// 56 KB (its description); 100 merged ones measured 1.95 MB. 8 MiB
	// holds 100 of 84 KB.
	gitlabMRListMaxBody = 8 << 20
	// A discussion averages 3.9 KB and reached 53 KB (its notes' bodies).
	// 8 MiB holds 100 of 84 KB.
	gitlabDiscussionsMaxBody = 8 << 20
	// A commented-on event carries its note: 2.5 KB on average, 4.8 KB at
	// most. 4 MiB holds 100 of 42 KB.
	gitlabEventsMaxBody = 4 << 20
)
