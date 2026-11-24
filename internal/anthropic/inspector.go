package anthropic

import (
	"encoding/json"
	"path"
	"regexp"
	"strings"
)

const (
	AgentRoleMain     = "main"
	AgentRoleSubagent = "subagent"
)

// ansiEscapeRegex matches standard ANSI CSI sequences (colors, cursor movement, erase)
// and OSC sequences (terminal titles, hyperlinks).
var ansiEscapeRegex = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]|\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)")

// DuplicateRead describes redundant Read tool calls targeting the same file range
// without an intervening modification to that file.
type DuplicateRead struct {
	FilePath        string
	Offset          int
	Limit           int
	Count           int
	RedundantCount  int
	ToolUseIDs      []string
	WastedBytes     int
	WastedEstTokens int
}

// ANSIDetection summarizes ANSI escape code pollution inside tool_result payloads.
type ANSIDetection struct {
	SequenceCount   int
	TotalBytes      int
	EstTokens       int
	AffectedResults int
	ToolUseIDs      []string
}

// CacheMissAnomaly describes a sudden mid-session drop in prompt cache hit rate.
type CacheMissAnomaly struct {
	PreviousHitRatio   float64
	CurrentHitRatio    float64
	HitRatioDrop       float64
	CacheCreationSpike int64
	Reason             string
}

// InspectionResult summarizes context structure, prompt caching markers, agent role,
// and context waste detected in an Anthropic /v1/messages request.
type InspectionResult struct {
	Model             string
	Stream            bool
	AgentRole         string
	MessageCount      int
	ToolCount         int
	SystemPromptText  string
	FirstUserText     string
	MetadataUserID    string
	MetadataSessionID string
	ToolFilePaths     []string

	SystemBytes          int
	SystemEstTokens      int
	ToolsBytes           int
	ToolsEstTokens       int
	MessagesBytes        int
	MessagesEstTokens    int
	ToolResultsBytes     int
	ToolResultsEstTokens int
	TotalEstTokens       int

	CacheBreakpoints      int
	CacheControlLocations []string
	HasPromptCaching      bool
	PostCacheToolBytes    int // Exact tool_result bytes strictly after the last cache_control boundary

	DuplicateReads              []DuplicateRead
	DuplicateReadRedundantCalls int
	DuplicateReadWastedBytes    int
	DuplicateReadWastedTokens   int

	ANSI ANSIDetection

	TotalWastedBytes     int
	TotalWastedEstTokens int
}

// InspectRequest parses a raw /v1/messages JSON payload and runs full context inspection.
func InspectRequest(body []byte) (*InspectionResult, error) {
	var req MessageRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	return InspectParsedRequest(&req), nil
}

