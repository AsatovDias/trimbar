package anthropic

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSSEParserUsageAndCacheMetrics(t *testing.T) {
	stream := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_01ABC","type":"message","role":"assistant","model":"claude-sonnet-4-20250514","content":[],"stop_reason":null,"usage":{"input_tokens":42,"cache_creation_input_tokens":1500,"cache_read_input_tokens":18400,"cache_creation":{"ephemeral_5m_input_tokens":1000,"ephemeral_1h_input_tokens":500},"output_tokens":1}}}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		"",
		"event: ping",
		`data: {"type":"ping"}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":0}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":128}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n")

	// Feed in fragmented 7-byte chunks to simulate arbitrary TCP segmentation.
	parser := NewSSEParser()
	raw := []byte(stream)
	for i := 0; i < len(raw); i += 7 {
		end := min(i+7, len(raw))
		if n, err := parser.Write(raw[i:end]); err != nil || n != end-i {
			t.Fatalf("Write() = (%d, %v), want (%d, nil)", n, err, end-i)
		}
	}
	res := parser.Finish()

	if res.MessageID != "msg_01ABC" {
		t.Errorf("MessageID = %q, want msg_01ABC", res.MessageID)
	}
	if res.Model != "claude-sonnet-4-20250514" {
		t.Errorf("Model = %q, want claude-sonnet-4-20250514", res.Model)
	}
	if res.StopReason != "end_turn" {
		t.Errorf("StopReason = %q, want end_turn", res.StopReason)
	}
	if !res.Completed {
		t.Errorf("Completed = false, want true")
	}
	if res.FirstTokenAt.IsZero() {
		t.Errorf("FirstTokenAt is zero, expected timestamp on content_block_delta")
	}

	// Verify message_delta did NOT clobber message_start input/cache tokens.
	if res.Usage.InputTokens != 42 {
		t.Errorf("InputTokens = %d, want 42", res.Usage.InputTokens)
	}
	if res.Usage.CacheCreationInputTokens != 1500 {
		t.Errorf("CacheCreationInputTokens = %d, want 1500", res.Usage.CacheCreationInputTokens)
	}
	if res.Usage.CacheReadInputTokens != 18400 {
		t.Errorf("CacheReadInputTokens = %d, want 18400", res.Usage.CacheReadInputTokens)
	}
	if res.Usage.OutputTokens != 128 {
		t.Errorf("OutputTokens = %d, want 128", res.Usage.OutputTokens)
	}
	if res.Usage.CacheCreation == nil ||
		res.Usage.CacheCreation.Ephemeral5mInputTokens != 1000 ||
		res.Usage.CacheCreation.Ephemeral1hInputTokens != 500 {
		t.Errorf("CacheCreation = %+v, want {5m:1000, 1h:500}", res.Usage.CacheCreation)
	}
}

func TestSSEParserCRLFAndMidStreamError(t *testing.T) {
	stream := "event: message_start\r\n" +
		`data: {"type":"message_start","message":{"id":"msg_err","model":"claude-opus-4-20250514","usage":{"input_tokens":10,"cache_read_input_tokens":5000}}}` + "\r\n\r\n" +
		"event: error\r\n" +
		`data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}` + "\r\n\r\n"

	parser := NewSSEParser()
	// 1-byte writes stress-test CRLF boundary handling.
	for i := 0; i < len(stream); i++ {
		_, _ = parser.Write([]byte{stream[i]})
	}
	res := parser.Finish()

	if res.ErrorType != "overloaded_error" || res.ErrorMessage != "Overloaded" {
		t.Errorf("Error = (%q, %q), want (overloaded_error, Overloaded)", res.ErrorType, res.ErrorMessage)
	}
	if res.Usage.CacheReadInputTokens != 5000 {
		t.Errorf("CacheReadInputTokens = %d, want 5000", res.Usage.CacheReadInputTokens)
	}
	if res.Completed {
		t.Errorf("Completed = true, want false when stream terminates with error")
	}
}

func TestInspectRequestCacheControlAndAgentRole(t *testing.T) {
	mainPayload := `{
		"model": "claude-sonnet-4-20250514",
		"max_tokens": 8192,
		"stream": true,
		"system": [
			{"type": "text", "text": "You are Claude Code.", "cache_control": {"type": "ephemeral"}}
		],
		"tools": [
			{"name": "Read", "description": "Read a file", "input_schema": {"type": "object"}, "cache_control": {"type": "ephemeral"}}
		],
		"messages": [
			{
				"role": "user",
				"content": [
					{"type": "text", "text": "Fix the bug in main.go", "cache_control": {"type": "ephemeral"}}
				]
			}
		]
	}`

	res, err := InspectRequest([]byte(mainPayload))
	if err != nil {
		t.Fatalf("InspectRequest() error = %v", err)
	}
	if res.AgentRole != AgentRoleMain {
		t.Errorf("AgentRole = %q, want %q", res.AgentRole, AgentRoleMain)
	}
	if !res.HasPromptCaching || res.CacheBreakpoints != 3 {
		t.Errorf("CacheBreakpoints = %d (HasPromptCaching=%v), want 3", res.CacheBreakpoints, res.HasPromptCaching)
	}
	if len(res.CacheControlLocations) != 3 {
		t.Errorf("CacheControlLocations = %v, want [system tools messages]", res.CacheControlLocations)
	}

	// Background Haiku sub-agent payload (string system + string message content)
	subPayload := `{
		"model": "claude-haiku-4-5-20251001",
		"max_tokens": 512,
		"stream": false,
		"system": "Summarize the following bash output concisely.",
		"messages": [
			{"role": "user", "content": "ls -la output..."}
		]
	}`
	subRes, err := InspectRequest([]byte(subPayload))
	if err != nil {
		t.Fatalf("InspectRequest(subagent) error = %v", err)
	}
	if subRes.AgentRole != AgentRoleSubagent {
		t.Errorf("subagent AgentRole = %q, want %q", subRes.AgentRole, AgentRoleSubagent)
	}
	if subRes.HasPromptCaching {
		t.Errorf("expected HasPromptCaching=false for uncached subagent request")
	}
}

func TestDuplicateReadDetector(t *testing.T) {
	fileBody := strings.Repeat("package main // line of code\n", 40) // 1160 bytes

	payload := map[string]any{
		"model":      "claude-sonnet-4-20250514",
		"max_tokens": 4096,
		"messages": []map[string]any{
			{"role": "user", "content": "Inspect main.go and config.go"},
			// 1st Read of main.go (legitimate) + Read of main.go offset 100 (different slice)
			{
				"role": "assistant",
				"content": []map[string]any{
					{"type": "tool_use", "id": "tu_r1", "name": "Read", "input": map[string]any{"file_path": "/repo/main.go"}},
					{"type": "tool_use", "id": "tu_r_slice", "name": "Read", "input": map[string]any{"file_path": "/repo/main.go", "offset": 100, "limit": 50}},
					{"type": "tool_use", "id": "tu_cfg1", "name": "Read", "input": map[string]any{"file_path": "/repo/config.go"}},
				},
			},
			{
				"role": "user",
				"content": []map[string]any{
					{"type": "tool_result", "tool_use_id": "tu_r1", "content": fileBody},
					{"type": "tool_result", "tool_use_id": "tu_r_slice", "content": "slice content"},
					{"type": "tool_result", "tool_use_id": "tu_cfg1", "content": "cfg v1"},
				},
			},
			// 2nd & 3rd redundant Read of /repo/main.go (NO intervening Edit!)
			// Plus an Edit of /repo/config.go followed by a legitimate re-Read of /repo/config.go.
			{
				"role": "assistant",
				"content": []map[string]any{
					{"type": "tool_use", "id": "tu_r2", "name": "Read", "input": map[string]any{"file_path": "/repo/main.go"}},
					{"type": "tool_use", "id": "tu_edit_cfg", "name": "Edit", "input": map[string]any{"file_path": "/repo/config.go"}},
				},
			},
			{
				"role": "user",
				"content": []map[string]any{
					{"type": "tool_result", "tool_use_id": "tu_r2", "content": fileBody},
					{"type": "tool_result", "tool_use_id": "tu_edit_cfg", "content": "ok"},
				},
			},
			{
				"role": "assistant",
				"content": []map[string]any{
					{"type": "tool_use", "id": "tu_r3", "name": "Read", "input": map[string]any{"file_path": "/repo/main.go"}},
					{"type": "tool_use", "id": "tu_cfg2", "name": "Read", "input": map[string]any{"file_path": "/repo/config.go"}},
				},
			},
			{
				"role": "user",
				"content": []map[string]any{
					{"type": "tool_result", "tool_use_id": "tu_r3", "content": fileBody},
					{"type": "tool_result", "tool_use_id": "tu_cfg2", "content": "cfg v2"},
				},
			},
		},
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	res, err := InspectRequest(raw)
	if err != nil {
		t.Fatalf("InspectRequest() error = %v", err)
	}

	// Only /repo/main.go (full read) should be flagged; /repo/config.go was edited in between
	// and /repo/main.go offset=100,limit=50 is a distinct slice.
	if len(res.DuplicateReads) != 1 {
		t.Fatalf("len(DuplicateReads) = %d, want 1 (%+v)", len(res.DuplicateReads), res.DuplicateReads)
	}

	dup := res.DuplicateReads[0]
	if dup.FilePath != "/repo/main.go" {
		t.Errorf("dup.FilePath = %q, want /repo/main.go", dup.FilePath)
	}
	if dup.Count != 3 || dup.RedundantCount != 2 {
		t.Errorf("dup Count=%d RedundantCount=%d, want 3 and 2", dup.Count, dup.RedundantCount)
	}
	if len(dup.ToolUseIDs) != 3 || dup.ToolUseIDs[0] != "tu_r1" || dup.ToolUseIDs[1] != "tu_r2" || dup.ToolUseIDs[2] != "tu_r3" {
		t.Errorf("dup.ToolUseIDs = %v, want [tu_r1 tu_r2 tu_r3]", dup.ToolUseIDs)
	}
	wantWastedBytes := len(fileBody) * 2
	if dup.WastedBytes != wantWastedBytes {
		t.Errorf("dup.WastedBytes = %d, want %d", dup.WastedBytes, wantWastedBytes)
	}
	if res.DuplicateReadRedundantCalls != 2 || res.DuplicateReadWastedBytes != wantWastedBytes {
		t.Errorf("aggregate duplicate stats = (%d, %d), want (2, %d)",
			res.DuplicateReadRedundantCalls, res.DuplicateReadWastedBytes, wantWastedBytes)
	}
}

func TestANSIDetectorAndStrip(t *testing.T) {
	coloredOutput := "\x1b[1;32mPASS\x1b[0m internal/proxy\n\x1b[31mFAIL\x1b[0m internal/foo (\x1b[38;5;208m0.12s\x1b[0m)\n\x1b]8;;https://example.com\x07link\x1b]8;;\x07"
	cleanOutput := "PASS internal/proxy\nFAIL internal/foo (0.12s)\nlink"

	if stripped := StripANSI(coloredOutput); stripped != cleanOutput {
		t.Errorf("StripANSI() = %q, want %q", stripped, cleanOutput)
	}

	payload := map[string]any{
		"model":      "claude-sonnet-4-20250514",
		"max_tokens": 4096,
		"messages": []map[string]any{
			{
				"role": "user",
				"content": []map[string]any{
					{"type": "tool_result", "tool_use_id": "tu_bash_1", "content": coloredOutput},
					{"type": "tool_result", "tool_use_id": "tu_bash_2", "content": "plain clean output without escape codes"},
				},
			},
		},
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	res, err := InspectRequest(raw)
	if err != nil {
		t.Fatalf("InspectRequest() error = %v", err)
	}

	// 6 CSI sequences + 2 OSC sequences = 8 sequences total
	if res.ANSI.SequenceCount != 8 {
		t.Errorf("ANSI.SequenceCount = %d, want 8", res.ANSI.SequenceCount)
	}
	wantANSIBytes := len(coloredOutput) - len(cleanOutput)
	if res.ANSI.TotalBytes != wantANSIBytes {
		t.Errorf("ANSI.TotalBytes = %d, want %d", res.ANSI.TotalBytes, wantANSIBytes)
	}
	if res.ANSI.AffectedResults != 1 {
		t.Errorf("ANSI.AffectedResults = %d, want 1", res.ANSI.AffectedResults)
	}
	if len(res.ANSI.ToolUseIDs) != 1 || res.ANSI.ToolUseIDs[0] != "tu_bash_1" {
		t.Errorf("ANSI.ToolUseIDs = %v, want [tu_bash_1]", res.ANSI.ToolUseIDs)
	}
	if res.ANSI.EstTokens <= 0 {
		t.Errorf("ANSI.EstTokens = %d, want > 0", res.ANSI.EstTokens)
	}
}

func TestDetectCacheMissAnomaly(t *testing.T) {
	warmPrev := Usage{
		InputTokens:              100,
		CacheCreationInputTokens: 200,
		CacheReadInputTokens:     25_000,
	}
	// Normal continuation turn -> no anomaly
	warmCurr := Usage{
		InputTokens:              120,
		CacheCreationInputTokens: 300,
		CacheReadInputTokens:     25_300,
	}
	if anomaly := DetectCacheMissAnomaly(warmPrev, warmCurr); anomaly != nil {
		t.Errorf("expected no anomaly for warm hit, got %+v", anomaly)
	}

	// Sudden mid-session invalidation -> anomaly detected
	coldMiss := Usage{
		InputTokens:              120,
		CacheCreationInputTokens: 25_000,
		CacheReadInputTokens:     500,
	}
	anomaly := DetectCacheMissAnomaly(warmPrev, coldMiss)
	if anomaly == nil {
		t.Fatalf("expected CacheMissAnomaly to be detected")
	}
	if anomaly.CacheCreationSpike != 25_000 {
		t.Errorf("CacheCreationSpike = %d, want 25000", anomaly.CacheCreationSpike)
	}
	if anomaly.HitRatioDrop < 0.80 {
		t.Errorf("HitRatioDrop = %.2f, want >= 0.80", anomaly.HitRatioDrop)
	}
}
