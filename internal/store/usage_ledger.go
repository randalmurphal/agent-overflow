package store

import (
	"database/sql"
	"fmt"
	"strings"
)

// usage_ledger accessors — append-only per-turn token/cost accounting.
//
// One row per (settled turn, model). Rows are written by triage on turn
// settlement and never updated or deleted; thread/project deletion leaves
// them in place so lifetime aggregates stay truthful (the table has no
// foreign keys by design — see migration v14). Values are per-turn deltas
// computed by the provider parsers; summing any slice of rows is safe.

// UsageLedgerRow is one model's share of one settled turn.
type UsageLedgerRow struct {
	CreatedAt                int64   `json:"createdAt"`
	ThreadID                 string  `json:"threadId"`
	ProjectID                string  `json:"projectId"`
	TurnID                   string  `json:"turnId"`
	Provider                 string  `json:"provider"`
	Model                    string  `json:"model"`
	InputTokens              int     `json:"inputTokens"`
	OutputTokens             int     `json:"outputTokens"`
	CacheReadInputTokens     int     `json:"cacheReadInputTokens"`
	CacheCreationInputTokens int     `json:"cacheCreationInputTokens"`
	ReasoningOutputTokens    int     `json:"reasoningOutputTokens"`
	CostUSD                  float64 `json:"costUsd"`
	// CostSource is 'wire' when CostUSD came from the provider (Claude
	// reports cost CLI-side) and 'none' when the row carries no
	// wire-reported cost (Codex has no cost on its wire; claudetui
	// synthesized results carry none). GetUsageStats (app_usage.go)
	// prices 'none' rows at query time from internal/usagecost when the
	// model is recognized; rows whose model isn't in that rate table
	// surface as UnpricedRows so a $ total can be labeled "partial"
	// instead of silently reading as complete.
	CostSource string `json:"costSource"`
}