// InspectParsedRequest inspects an already-decoded MessageRequest.
func InspectParsedRequest(req *MessageRequest) *InspectionResult {
	if req == nil {
		return &InspectionResult{}
	}

	res := &InspectionResult{
		Model:            req.Model,
		Stream:           req.Stream,
		AgentRole:        ClassifyAgentRole(req),
		MessageCount:     len(req.Messages),
		ToolCount:        len(req.Tools),
		SystemPromptText: req.System.Text(),
	}
	if req.Metadata != nil {
		if v, ok := req.Metadata["user_id"].(string); ok {
			res.MetadataUserID = v
		}
		if v, ok := req.Metadata["session_id"].(string); ok {
			res.MetadataSessionID = v
		}
	}
	for _, msg := range req.Messages {
		if msg.Role == "user" {
			var parts []string
			for _, b := range msg.Content.Blocks {
				if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
					parts = append(parts, b.Text)
				}
			}
			if len(parts) > 0 {
				res.FirstUserText = strings.Join(parts, "\n")
				break
			}
		}
	}

	cacheLocSeen := make(map[string]bool)
	recordCache := func(loc string) {
		res.CacheBreakpoints++
		res.HasPromptCaching = true
		if !cacheLocSeen[loc] {
			cacheLocSeen[loc] = true
			res.CacheControlLocations = append(res.CacheControlLocations, loc)
		}
	}

	// 1. Inspect top-level automatic cache_control and system prompt
	if req.CacheControl != nil && req.CacheControl.Type != "" {
		recordCache("top_level")
	}
	for _, b := range req.System.Blocks {
		res.SystemBytes += len(b.Text)
		if b.CacheControl != nil && b.CacheControl.Type != "" {
			recordCache("system")
		}
	}
	res.SystemEstTokens = estimateTokens(res.SystemBytes)

	// 2. Inspect tool definitions
	for _, t := range req.Tools {
		res.ToolsBytes += len(t.Name) + len(t.Description) + len(t.InputSchema)
		if t.CacheControl != nil && t.CacheControl.Type != "" {
			recordCache("tools")
		}
	}
	res.ToolsEstTokens = estimateTokens(res.ToolsBytes)

	// Compute the exact frozen cache_control boundary so we can measure real
	// post-cache tool_result bytes eligible for trimming even in Shadow Mode.
	frozenMsgIdx, frozenBlockIdx := findFrozenMessageBoundary(req)

	// 3. Index tool_result payloads by tool_use_id for accurate waste attribution,
	//    collect file paths from tool_use calls for gitmeta fallback,
	//    measure post_cache_tool_bytes, and inspect ANSI escape noise + message sizes.
	toolResultBytesByID := make(map[string]int)
	toolPathSeen := make(map[string]bool)
	for msgIdx, msg := range req.Messages {
		for blockIdx, block := range msg.Content.Blocks {
			if block.CacheControl != nil && block.CacheControl.Type != "" {
				recordCache("messages")
			}

			switch block.Type {
			case "text":
				res.MessagesBytes += len(block.Text)
			case "thinking":
				res.MessagesBytes += len(block.Thinking)
			case "tool_use":
				res.MessagesBytes += len(block.Name) + len(block.Input)
				if len(res.ToolFilePaths) < 8 {
					if p := extractToolUsePath(block.Name, block.Input); p != "" && !toolPathSeen[p] {
						toolPathSeen[p] = true
						res.ToolFilePaths = append(res.ToolFilePaths, p)
					}
				}
			case "tool_result":
				resultLen := block.Content.ByteLen()
				res.MessagesBytes += resultLen
				res.ToolResultsBytes += resultLen
				if isAfterFrozenBoundary(msgIdx, blockIdx, frozenMsgIdx, frozenBlockIdx) {
					res.PostCacheToolBytes += resultLen
				}
				if block.ToolUseID != "" {
					toolResultBytesByID[block.ToolUseID] += resultLen
				}
				inspectANSIInBlock(block, &res.ANSI)
			}
		}
	}

	res.MessagesEstTokens = estimateTokens(res.MessagesBytes)
	res.ToolResultsEstTokens = estimateTokens(res.ToolResultsBytes)
	res.TotalEstTokens = res.SystemEstTokens + res.ToolsEstTokens + res.MessagesEstTokens

	// 4. Detect duplicate Read tool calls across conversation history
	detectDuplicateReads(req.Messages, toolResultBytesByID, res)

	// 5. Finalize ANSI token estimate and total waste summary
	if res.ANSI.TotalBytes > 0 {
		res.ANSI.EstTokens = estimateTokensMin1(res.ANSI.TotalBytes)
	}
	res.TotalWastedBytes = res.DuplicateReadWastedBytes + res.ANSI.TotalBytes
	res.TotalWastedEstTokens = res.DuplicateReadWastedTokens + res.ANSI.EstTokens

	return res
}

// ClassifyAgentRole distinguishes Main Agent turns (Sonnet/Opus interactive loops)
// from Claude Code background sub-agents (e.g., Haiku helpers, read-only Explore/Search
// sub-agents with Read/Grep/Glob and no Edit/Write tools, or tiny title/classifier calls).
func ClassifyAgentRole(req *MessageRequest) string {
	if req == nil {
		return AgentRoleMain
	}
	modelLower := strings.ToLower(req.Model)
	if strings.Contains(modelLower, "haiku") {
		return AgentRoleSubagent
	}

	// Signature of Claude Code's read-only exploration/search sub-agents (even on Sonnet):
	// declares search/read tools (Read/Grep/Glob) without any file mutation tools (Edit/Write).
	if len(req.Tools) > 0 {
		hasReadSearch := false
		hasMutate := false
		for _, t := range req.Tools {
			switch strings.ToLower(strings.TrimSpace(t.Name)) {
			case "read", "grep", "glob", "ls", "read_file", "grep_search":
				hasReadSearch = true
			case "edit", "write", "multiedit", "notebookedit", "edit_file", "write_file":
				hasMutate = true
			}
		}
		if hasReadSearch && !hasMutate {
			return AgentRoleSubagent
		}
	}

	// Utility calls with no tools and tiny output caps (e.g. topic/title classifiers)
	if len(req.Tools) == 0 && req.MaxTokens > 0 && req.MaxTokens <= 512 && len(req.Messages) <= 2 {
		return AgentRoleSubagent
	}
	return AgentRoleMain
}

