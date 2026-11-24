package trimmer

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestFrozenPrefixUntouched verifies that ANY content at or before the last cache_control
// breakpoint (including duplicate Read results and ANSI escape codes) is preserved
// byte-for-byte without modification.
func TestFrozenPrefixUntouched(t *testing.T) {
	fileContent := strings.Repeat("package main // cached content\n", 40)
	ansiOutput := "\x1b[1;32mPASS\x1b[0m \x1b[31mFAIL\x1b[0m internal/proxy"

	// Put duplicate Read AND ANSI output strictly BEFORE/ON the last cache_control breakpoint.
	payload := map[string]any{
		"model":      "claude-sonnet-4-20250514",
		"max_tokens": 8192,
		"system": []map[string]any{
			{"type": "text", "text": "You are Claude Code.", "cache_control": map[string]string{"type": "ephemeral"}},
		},
		"messages": []map[string]any{
			{"role": "user", "content": "Inspect main.go"},
			{
				"role": "assistant",
				"content": []map[string]any{
					{"type": "tool_use", "id": "tu_r1", "name": "Read", "input": map[string]string{"file_path": "/repo/main.go"}},
				},
			},
			{
				"role": "user",
				"content": []map[string]any{
					{"type": "tool_result", "tool_use_id": "tu_r1", "content": fileContent},
				},
			},
			{
				"role": "assistant",
				"content": []map[string]any{
					{"type": "tool_use", "id": "tu_r2", "name": "Read", "input": map[string]string{"file_path": "/repo/main.go"}},
					{"type": "tool_use", "id": "tu_b1", "name": "Bash", "input": map[string]string{"command": "go test ./..."}},
				},
			},
			{
				"role": "user",
				"content": []map[string]any{
					// Duplicate Read BEFORE the cache_control block
					{"type": "tool_result", "tool_use_id": "tu_r2", "content": fileContent},
					// ANSI output ON the cache_control block itself (inclusive frozen boundary!)
					{"type": "tool_result", "tool_use_id": "tu_b1", "content": ansiOutput, "cache_control": map[string]string{"type": "ephemeral"}},
				},
			},
		},
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	res, err := TrimRequest(raw)
	if err != nil {
		t.Fatalf("TrimRequest() error = %v", err)
	}
	if res.Modified {
		t.Errorf("expected Modified=false when all waste is inside frozen prefix, got %+v", res)
	}
	if res.TrimmedBytes != 0 || res.TrimmedTokens != 0 {
		t.Errorf("expected 0 trimmed bytes/tokens, got (%d, %d)", res.TrimmedBytes, res.TrimmedTokens)
	}
	if res.FrozenMessageIdx != 4 || res.FrozenBlockIdx != 1 {
		t.Errorf("Frozen boundary = (%d, %d), want (4, 1)", res.FrozenMessageIdx, res.FrozenBlockIdx)
	}
	if !bytes.Equal(res.Body, raw) {
		t.Errorf("expected byte-for-byte identical body when nothing after cache_control changed")
	}
}

// TestTrimAfterCacheControl verifies that duplicate Reads and ANSI codes AFTER the last
// cache_control breakpoint are cleanly trimmed while frozen messages remain byte-for-byte identical.
func TestTrimAfterCacheControl(t *testing.T) {
	fileContent := strings.Repeat("func handler() { /* important code */ }\n", 40) // 1600 bytes
	frozenANSI := "\x1b[31mFROZEN_ANSI\x1b[0m"
	postCacheANSI := "\x1b[1;32mPASS\x1b[0m test suite (\x1b[33m0.12s\x1b[0m)\n\x1b]8;;https://example.com\x07docs\x1b]8;;\x07"
	wantCleanANSI := "PASS test suite (0.12s)\ndocs"

	payload := map[string]any{
		"model":      "claude-sonnet-4-20250514",
		"max_tokens": 8192,
		"thinking":   map[string]any{"type": "enabled", "budget_tokens": 2048},
		"messages": []map[string]any{
			{"role": "user", "content": "Inspect handler.go"},
			{
				"role": "assistant",
				"content": []map[string]any{
					{"type": "tool_use", "id": "tu_r1", "name": "Read", "input": map[string]string{"file_path": "internal/proxy/handler.go"}},
					{"type": "tool_use", "id": "tu_b_frozen", "name": "Bash", "input": map[string]string{"command": "echo hi"}},
				},
			},
			// Frozen boundary ends here (msg 2, block 1 has cache_control)
			{
				"role": "user",
				"content": []map[string]any{
					{"type": "tool_result", "tool_use_id": "tu_r1", "content": fileContent},
					{"type": "tool_result", "tool_use_id": "tu_b_frozen", "content": frozenANSI, "cache_control": map[string]string{"type": "ephemeral"}},
				},
			},
			// Post-cache turn: duplicate Read (full file), sliced Read (offset/limit -> must NOT be replaced), and ANSI Bash output
			{
				"role": "assistant",
				"content": []map[string]any{
					{"type": "tool_use", "id": "tu_r2_dup", "name": "Read", "input": map[string]string{"file_path": "internal/proxy/handler.go"}},
					{"type": "tool_use", "id": "tu_r_slice", "name": "Read", "input": map[string]any{"file_path": "internal/proxy/handler.go", "offset": 10, "limit": 20}},
					{"type": "tool_use", "id": "tu_b_new", "name": "Bash", "input": map[string]string{"command": "go test ./..."}},
				},
			},
			{
				"role": "user",
				"content": []map[string]any{
					{"type": "tool_result", "tool_use_id": "tu_r2_dup", "content": fileContent},
					{"type": "tool_result", "tool_use_id": "tu_r_slice", "content": fileContent},
					{"type": "tool_result", "tool_use_id": "tu_b_new", "content": postCacheANSI},
				},
			},
		},
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Capture original frozen message bytes to verify exact byte-for-byte preservation.
	var origTop map[string]json.RawMessage
	_ = json.Unmarshal(raw, &origTop)
	var origMsgs []json.RawMessage
	_ = json.Unmarshal(origTop["messages"], &origMsgs)

	res, err := TrimRequest(raw)
	if err != nil {
		t.Fatalf("TrimRequest() error = %v", err)
	}
	if !res.Modified {
		t.Fatalf("expected Modified=true")
	}
	if res.DupReadsTrimmed != 1 {
		t.Errorf("DupReadsTrimmed = %d, want 1", res.DupReadsTrimmed)
	}
	if res.ANSIStrippedCount != 6 {
		t.Errorf("ANSIStrippedCount = %d, want 6", res.ANSIStrippedCount)
	}
	if res.TrimmedTokens <= 0 {
		t.Errorf("TrimmedTokens = %d, want > 0", res.TrimmedTokens)
	}

	// Verify frozen messages 0, 1, 2 AND unknown top-level field "thinking" are byte-for-byte identical.
	var newTop map[string]json.RawMessage
	if err := json.Unmarshal(res.Body, &newTop); err != nil {
		t.Fatalf("unmarshal trimmed body: %v", err)
	}
	if !bytes.Equal(origTop["thinking"], newTop["thinking"]) {
		t.Errorf("top-level unknown field 'thinking' was altered")
	}
	var newMsgs []json.RawMessage
	if err := json.Unmarshal(newTop["messages"], &newMsgs); err != nil {
		t.Fatalf("unmarshal trimmed messages: %v", err)
	}
	for i := 0; i <= 2; i++ {
		if !bytes.Equal(origMsgs[i], newMsgs[i]) {
			t.Errorf("frozen message[%d] was modified!\norig=%s\nnew =%s", i, origMsgs[i], newMsgs[i])
		}
	}

	// Inspect post-cache message[4] blocks
	var msg4 struct {
		Content []struct {
			ToolUseID string `json:"tool_use_id"`
			Content   string `json:"content"`
		} `json:"content"`
	}
	if err := json.Unmarshal(newMsgs[4], &msg4); err != nil {
		t.Fatalf("unmarshal msg[4]: %v", err)
	}

	wantPlaceholder := "[Trimbar: identical content of internal/proxy/handler.go already loaded earlier in context]"
	if msg4.Content[0].Content != wantPlaceholder {
		t.Errorf("duplicate Read content = %q, want %q", msg4.Content[0].Content, wantPlaceholder)
	}
	if msg4.Content[1].Content != fileContent {
		t.Errorf("offset/limit Read should NOT have been deduplicated")
	}
	if msg4.Content[2].Content != wantCleanANSI {
		t.Errorf("ANSI stripped content = %q, want %q", msg4.Content[2].Content, wantCleanANSI)
	}
}

// TestEditOrWriteResetsDeduplication verifies that modifying a file via Edit or Write
// invalidates prior Read history so a subsequent Read is NOT replaced.
func TestEditOrWriteResetsDeduplication(t *testing.T) {
	v1 := strings.Repeat("version 1 of file content\n", 30)
	v2 := strings.Repeat("version 2 after edit/write\n", 30)

	for _, mutatorTool := range []string{"Edit", "Write"} {
		t.Run("reset_after_"+mutatorTool, func(t *testing.T) {
			payload := map[string]any{
				"model":      "claude-sonnet-4-20250514",
				"max_tokens": 4096,
				"messages": []map[string]any{
					{
						"role": "user",
						"content": []map[string]any{
							{"type": "text", "text": "Update config.go", "cache_control": map[string]string{"type": "ephemeral"}},
						},
					},
					{
						"role": "assistant",
						"content": []map[string]any{
							{"type": "tool_use", "id": "tu_r1", "name": "Read", "input": map[string]string{"file_path": "internal/config/config.go"}},
						},
					},
					{
						"role": "user",
						"content": []map[string]any{
							{"type": "tool_result", "tool_use_id": "tu_r1", "content": v1},
						},
					},
					{
						"role": "assistant",
						"content": []map[string]any{
							{"type": "tool_use", "id": "tu_mut", "name": mutatorTool, "input": map[string]string{"file_path": "internal/config/config.go"}},
						},
					},
					{
						"role": "user",
						"content": []map[string]any{
							{"type": "tool_result", "tool_use_id": "tu_mut", "content": "File updated successfully"},
						},
					},
					{
						"role": "assistant",
						"content": []map[string]any{
							{"type": "tool_use", "id": "tu_r2", "name": "Read", "input": map[string]string{"file_path": "internal/config/config.go"}},
						},
					},
					{
						"role": "user",
						"content": []map[string]any{
							{"type": "tool_result", "tool_use_id": "tu_r2", "content": v2},
						},
					},
				},
			}

			raw, err := json.Marshal(payload)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}

			res, err := TrimRequest(raw)
			if err != nil {
				t.Fatalf("TrimRequest() error = %v", err)
			}
			if res.Modified || res.DupReadsTrimmed != 0 {
				t.Errorf("Read after %s must NOT be deduplicated; got Modified=%v DupReadsTrimmed=%d",
					mutatorTool, res.Modified, res.DupReadsTrimmed)
			}
		})
	}
}

// TestTrimNestedBlockArrayToolResult verifies trimming works when tool_result.content
// uses the array-of-blocks form [{"type":"text","text":"..."}] as well as string form.
func TestTrimNestedBlockArrayToolResult(t *testing.T) {
	fileContent := strings.Repeat("line of source code\n", 40)

	payload := map[string]any{
		"model":      "claude-sonnet-4-20250514",
		"max_tokens": 4096,
		"system": []map[string]any{
			{"type": "text", "text": "System prompt", "cache_control": map[string]string{"type": "ephemeral"}},
		},
		"messages": []map[string]any{
			{
				"role": "assistant",
				"content": []map[string]any{
					{"type": "tool_use", "id": "tu_r1", "name": "Read", "input": map[string]string{"file_path": "main.go"}},
				},
			},
			{
				"role": "user",
				"content": []map[string]any{
					{"type": "tool_result", "tool_use_id": "tu_r1", "content": []map[string]string{{"type": "text", "text": fileContent}}},
				},
			},
			{
				"role": "assistant",
				"content": []map[string]any{
					{"type": "tool_use", "id": "tu_r2", "name": "Read", "input": map[string]string{"file_path": "main.go"}},
					{"type": "tool_use", "id": "tu_b1", "name": "Bash", "input": map[string]string{"command": "ls"}},
				},
			},
			{
				"role": "user",
				"content": []map[string]any{
					{"type": "tool_result", "tool_use_id": "tu_r2", "content": []map[string]string{{"type": "text", "text": fileContent}}},
					{"type": "tool_result", "tool_use_id": "tu_b1", "content": []map[string]string{{"type": "text", "text": "\x1b[32mok\x1b[0m"}}},
				},
			},
		},
	}

	raw, _ := json.Marshal(payload)
	res, err := TrimRequest(raw)
	if err != nil {
		t.Fatalf("TrimRequest() error = %v", err)
	}
	if !res.Modified || res.DupReadsTrimmed != 1 || res.ANSIStrippedCount != 2 {
		t.Errorf("unexpected result: Modified=%v DupReadsTrimmed=%d ANSIStrippedCount=%d",
			res.Modified, res.DupReadsTrimmed, res.ANSIStrippedCount)
	}
}

// TestDuplicateReadSafetyGuards guarantees that deduplication NEVER fires when:
//  1) The original tool_result is shorter than 200 characters
//  2) The original tool_result was compacted/summarized by Claude Code (/compact)
//  3) The original tool_result is missing from the current messages array
//  4) The file contents changed externally (different content on second Read)
func TestDuplicateReadSafetyGuards(t *testing.T) {
	fullBody := strings.Repeat("package main // full genuine source code line\n", 20) // > 900 chars
	shortBody := "package main\nfunc main() { println(\"short < 200 chars\") }\n"
	compactedStub := "[Compacted summary of previous conversation] " + strings.Repeat("Earlier we read main.go and discussed architecture. ", 8)
	changedBody := strings.Repeat("package main // modified externally via bash!\n", 20)

	cases := []struct {
		name           string
		messages       []map[string]any
		wantDupTrimmed int
	}{
		{
			name: "short original tool_result (<200 chars) is NOT deduplicated",
			messages: []map[string]any{
				{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "id": "r1", "name": "Read", "input": map[string]string{"file_path": "short.go"}}}},
				{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "r1", "content": shortBody, "cache_control": map[string]string{"type": "ephemeral"}}}},
				{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "id": "r2", "name": "Read", "input": map[string]string{"file_path": "short.go"}}}},
				{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "r2", "content": shortBody}}},
			},
			wantDupTrimmed: 0,
		},
		{
			name: "compacted /compact summary stub is NOT treated as full original Read",
			messages: []map[string]any{
				{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "id": "r1", "name": "Read", "input": map[string]string{"file_path": "main.go"}}}},
				{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "r1", "content": compactedStub, "cache_control": map[string]string{"type": "ephemeral"}}}},
				{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "id": "r2", "name": "Read", "input": map[string]string{"file_path": "main.go"}}}},
				{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "r2", "content": fullBody}}},
			},
			wantDupTrimmed: 0,
		},
		{
			name: "missing original tool_result in messages is NOT deduplicated",
			messages: []map[string]any{
				{"role": "user", "content": []map[string]any{{"type": "text", "text": "Summary: earlier we read main.go", "cache_control": map[string]string{"type": "ephemeral"}}}},
				{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "id": "r1", "name": "Read", "input": map[string]string{"file_path": "main.go"}}}},
				// Notice: no tool_result for r1!
				{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "id": "r2", "name": "Read", "input": map[string]string{"file_path": "main.go"}}}},
				{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "r2", "content": fullBody}}},
			},
			wantDupTrimmed: 0,
		},
		{
			name: "externally changed file content is NOT deduplicated",
			messages: []map[string]any{
				{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "id": "r1", "name": "Read", "input": map[string]string{"file_path": "main.go"}}}},
				{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "r1", "content": fullBody, "cache_control": map[string]string{"type": "ephemeral"}}}},
				{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "id": "r2", "name": "Read", "input": map[string]string{"file_path": "main.go"}}}},
				{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "r2", "content": changedBody}}},
			},
			wantDupTrimmed: 0,
		},
		{
			name: "full uncompacted identical original tool_result IS deduplicated",
			messages: []map[string]any{
				{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "id": "r1", "name": "Read", "input": map[string]string{"file_path": "main.go"}}}},
				{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "r1", "content": fullBody, "cache_control": map[string]string{"type": "ephemeral"}}}},
				{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "id": "r2", "name": "Read", "input": map[string]string{"file_path": "main.go"}}}},
				{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "r2", "content": fullBody}}},
			},
			wantDupTrimmed: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(map[string]any{
				"model":      "claude-sonnet-4-20250514",
				"max_tokens": 4096,
				"messages":   tc.messages,
			})
			res, err := TrimRequest(raw)
			if err != nil {
				t.Fatalf("TrimRequest() error = %v", err)
			}
			if res.DupReadsTrimmed != tc.wantDupTrimmed {
				t.Errorf("DupReadsTrimmed = %d, want %d", res.DupReadsTrimmed, tc.wantDupTrimmed)
			}
		})
	}
}