// AppendUsage inserts the rows in one transaction. Empty input is a no-op.
// Rows with an empty CostSource are stamped 'wire' when CostUSD > 0 and
// 'none' otherwise.
func (s *Store) AppendUsage(rows []UsageLedgerRow) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: usage append begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.Prepare(`INSERT INTO usage_ledger (
        created_at, thread_id, project_id, turn_id, provider, model,
        input_tokens, output_tokens, cache_read_input_tokens,
        cache_creation_input_tokens, reasoning_output_tokens,
        cost_usd, cost_source)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("store: usage append prepare: %w", err)
	}
	defer stmt.Close()

	for _, row := range rows {
		source := row.CostSource
		if source == "" {
			if row.CostUSD > 0 {
				source = "wire"
			} else {
				source = "none"
			}
		}
		if _, err := stmt.Exec(
			row.CreatedAt, row.ThreadID, row.ProjectID, row.TurnID,
			row.Provider, row.Model,
			row.InputTokens, row.OutputTokens, row.CacheReadInputTokens,
			row.CacheCreationInputTokens, row.ReasoningOutputTokens,
			row.CostUSD, source,
		); err != nil {
			return fmt.Errorf("store: usage append insert: %w", err)
		}
	}
	return tx.Commit()
}

// UsageQuery filters and shapes an aggregation over the ledger.
type UsageQuery struct {
	// FromMillis / ToMillis bound created_at (inclusive / exclusive).
	// Zero means unbounded on that side.
	FromMillis int64 `json:"fromMillis"`
	ToMillis   int64 `json:"toMillis"`
	// Optional equality filters.
	ThreadID  string `json:"threadId"`
	ProjectID string `json:"projectId"`
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	// GroupBy selects the bucket dimension: "" (single lifetime bucket),
	// "day", "week", "month" (calendar buckets in the query's timezone),
	// or "model", "provider", "thread", "project".
	GroupBy string `json:"groupBy"`
	// TZOffsetMinutes shifts calendar bucketing east of UTC (e.g. -300
	// for EST). Only meaningful for day/week/month grouping; block math
	// and raw timestamps stay UTC. It is a SINGLE fixed offset applied
	// across the whole query range — DST transitions and rows recorded
	// under a different UTC offset than the caller's "now" are not
	// modeled, so a timestamp within ~1h of local midnight can land in
	// the adjacent calendar day/week/month from what its own wall clock
	// would have shown. Accepted tradeoff (per-row IANA tz bucketing
	// would need each row to know a timezone, not just an offset); it is
	// not a bug callers need to work around, just an edge case to expect.
	// GetUsageStats (app_usage.go) clamps this to [-840, 840] before use.
	TZOffsetMinutes int `json:"tzOffsetMinutes"`
}

// UsageBucket is one aggregated row of a usage query.
//
// QueryUsage alone only populates token totals and TurnCount — it has
// no rate table to price cost_source='none' rows, so CostUSD and
// UnpricedRows are left at their zero value on a raw QueryUsage result.
// GetUsageStats (app_usage.go) is the caller that merges QueryUsage's
// totals with QueryUsageDetail's per-model breakdown priced through
// internal/usagecost, producing the complete CostUSD / UnpricedRows
// pair callers should read.
type UsageBucket struct {
	// Bucket is the group key: a date ("2026-07-03"), ISO 8601 week
	// ("2026-W26"), month ("2026-07"), model/provider/thread/project id,
	// or "" for the lifetime bucket.
	Bucket                   string  `json:"bucket"`
	InputTokens              int64   `json:"inputTokens"`
	OutputTokens             int64   `json:"outputTokens"`
	CacheReadInputTokens     int64   `json:"cacheReadInputTokens"`
	CacheCreationInputTokens int64   `json:"cacheCreationInputTokens"`
	ReasoningOutputTokens    int64   `json:"reasoningOutputTokens"`
	CostUSD                  float64 `json:"costUsd"`
	// TurnCount counts distinct settled turns in the bucket (a turn that
	// used several models is one turn). UnpricedRows counts rows whose
	// model has no known price in the internal/usagecost rate table —
	// when > 0 the bucket's CostUSD is a lower bound, not a total. Set
	// by GetUsageStats, not by QueryUsage (see the struct doc above).
	TurnCount    int64 `json:"turnCount"`
	UnpricedRows int64 `json:"unpricedRows"`
}

// UsageDetailRow is one (bucket, model, cost-source) group of the
// ledger. QueryUsageDetail shares QueryUsage's filters and bucket
// expression, so every row's Bucket matches a Bucket already present in
// the corresponding QueryUsage call's result — GetUsageStats merges the
// two by that key rather than re-deriving bucket boundaries.
type UsageDetailRow struct {
	Bucket                   string `json:"bucket"`
	Model                    string `json:"model"`
	CostSource               string `json:"costSource"`
	InputTokens              int64  `json:"inputTokens"`
	OutputTokens             int64  `json:"outputTokens"`
	CacheReadInputTokens     int64  `json:"cacheReadInputTokens"`
	CacheCreationInputTokens int64  `json:"cacheCreationInputTokens"`
	ReasoningOutputTokens    int64  `json:"reasoningOutputTokens"`
	// CostUSD is SUM(cost_usd) for the group — meaningful only when
	// CostSource is 'wire'; 'none' groups carry 0 here and must be
	// priced from the token sums instead.
	CostUSD float64 `json:"costUsd"`
	// Rows is COUNT(*) for the group, used to attribute UnpricedRows
	// when a 'none' group's model has no entry in the rate table.
	Rows int64 `json:"rows"`
}

// usageBucketExpr returns the SQL expression producing the group key.
// Calendar buckets shift the epoch by the timezone offset before
// formatting so "today" matches the user's wall clock.
//
// tzOffsetMinutes is one fixed offset applied to every row in the
// query, not a per-row IANA timezone lookup. A DST transition (or a
// query spanning rows recorded under a different UTC offset than the
// caller's current "now") means a timestamp within ~1h of local
// midnight can bucket into the adjacent day/week/month rather than the
// one its own wall clock would show. This is an accepted tradeoff, not
// a bug — see the TZOffsetMinutes field doc on UsageQuery.
func usageBucketExpr(groupBy string, tzOffsetMinutes int) (string, error) {
	local := fmt.Sprintf("(created_at / 1000 + %d)", tzOffsetMinutes*60)
	switch groupBy {
	case "":
		return `''`, nil
	case "day":
		return fmt.Sprintf(`strftime('%%Y-%%m-%%d', %s, 'unixepoch')`, local), nil
	case "week":
		// %G/%V (ISO 8601 year + week) rather than %Y/%W: %W is not ISO
		// (week 00 for the first partial week of January) and pairing it
		// with %Y mislabels the year on a year-straddling week (e.g.
		// 2027-01-01 is ISO week 2026-W53, but %Y-W%W reports 2027-W00).
		// Verified against modernc.org/sqlite 3.51.3, which supports both
		// specifiers.
		return fmt.Sprintf(`strftime('%%G-W%%V', %s, 'unixepoch')`, local), nil
	case "month":
		return fmt.Sprintf(`strftime('%%Y-%%m', %s, 'unixepoch')`, local), nil
	case "model":
		return "model", nil
	case "provider":
		return "provider", nil
	case "thread":
		return "thread_id", nil
	case "project":
		return "project_id", nil
	default:
		return "", fmt.Errorf(
			"store: usage query groupBy %q is not supported (expected \"\", day, week, month, model, provider, thread, or project)",
			groupBy)
	}
}

// usageWhereFilters builds the WHERE-clause fragments and bound args
// shared by QueryUsage and QueryUsageDetail. Both aggregate the same
// table under the same filter set, so a new filter field must apply to
// both — keep this the single place that translates UsageQuery into
// SQL predicates.
func usageWhereFilters(q UsageQuery) ([]string, []any) {
	where := make([]string, 0, 6)
	args := make([]any, 0, 6)
	appendFilter := func(clause string, value any, active bool) {
		if active {
			where = append(where, clause)
			args = append(args, value)
		}
	}
	appendFilter("created_at >= ?", q.FromMillis, q.FromMillis > 0)
	appendFilter("created_at < ?", q.ToMillis, q.ToMillis > 0)
	appendFilter("thread_id = ?", q.ThreadID, q.ThreadID != "")
	appendFilter("project_id = ?", q.ProjectID, q.ProjectID != "")
	appendFilter("provider = ?", q.Provider, q.Provider != "")
	appendFilter("model = ?", q.Model, q.Model != "")
	return where, args
}

// QueryUsage aggregates the ledger per the query into token totals and
// turn counts only. Buckets order by key ascending, so calendar
// groupings come back chronologically. CostUSD and UnpricedRows are
// left at their zero value here — GetUsageStats (app_usage.go) is the
// only caller that populates them, by merging in QueryUsageDetail's
// per-model breakdown priced through internal/usagecost. See the
// UsageBucket doc comment.
func (s *Store) QueryUsage(q UsageQuery) ([]UsageBucket, error) {
	bucketExpr, err := usageBucketExpr(q.GroupBy, q.TZOffsetMinutes)
	if err != nil {
		return nil, err
	}
	where, args := usageWhereFilters(q)

	query := fmt.Sprintf(`SELECT %s AS bucket,
        SUM(input_tokens), SUM(output_tokens),
        SUM(cache_read_input_tokens), SUM(cache_creation_input_tokens),
        SUM(reasoning_output_tokens),
        COUNT(DISTINCT CASE WHEN turn_id <> '' THEN turn_id END)
        FROM usage_ledger`, bucketExpr)
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " GROUP BY bucket ORDER BY bucket ASC"

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: usage query: %w", err)
	}
	defer rows.Close()

	buckets := make([]UsageBucket, 0, 16)
	for rows.Next() {
		var b UsageBucket
		var bucket sql.NullString
		if err := rows.Scan(
			&bucket,
			&b.InputTokens, &b.OutputTokens,
			&b.CacheReadInputTokens, &b.CacheCreationInputTokens,
			&b.ReasoningOutputTokens,
			&b.TurnCount,
		); err != nil {
			return nil, fmt.Errorf("store: usage query scan: %w", err)
		}
		b.Bucket = bucket.String
		buckets = append(buckets, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: usage query rows: %w", err)
	}
	return buckets, nil
}

// QueryUsageDetail aggregates the ledger per the query like QueryUsage,
// but additionally splits each bucket by (model, cost_source). This is
// the granularity GetUsageStats needs to price cost_source='none' rows
// per model from internal/usagecost without re-deriving bucket
// boundaries — see UsageDetailRow.
func (s *Store) QueryUsageDetail(q UsageQuery) ([]UsageDetailRow, error) {
	bucketExpr, err := usageBucketExpr(q.GroupBy, q.TZOffsetMinutes)
	if err != nil {
		return nil, err
	}
	where, args := usageWhereFilters(q)

	query := fmt.Sprintf(`SELECT %s AS bucket, model, cost_source,
        SUM(input_tokens), SUM(output_tokens),
        SUM(cache_read_input_tokens), SUM(cache_creation_input_tokens),
        SUM(reasoning_output_tokens), SUM(cost_usd), COUNT(*)
        FROM usage_ledger`, bucketExpr)
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " GROUP BY bucket, model, cost_source ORDER BY bucket ASC, model ASC"

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: usage detail query: %w", err)
	}
	defer rows.Close()

	details := make([]UsageDetailRow, 0, 16)
	for rows.Next() {
		var d UsageDetailRow
		var bucket sql.NullString
		if err := rows.Scan(
			&bucket, &d.Model, &d.CostSource,
			&d.InputTokens, &d.OutputTokens,
			&d.CacheReadInputTokens, &d.CacheCreationInputTokens,
			&d.ReasoningOutputTokens, &d.CostUSD, &d.Rows,
		); err != nil {
			return nil, fmt.Errorf("store: usage detail scan: %w", err)
		}
		d.Bucket = bucket.String
		details = append(details, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: usage detail rows: %w", err)
	}
	return details, nil
}
