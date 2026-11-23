package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/trimbar/trimbar/internal/anthropic"
)

// RequestLog represents a single persisted Anthropic proxy request record.
type RequestLog struct {
	ID          int64
	Timestamp   time.Time
	SessionID   string
	RequestID   string
	Method      string
	Path        string
	StatusCode  int
	Model       string
	AgentRole   string
	IsStreaming bool

	GitRepo   string
	GitBranch string
	CWD       string

	InputTokens         int64
	OutputTokens        int64
	CacheCreationTokens int64
	CacheReadTokens     int64

	CostUSD            float64
	CacheSavingsUSD    float64
	NetCacheSavingsUSD float64
	ShadowSavingsUSD   float64

	LatencyMS int64
	TTFBMS    int64

	CacheBreakpoints int
	CacheMissAnomaly bool
	CacheMissReason  string

	SystemEstTokens      int
	ToolsEstTokens       int
	MessagesEstTokens    int
	ToolResultsEstTokens int

	DuplicateReadCount        int
	DuplicateReadWastedTokens int
	DuplicateReads            []anthropic.DuplicateRead

	ANSISequenceCount int
	ANSIWastedTokens  int
	TotalWastedTokens int

	ErrorBody string
}

// TotalInputTokens returns uncached + cache creation + cache read tokens.
func (r RequestLog) TotalInputTokens() int64 {
	return r.InputTokens + r.CacheCreationTokens + r.CacheReadTokens
}

// CacheHitRatio returns cache read share in [0, 1].
func (r RequestLog) CacheHitRatio() float64 {
	total := r.TotalInputTokens()
	if total <= 0 {
		return 0
	}
	return float64(r.CacheReadTokens) / float64(total)
}

// StatsFilter narrows aggregated stats and inspect queries.
type StatsFilter struct {
	Since     time.Time
	Repo      string
	Branch    string
	SessionID string
	Limit     int
}