func extractToolUsePath(toolName string, rawInput json.RawMessage) string {
	switch strings.ToLower(strings.TrimSpace(toolName)) {
	case "read", "read_file", "glob", "grep", "edit", "write", "multiedit", "notebookedit", "edit_file", "write_file":
		var in struct {
			FilePath string `json:"file_path"`
			Path     string `json:"path"`
		}
		if err := json.Unmarshal(rawInput, &in); err != nil {
			return ""
		}
		if p := strings.TrimSpace(in.FilePath); p != "" {
			return p
		}
		return strings.TrimSpace(in.Path)
	}
	return ""
}

// DetectANSI inspects a string for ANSI escape sequences and returns (sequenceCount, totalANSIBytes).
func DetectANSI(s string) (int, int) {
	if !strings.ContainsRune(s, '\x1b') {
		return 0, 0
	}
	matches := ansiEscapeRegex.FindAllStringIndex(s, -1)
	if len(matches) == 0 {
		return 0, 0
	}
	totalBytes := 0
	for _, m := range matches {
		totalBytes += m[1] - m[0]
	}
	return len(matches), totalBytes
}

// StripANSI removes all ANSI CSI and OSC escape sequences from s.
func StripANSI(s string) string {
	if !strings.ContainsRune(s, '\x1b') {
		return s
	}
	return ansiEscapeRegex.ReplaceAllString(s, "")
}

// DetectCacheMissAnomaly compares two consecutive turns from the same session and flags
// an unexpected prompt cache invalidation (e.g. a sharp drop in cache hit ratio accompanied
// by a large cache creation write).
func DetectCacheMissAnomaly(prev, curr Usage) *CacheMissAnomaly {
	prevTotal := prev.TotalInputTokens()
	currTotal := curr.TotalInputTokens()
	if prevTotal < 2000 || currTotal < 2000 {
		return nil
	}

	prevHit := prev.CacheHitRatio()
	currHit := curr.CacheHitRatio()
	drop := prevHit - currHit

	// Flag when previous turn had a warm cache (>= 50% hit ratio), current hit ratio
	// dropped by at least 40 percentage points, and Anthropic had to re-create >= 1000 tokens.
	if prevHit >= 0.50 && drop >= 0.40 && curr.CacheCreationInputTokens >= 1000 {
		return &CacheMissAnomaly{
			PreviousHitRatio:   prevHit,
			CurrentHitRatio:    currHit,
			HitRatioDrop:       drop,
			CacheCreationSpike: curr.CacheCreationInputTokens,
			Reason:             "sudden prompt cache invalidation mid-session",
		}
	}
	return nil
}

type readCallKey struct {
	filePath string
	offset   int
	limit    int
}

type readCallInput struct {
	FilePath string `json:"file_path"`
	Path     string `json:"path"`
	Offset   int    `json:"offset"`
	Limit    int    `json:"limit"`
}

type readTracker struct {
	key        readCallKey
	rawPath    string
	toolUseIDs []string
	wasted     int
}

func detectDuplicateReads(messages []Message, toolResultBytes map[string]int, res *InspectionResult) {
	// Active trackers represent reads since the last mutation of each file.
	// Once a file is modified (Edit/Write/MultiEdit), active entries for that file are finalized and reset.
	active := make(map[readCallKey]*readTracker)
	var completed []*readTracker

	finalizeFile := func(normPath string) {
		for k, tr := range active {
			if k.filePath == normPath {
				if len(tr.toolUseIDs) > 1 {
					completed = append(completed, tr)
				}
				delete(active, k)
			}
		}
	}

	for _, msg := range messages {
		if msg.Role != "assistant" {
			continue
		}
		for _, block := range msg.Content.Blocks {
			if block.Type != "tool_use" {
				continue
			}

			nameLower := strings.ToLower(block.Name)
			switch nameLower {
			case "read", "read_file":
				var in readCallInput
				if err := json.Unmarshal(block.Input, &in); err != nil {
					continue
				}
				rawPath := in.FilePath
				if rawPath == "" {
					rawPath = in.Path
				}
				normPath := normalizeToolPath(rawPath)
				if normPath == "" {
					continue
				}
				key := readCallKey{
					filePath: normPath,
					offset:   in.Offset,
					limit:    in.Limit,
				}
				tr, exists := active[key]
				if !exists {
					active[key] = &readTracker{
						key:        key,
						rawPath:    rawPath,
						toolUseIDs: []string{block.ID},
					}
					continue
				}
				tr.toolUseIDs = append(tr.toolUseIDs, block.ID)
				if block.ID != "" {
					tr.wasted += toolResultBytes[block.ID]
				}

			case "edit", "write", "multiedit", "notebookedit", "write_file", "edit_file":
				var in readCallInput
				if err := json.Unmarshal(block.Input, &in); err != nil {
					continue
				}
				rawPath := in.FilePath
				if rawPath == "" {
					rawPath = in.Path
				}
				if normPath := normalizeToolPath(rawPath); normPath != "" {
					finalizeFile(normPath)
				}
			}
		}
	}

	for _, tr := range active {
		if len(tr.toolUseIDs) > 1 {
			completed = append(completed, tr)
		}
	}

	for _, tr := range completed {
		redundant := len(tr.toolUseIDs) - 1
		estTokens := estimateTokens(tr.wasted)
		res.DuplicateReads = append(res.DuplicateReads, DuplicateRead{
			FilePath:        tr.rawPath,
			Offset:          tr.key.offset,
			Limit:           tr.key.limit,
			Count:           len(tr.toolUseIDs),
			RedundantCount:  redundant,
			ToolUseIDs:      tr.toolUseIDs,
			WastedBytes:     tr.wasted,
			WastedEstTokens: estTokens,
		})
		res.DuplicateReadRedundantCalls += redundant
		res.DuplicateReadWastedBytes += tr.wasted
		res.DuplicateReadWastedTokens += estTokens
	}
}

func inspectANSIInBlock(block ContentBlock, out *ANSIDetection) {
	text := block.Content.CombinedText()
	if text == "" {
		return
	}
	count, byteLen := DetectANSI(text)
	if count == 0 {
		return
	}
	out.SequenceCount += count
	out.TotalBytes += byteLen
	out.AffectedResults++
	if block.ToolUseID != "" {
		out.ToolUseIDs = append(out.ToolUseIDs, block.ToolUseID)
	}
}

func normalizeToolPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	// Normalize Windows backslashes so identical paths compare consistently across platforms.
	p = strings.ReplaceAll(p, "\\", "/")
	return path.Clean(p)
}

func estimateTokens(byteLen int) int {
	if byteLen <= 0 {
		return 0
	}
	return byteLen / 4
}

func estimateTokensMin1(byteLen int) int {
	if byteLen <= 0 {
		return 0
	}
	t := (byteLen + 3) / 4
	if t < 1 {
		return 1
	}
	return t
}

const freezeWholeMsg = int(^uint(0) >> 1)

// findFrozenMessageBoundary mirrors internal/trimmer's frozen prefix calculation
// (inline cache_control markers + top-level automatic cache_control).
func findFrozenMessageBoundary(req *MessageRequest) (int, int) {
	if req == nil || len(req.Messages) == 0 {
		return -1, -1
	}
	lastMsg := -1
	lastBlock := -1
	for i, msg := range req.Messages {
		for j, b := range msg.Content.Blocks {
			if b.CacheControl != nil && b.CacheControl.Type != "" {
				lastMsg = i
				lastBlock = j
			}
		}
	}

	if req.CacheControl != nil && req.CacheControl.Type != "" {
		autoMsg := len(req.Messages) - 1
		if strings.EqualFold(strings.TrimSpace(req.Messages[len(req.Messages)-1].Role), "user") {
			autoMsg = len(req.Messages) - 2
		}
		if autoMsg > lastMsg {
			lastMsg = autoMsg
			lastBlock = freezeWholeMsg
		} else if autoMsg == lastMsg && autoMsg >= 0 {
			lastBlock = freezeWholeMsg
		}
	}
	return lastMsg, lastBlock
}

func isAfterFrozenBoundary(msgIdx, blockIdx, frozenMsgIdx, frozenBlockIdx int) bool {
	if frozenMsgIdx < 0 {
		return true
	}
	if msgIdx > frozenMsgIdx {
		return true
	}
	if msgIdx == frozenMsgIdx && blockIdx > frozenBlockIdx {
		return true
	}
	return false
}