// ModelStats aggregates request volume, tokens, cache hit ratio, and spend per model.
type ModelStats struct {
	Model               string  `json:"model"`
	Requests            int64   `json:"requests"`
	InputTokens         int64   `json:"input_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	CacheHitRatio       float64 `json:"cache_hit_ratio"`
	CostUSD             float64 `json:"cost_usd"`
}

// RepoBranchStats aggregates telemetry per Git repository & branch.
type RepoBranchStats struct {
	Repo              string  `json:"repo"`
	Branch            string  `json:"branch"`
	Requests          int64   `json:"requests"`
	MainAgentRequests int64   `json:"main_agent_requests"`
	SubagentRequests  int64   `json:"subagent_requests"`
	CacheHitRatio     float64 `json:"cache_hit_ratio"`
	CostUSD           float64 `json:"cost_usd"`
	WastedTokens      int64   `json:"wasted_tokens"`
}

// StatsSummary is returned by Repository.GetStats for `trimbar stats`.
type StatsSummary struct {
	TotalRequests             int64             `json:"total_requests"`
	MainAgentRequests         int64             `json:"main_agent_requests"`
	SubagentRequests          int64             `json:"subagent_requests"`
	ErrorRequests             int64             `json:"error_requests"`
	InputTokens               int64             `json:"input_tokens"`
	OutputTokens              int64             `json:"output_tokens"`
	CacheCreationTokens       int64             `json:"cache_creation_tokens"`
	CacheReadTokens           int64             `json:"cache_read_tokens"`
	CacheHitRatio             float64           `json:"cache_hit_ratio"`
	TotalCostUSD              float64           `json:"total_cost_usd"`
	TotalCacheSavingsUSD      float64           `json:"total_cache_savings_usd"`
	TotalNetCacheSavingsUSD   float64           `json:"total_net_cache_savings_usd"`
	TotalShadowSavingsUSD     float64           `json:"total_shadow_savings_usd"`
	AvgLatencyMS              float64           `json:"avg_latency_ms"`
	CacheMissAnomalies        int64             `json:"cache_miss_anomalies"`
	DuplicateReadCalls        int64             `json:"duplicate_read_calls"`
	DuplicateReadWastedTokens int64             `json:"duplicate_read_wasted_tokens"`
	ANSIWastedTokens          int64             `json:"ansi_wasted_tokens"`
	TotalWastedTokens         int64             `json:"total_wasted_tokens"`
	ByModel                   []ModelStats      `json:"by_model"`
	ByRepoBranch              []RepoBranchStats `json:"by_repo_branch"`
}

// DuplicateReadHotspot aggregates repeated Read waste for a file across inspected turns.
type DuplicateReadHotspot struct {
	FilePath       string `json:"file_path"`
	RedundantReads int    `json:"redundant_reads"`
	WastedTokens   int    `json:"wasted_tokens"`
	Occurrences    int    `json:"occurrences"`
}

// ContextComposition summarizes average estimated token distribution across prompt sections.
type ContextComposition struct {
	SystemTokens      int64 `json:"system_tokens"`
	ToolsTokens       int64 `json:"tools_tokens"`
	ToolResultsTokens int64 `json:"tool_results_tokens"`
	OtherMsgTokens    int64 `json:"other_msg_tokens"`
	TotalEstTokens    int64 `json:"total_est_tokens"`
}

// InspectReport is returned by Repository.GetInspect for `trimbar inspect`.
type InspectReport struct {
	Turns              []RequestLog           `json:"turns"`
	Context            ContextComposition     `json:"context"`
	DuplicateHotspots  []DuplicateReadHotspot `json:"duplicate_hotspots"`
	CacheMissAnomalies []RequestLog           `json:"cache_miss_anomalies"`
	TotalANSISequences int64                  `json:"total_ansi_sequences"`
	TotalANSIWasted    int64                  `json:"total_ansi_wasted_tokens"`
}

// Repository provides persistence and analytical queries over the SQLite requests table.
type Repository struct {
	db *DB
}

// NewRepository wraps db with a Repository.
func NewRepository(db *DB) *Repository {
	return &Repository{db: db}
}

// InsertRequest stores a RequestLog entry and returns its assigned row ID.
func (r *Repository) InsertRequest(ctx context.Context, rec *RequestLog) (int64, error) {
	if rec == nil {
		return 0, fmt.Errorf("request log is nil")
	}
	ts := rec.Timestamp.UTC()
	if ts.IsZero() {
		ts = time.Now().UTC()
	}

	dupJSON := "[]"
	if len(rec.DuplicateReads) > 0 {
		if raw, err := json.Marshal(rec.DuplicateReads); err == nil {
			dupJSON = string(raw)
		}
	}

	agentRole := rec.AgentRole
	if agentRole == "" {
		agentRole = anthropic.AgentRoleMain
	}

	const q = `
INSERT INTO requests (
	timestamp, session_id, request_id, method, path, status_code, model, agent_role, is_streaming,
	git_repo, git_branch, cwd,
	input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
	cost_usd, cache_savings_usd, net_cache_savings_usd, shadow_savings_usd,
	latency_ms, ttfb_ms,
	cache_breakpoints, cache_miss_anomaly, cache_miss_reason,
	system_est_tokens, tools_est_tokens, messages_est_tokens, tool_results_est_tokens,
	duplicate_read_count, duplicate_read_wasted_tokens, duplicate_reads_json,
	ansi_sequence_count, ansi_wasted_tokens, total_wasted_tokens,
	error_body
) VALUES (
	?, ?, ?, ?, ?, ?, ?, ?, ?,
	?, ?, ?,
	?, ?, ?, ?,
	?, ?, ?, ?,
	?, ?,
	?, ?, ?,
	?, ?, ?, ?,
	?, ?, ?,
	?, ?, ?,
	?
);`

	res, err := r.db.sqlDB.ExecContext(ctx, q,
		ts.Format(time.RFC3339Nano),
		rec.SessionID,
		rec.RequestID,
		rec.Method,
		rec.Path,
		rec.StatusCode,
		rec.Model,
		agentRole,
		boolToInt(rec.IsStreaming),
		rec.GitRepo,
		rec.GitBranch,
		rec.CWD,
		rec.InputTokens,
		rec.OutputTokens,
		rec.CacheCreationTokens,
		rec.CacheReadTokens,
		rec.CostUSD,
		rec.CacheSavingsUSD,
		rec.NetCacheSavingsUSD,
		rec.ShadowSavingsUSD,
		rec.LatencyMS,
		rec.TTFBMS,
		rec.CacheBreakpoints,
		boolToInt(rec.CacheMissAnomaly),
		rec.CacheMissReason,
		rec.SystemEstTokens,
		rec.ToolsEstTokens,
		rec.MessagesEstTokens,
		rec.ToolResultsEstTokens,
		rec.DuplicateReadCount,
		rec.DuplicateReadWastedTokens,
		dupJSON,
		rec.ANSISequenceCount,
		rec.ANSIWastedTokens,
		rec.TotalWastedTokens,
		rec.ErrorBody,
	)
	if err != nil {
		return 0, fmt.Errorf("insert request: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	rec.ID = id
	return id, nil
}

// LatestUsageForSession fetches the most recent successful main-agent usage for sessionID,
// enabling real-time Cache Miss Anomaly detection mid-session.
func (r *Repository) LatestUsageForSession(ctx context.Context, sessionID string) (*anthropic.Usage, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, nil
	}
	const q = `
SELECT input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens
FROM requests
WHERE session_id = ?
  AND status_code >= 200 AND status_code < 300
  AND agent_role = 'main'
  AND (input_tokens + cache_creation_tokens + cache_read_tokens) > 0
ORDER BY id DESC
LIMIT 1;`

	var u anthropic.Usage
	err := r.db.sqlDB.QueryRowContext(ctx, q, sessionID).Scan(
		&u.InputTokens,
		&u.OutputTokens,
		&u.CacheCreationInputTokens,
		&u.CacheReadInputTokens,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query latest session usage: %w", err)
	}
	return &u, nil
}

// GetStats computes aggregated metrics, per-model breakdown, and per-repo/branch breakdown.
func (r *Repository) GetStats(ctx context.Context, filter StatsFilter) (*StatsSummary, error) {
	where, args := buildWhereClause(filter)

	summary := &StatsSummary{}
	aggQ := fmt.Sprintf(`
SELECT
	COUNT(*),
	COALESCE(SUM(CASE WHEN agent_role = 'main' THEN 1 ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN agent_role = 'subagent' THEN 1 ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN status_code < 200 OR status_code >= 300 THEN 1 ELSE 0 END), 0),
	COALESCE(SUM(input_tokens), 0),
	COALESCE(SUM(output_tokens), 0),
	COALESCE(SUM(cache_creation_tokens), 0),
	COALESCE(SUM(cache_read_tokens), 0),
	COALESCE(SUM(cost_usd), 0),
	COALESCE(SUM(cache_savings_usd), 0),
	COALESCE(SUM(net_cache_savings_usd), 0),
	COALESCE(SUM(shadow_savings_usd), 0),
	COALESCE(AVG(latency_ms), 0),
	COALESCE(SUM(CASE WHEN cache_miss_anomaly = 1 THEN 1 ELSE 0 END), 0),
	COALESCE(SUM(duplicate_read_count), 0),
	COALESCE(SUM(duplicate_read_wasted_tokens), 0),
	COALESCE(SUM(ansi_wasted_tokens), 0),
	COALESCE(SUM(total_wasted_tokens), 0)
FROM requests %s;`, where)

	if err := r.db.sqlDB.QueryRowContext(ctx, aggQ, args...).Scan(
		&summary.TotalRequests,
		&summary.MainAgentRequests,
		&summary.SubagentRequests,
		&summary.ErrorRequests,
		&summary.InputTokens,
		&summary.OutputTokens,
		&summary.CacheCreationTokens,
		&summary.CacheReadTokens,
		&summary.TotalCostUSD,
		&summary.TotalCacheSavingsUSD,
		&summary.TotalNetCacheSavingsUSD,
		&summary.TotalShadowSavingsUSD,
		&summary.AvgLatencyMS,
		&summary.CacheMissAnomalies,
		&summary.DuplicateReadCalls,
		&summary.DuplicateReadWastedTokens,
		&summary.ANSIWastedTokens,
		&summary.TotalWastedTokens,
	); err != nil {
		return nil, fmt.Errorf("query stats aggregate: %w", err)
	}

	totalPrompt := summary.InputTokens + summary.CacheCreationTokens + summary.CacheReadTokens
	if totalPrompt > 0 {
		summary.CacheHitRatio = float64(summary.CacheReadTokens) / float64(totalPrompt)
	}

	// Breakdown by model
	modelQ := fmt.Sprintf(`
SELECT
	CASE WHEN model = '' THEN '(unknown)' ELSE model END AS m,
	COUNT(*),
	COALESCE(SUM(input_tokens), 0),
	COALESCE(SUM(output_tokens), 0),
	COALESCE(SUM(cache_creation_tokens), 0),
	COALESCE(SUM(cache_read_tokens), 0),
	COALESCE(SUM(cost_usd), 0)
FROM requests %s
GROUP BY m
ORDER BY SUM(cost_usd) DESC, COUNT(*) DESC;`, where)

	rows, err := r.db.sqlDB.QueryContext(ctx, modelQ, args...)
	if err != nil {
		return nil, fmt.Errorf("query model stats: %w", err)
	}
	for rows.Next() {
		var ms ModelStats
		if err := rows.Scan(
			&ms.Model,
			&ms.Requests,
			&ms.InputTokens,
			&ms.OutputTokens,
			&ms.CacheCreationTokens,
			&ms.CacheReadTokens,
			&ms.CostUSD,
		); err != nil {
			_ = rows.Close()
			return nil, err
		}
		tot := ms.InputTokens + ms.CacheCreationTokens + ms.CacheReadTokens
		if tot > 0 {
			ms.CacheHitRatio = float64(ms.CacheReadTokens) / float64(tot)
		}
		summary.ByModel = append(summary.ByModel, ms)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Breakdown by git repo + branch
	repoQ := fmt.Sprintf(`
SELECT
	CASE WHEN git_repo = '' THEN '(no-git)' ELSE git_repo END AS r,
	CASE WHEN git_branch = '' THEN '-' ELSE git_branch END AS b,
	COUNT(*),
	COALESCE(SUM(CASE WHEN agent_role = 'main' THEN 1 ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN agent_role = 'subagent' THEN 1 ELSE 0 END), 0),
	COALESCE(SUM(input_tokens + cache_creation_tokens + cache_read_tokens), 0),
	COALESCE(SUM(cache_read_tokens), 0),
	COALESCE(SUM(cost_usd), 0),
	COALESCE(SUM(total_wasted_tokens), 0)
FROM requests %s
GROUP BY r, b
ORDER BY SUM(cost_usd) DESC, COUNT(*) DESC;`, where)

	repoRows, err := r.db.sqlDB.QueryContext(ctx, repoQ, args...)
	if err != nil {
		return nil, fmt.Errorf("query repo stats: %w", err)
	}
	for repoRows.Next() {
		var rs RepoBranchStats
		var totIn, readIn int64
		if err := repoRows.Scan(
			&rs.Repo,
			&rs.Branch,
			&rs.Requests,
			&rs.MainAgentRequests,
			&rs.SubagentRequests,
			&totIn,
			&readIn,
			&rs.CostUSD,
			&rs.WastedTokens,
		); err != nil {
			_ = repoRows.Close()
			return nil, err
		}
		if totIn > 0 {
			rs.CacheHitRatio = float64(readIn) / float64(totIn)
		}
		summary.ByRepoBranch = append(summary.ByRepoBranch, rs)
	}
	_ = repoRows.Close()
	if err := repoRows.Err(); err != nil {
		return nil, err
	}

	return summary, nil
}

// GetInspect retrieves detailed recent turns, context composition, duplicate Read hotspots,
// and cache miss anomalies for `trimbar inspect`.
func (r *Repository) GetInspect(ctx context.Context, filter StatsFilter) (*InspectReport, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 20
	}

	where, args := buildWhereClause(filter)
	args = append(args, limit)

	q := fmt.Sprintf(`
SELECT
	id, timestamp, session_id, request_id, method, path, status_code, model, agent_role, is_streaming,
	git_repo, git_branch, cwd,
	input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
	cost_usd, cache_savings_usd, net_cache_savings_usd, shadow_savings_usd,
	latency_ms, ttfb_ms,
	cache_breakpoints, cache_miss_anomaly, cache_miss_reason,
	system_est_tokens, tools_est_tokens, messages_est_tokens, tool_results_est_tokens,
	duplicate_read_count, duplicate_read_wasted_tokens, duplicate_reads_json,
	ansi_sequence_count, ansi_wasted_tokens, total_wasted_tokens,
	error_body
FROM requests %s
ORDER BY id DESC
LIMIT ?;`, where)

	rows, err := r.db.sqlDB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query inspect turns: %w", err)
	}
	defer rows.Close()

	report := &InspectReport{}
	hotspots := make(map[string]*DuplicateReadHotspot)

	for rows.Next() {
		var (
			rec         RequestLog
			tsStr       string
			isStreamInt int
			anomalyInt  int
			dupJSON     string
		)
		if err := rows.Scan(
			&rec.ID, &tsStr, &rec.SessionID, &rec.RequestID, &rec.Method, &rec.Path,
			&rec.StatusCode, &rec.Model, &rec.AgentRole, &isStreamInt,
			&rec.GitRepo, &rec.GitBranch, &rec.CWD,
			&rec.InputTokens, &rec.OutputTokens, &rec.CacheCreationTokens, &rec.CacheReadTokens,
			&rec.CostUSD, &rec.CacheSavingsUSD, &rec.NetCacheSavingsUSD, &rec.ShadowSavingsUSD,
			&rec.LatencyMS, &rec.TTFBMS,
			&rec.CacheBreakpoints, &anomalyInt, &rec.CacheMissReason,
			&rec.SystemEstTokens, &rec.ToolsEstTokens, &rec.MessagesEstTokens, &rec.ToolResultsEstTokens,
			&rec.DuplicateReadCount, &rec.DuplicateReadWastedTokens, &dupJSON,
			&rec.ANSISequenceCount, &rec.ANSIWastedTokens, &rec.TotalWastedTokens,
			&rec.ErrorBody,
		); err != nil {
			return nil, err
		}

		if parsedTS, err := time.Parse(time.RFC3339Nano, tsStr); err == nil {
			rec.Timestamp = parsedTS
		}
		rec.IsStreaming = isStreamInt == 1
		rec.CacheMissAnomaly = anomalyInt == 1
		if dupJSON != "" && dupJSON != "[]" {
			_ = json.Unmarshal([]byte(dupJSON), &rec.DuplicateReads)
		}

		report.Turns = append(report.Turns, rec)
		if rec.CacheMissAnomaly {
			report.CacheMissAnomalies = append(report.CacheMissAnomalies, rec)
		}

		report.TotalANSISequences += int64(rec.ANSISequenceCount)
		report.TotalANSIWasted += int64(rec.ANSIWastedTokens)

		// Context composition (aggregate across inspected turns)
		report.Context.SystemTokens += int64(rec.SystemEstTokens)
		report.Context.ToolsTokens += int64(rec.ToolsEstTokens)
		report.Context.ToolResultsTokens += int64(rec.ToolResultsEstTokens)
		otherMsg := rec.MessagesEstTokens - rec.ToolResultsEstTokens
		if otherMsg < 0 {
			otherMsg = 0
		}
		report.Context.OtherMsgTokens += int64(otherMsg)

		for _, dup := range rec.DuplicateReads {
			h, ok := hotspots[dup.FilePath]
			if !ok {
				h = &DuplicateReadHotspot{FilePath: dup.FilePath}
				hotspots[dup.FilePath] = h
			}
			h.RedundantReads += dup.RedundantCount
			h.WastedTokens += dup.WastedEstTokens
			h.Occurrences++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	report.Context.TotalEstTokens = report.Context.SystemTokens +
		report.Context.ToolsTokens +
		report.Context.ToolResultsTokens +
		report.Context.OtherMsgTokens

	for _, h := range hotspots {
		report.DuplicateHotspots = append(report.DuplicateHotspots, *h)
	}
	sort.Slice(report.DuplicateHotspots, func(i, j int) bool {
		if report.DuplicateHotspots[i].WastedTokens != report.DuplicateHotspots[j].WastedTokens {
			return report.DuplicateHotspots[i].WastedTokens > report.DuplicateHotspots[j].WastedTokens
		}
		return report.DuplicateHotspots[i].RedundantReads > report.DuplicateHotspots[j].RedundantReads
	})

	return report, nil
}

func buildWhereClause(f StatsFilter) (string, []any) {
	var clauses []string
	var args []any

	if !f.Since.IsZero() {
		clauses = append(clauses, "timestamp >= ?")
		args = append(args, f.Since.UTC().Format(time.RFC3339Nano))
	}
	if strings.TrimSpace(f.Repo) != "" {
		clauses = append(clauses, "git_repo = ?")
		args = append(args, strings.TrimSpace(f.Repo))
	}
	if strings.TrimSpace(f.Branch) != "" {
		clauses = append(clauses, "git_branch = ?")
		args = append(args, strings.TrimSpace(f.Branch))
	}
	if strings.TrimSpace(f.SessionID) != "" {
		clauses = append(clauses, "session_id = ?")
		args = append(args, strings.TrimSpace(f.SessionID))
	}

	if len(clauses) == 0 {
		return "", nil
	}
	return "WHERE " + strings.Join(clauses, " AND "), args
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
