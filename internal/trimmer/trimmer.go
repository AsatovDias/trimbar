package trimmer

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"github.com/AsatovDias/trimbar/internal/anthropic"
)

// MinOriginalReadChars is the minimum character length an earlier Read tool_result
// must physically have in the current messages array to qualify as a full uncompacted
// read eligible for downstream deduplication.
const MinOriginalReadChars = 200

// TrimResult describes the outcome of cache-safe active trimming on a /v1/messages request.
type TrimResult struct {
	Body              []byte   // Trimmed JSON payload (or original bytes untouched if Modified == false)
	Modified          bool     // True only when at least one post-cache_control block was changed
	FrozenMessageIdx   int      // Index of the last message containing cache_control (-1 if none in messages)
	FrozenBlockIdx     int      // Index of the last block containing cache_control within FrozenMessageIdx (-1 if none)
	PostCacheToolBytes int      // Total tool_result content bytes strictly after the last cache_control boundary
	TrimmedBytes       int      // Exact byte reduction achieved across modified blocks
	TrimmedTokens     int      // Estimated tokens saved (approx. TrimmedBytes / 4)
	ANSIStrippedCount int      // Number of ANSI escape sequences removed
	ANSIBytesSaved    int      // Bytes removed by ANSI stripping
	DupReadsTrimmed   int      // Number of duplicate Read tool_results replaced with compact placeholders
	DupReadBytesSaved int      // Bytes removed by duplicate Read deduplication
	DupReadFiles      []string // File paths deduplicated
}

type readMeta struct {
	normPath string
	rawPath  string
}

type seenReadEntry struct {
	contentHash [32]byte
	contentLen  int
}

type toolInputPath struct {
	FilePath string `json:"file_path"`
	Path     string `json:"path"`
	Offset   int    `json:"offset"`
	Limit    int    `json:"limit"`
}

// TrimRequest applies deterministic, cache-safe context trimming to a raw Anthropic
// POST /v1/messages JSON payload.
//
// Frozen Prefix Guarantee:
//   - "system", "tools", and all other top-level fields are NEVER modified.
//   - Every message and content block up to and including the LAST "cache_control"
//     marker in "messages" is preserved byte-for-byte verbatim.
//   - Only "tool_result" blocks strictly AFTER the last "cache_control" boundary
//     are eligible for:
//     1) Stripping ANSI escape sequences from terminal output.
//     2) Replacing redundant full-file Read results (offset==0, limit==0, not modified
//        by an intervening Edit/Write) ONLY when the full, uncompacted (>=200 chars),
//        identical original tool_result physically exists earlier in messages.
func TrimRequest(body []byte) (TrimResult, error) {
	res := TrimResult{
		Body:             body,
		FrozenMessageIdx: -1,
		FrozenBlockIdx:   -1,
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return res, nil
	}

	// Parse top-level JSON into raw fields so every untouched field (system, tools,
	// thinking, metadata, etc.) stays byte-for-byte intact.
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return res, err
	}

	rawMsgsField, hasMsgs := top["messages"]
	if !hasMsgs || len(bytes.TrimSpace(rawMsgsField)) == 0 || bytes.Equal(bytes.TrimSpace(rawMsgsField), []byte("null")) {
		return res, nil
	}

	var rawMessages []json.RawMessage
	if err := json.Unmarshal(rawMsgsField, &rawMessages); err != nil {
		return res, err
	}
	if len(rawMessages) == 0 {
		return res, nil
	}

	// Pass 1: Locate the exact frozen prefix boundary.
	// - Explicit inline cache_control markers inside messages freeze everything up to and
	//   including the last marked block.
	// - Top-level automatic caching (root "cache_control" field in /v1/messages) freezes
	//   every earlier turn before the final user message of the current turn.
	frozenMsgIdx, frozenBlockIdx := findFrozenBoundary(top, rawMessages)
	res.FrozenMessageIdx = frozenMsgIdx
	res.FrozenBlockIdx = frozenBlockIdx

	// Pass 2: Walk messages chronologically from index 0 to track verified full-file Reads
	// physically present in messages and Edit/Write invalidations, while ONLY mutating
	// tool_result blocks strictly after (frozenMsgIdx, frozenBlockIdx).
	seenReads := make(map[string]seenReadEntry)
	pendingReads := make(map[string]readMeta)
	dupFileSeen := make(map[string]bool)

	anyModified := false
	updatedMessages := make([]json.RawMessage, len(rawMessages))
	copy(updatedMessages, rawMessages)

	for msgIdx, rawMsg := range rawMessages {
		var msgObj map[string]json.RawMessage
		if err := json.Unmarshal(rawMsg, &msgObj); err != nil {
			continue
		}

		var role string
		if rawRole, ok := msgObj["role"]; ok {
			_ = json.Unmarshal(rawRole, &role)
		}

		rawContent, hasContent := msgObj["content"]
		if !hasContent {
			continue
		}
		trimmedContent := bytes.TrimSpace(rawContent)
		if len(trimmedContent) == 0 || trimmedContent[0] != '[' {
			// Plain string content cannot contain tool_use or tool_result blocks.
			continue
		}

		var rawBlocks []json.RawMessage
		if err := json.Unmarshal(trimmedContent, &rawBlocks); err != nil {
			continue
		}

		msgModified := false
		for blockIdx, rawBlock := range rawBlocks {
			var blockObj map[string]json.RawMessage
			if err := json.Unmarshal(rawBlock, &blockObj); err != nil {
				continue
			}

			var blockType string
			if rawType, ok := blockObj["type"]; ok {
				_ = json.Unmarshal(rawType, &blockType)
			}

			switch {
			case role == "assistant" && blockType == "tool_use":
				trackAssistantToolUse(blockObj, seenReads, pendingReads)

			case blockType == "tool_result":
				isAfterFrozen := isStrictlyAfterFrozen(msgIdx, blockIdx, frozenMsgIdx, frozenBlockIdx)

				var toolUseID string
				if rawID, ok := blockObj["tool_use_id"]; ok {
					_ = json.Unmarshal(rawID, &toolUseID)
				}
				var isErr bool
				if rawIsErr, ok := blockObj["is_error"]; ok {
					_ = json.Unmarshal(rawIsErr, &isErr)
				}

				rawResultContent, hasResultContent := blockObj["content"]

				// Determine if this tool_result corresponds to a full-file Read call.
				meta, isTrackedRead := pendingReads[toolUseID]
				isDuplicateRead := false
				if isTrackedRead {
					delete(pendingReads, toolUseID)
					if !isErr && hasResultContent {
						resultText, textOK := extractToolResultText(rawResultContent)
						if textOK && isFullUncompactedReadContent(resultText) {
							sum := sha256.Sum256([]byte(resultText))
							if prev, exists := seenReads[meta.normPath]; exists && prev.contentHash == sum {
								// Original full uncompacted Read is physically present in messages
								// and matches byte-for-byte -> safe to deduplicate.
								isDuplicateRead = true
							} else {
								// Record (or refresh if file changed externally) the live full content.
								seenReads[meta.normPath] = seenReadEntry{
									contentHash: sum,
									contentLen:  len(resultText),
								}
							}
						}
					}
				}

				// Frozen prefix protection: NEVER mutate anything at or before cache_control!
				if !isAfterFrozen || !hasResultContent {
					continue
				}

				if txt, ok := extractToolResultText(rawResultContent); ok && len(txt) > 0 {
					res.PostCacheToolBytes += len(txt)
				} else {
					res.PostCacheToolBytes += len(bytes.TrimSpace(rawResultContent))
				}

				var (
					newContentRaw json.RawMessage
					blockChanged  bool
				)

				// Priority 1: Duplicate Read replacement (replaces entire body with compact notice).
				if isDuplicateRead {
					placeholder := duplicateReadPlaceholder(meta.rawPath)
					replacedRaw, savedBytes, replaced := replaceToolResultContent(rawResultContent, placeholder)
					if replaced && savedBytes > 0 {
						newContentRaw = replacedRaw
						blockChanged = true
						res.DupReadsTrimmed++
						res.DupReadBytesSaved += savedBytes
						res.TrimmedBytes += savedBytes
						if !dupFileSeen[meta.rawPath] {
							dupFileSeen[meta.rawPath] = true
							res.DupReadFiles = append(res.DupReadFiles, meta.rawPath)
						}
					}
				}

				// Priority 2: ANSI escape sequence stripping (if not already replaced above).
				if !blockChanged {
					cleanedRaw, seqCount, savedBytes, cleaned := stripANSIFromToolResultContent(rawResultContent)
					if cleaned && savedBytes > 0 {
						newContentRaw = cleanedRaw
						blockChanged = true
						res.ANSIStrippedCount += seqCount
						res.ANSIBytesSaved += savedBytes
						res.TrimmedBytes += savedBytes
					}
				}

				if blockChanged {
					blockObj["content"] = newContentRaw
					reencodedBlock, err := json.Marshal(blockObj)
					if err == nil {
						rawBlocks[blockIdx] = reencodedBlock
						msgModified = true
					}
				}
			}
		}

		if msgModified {
			newContentArr, err := json.Marshal(rawBlocks)
			if err != nil {
				return res, err
			}
			msgObj["content"] = newContentArr
			newMsgRaw, err := json.Marshal(msgObj)
			if err != nil {
				return res, err
			}
			updatedMessages[msgIdx] = newMsgRaw
			anyModified = true
		}
	}

	if !anyModified {
		return res, nil
	}

	// Rebuild top-level JSON replacing ONLY the "messages" array, preserving all other
	// top-level fields and all frozen message elements verbatim.
	newMsgsRaw, err := json.Marshal(updatedMessages)
	if err != nil {
		return res, err
	}
	top["messages"] = newMsgsRaw

	finalBody, err := json.Marshal(top)
	if err != nil {
		return res, err
	}

	res.Body = finalBody
	res.Modified = true
	if res.TrimmedBytes > 0 {
		res.TrimmedTokens = (res.TrimmedBytes + 3) / 4
	}
	return res, nil
}

// isFullUncompactedReadContent verifies that a Read tool_result contains genuine,
// full file content (>= MinOriginalReadChars) rather than a compacted/truncated stub
// created by Claude Code's /compact or an earlier Trimbar placeholder.
func isFullUncompactedReadContent(text string) bool {
	trimmed := strings.TrimSpace(text)
	if len(trimmed) < MinOriginalReadChars {
		return false
	}

	lower := strings.ToLower(trimmed)
	compactedPrefixes := []string{
		"[trimbar:",
		"[compacted",
		"[summary",
		"[truncated",
		"[file content omitted",
		"[content omitted",
		"[previous tool result",
		"<summary>",
		"<compacted",
		"this session is being continued from a previous conversation",
	}
	for _, p := range compactedPrefixes {
		if strings.HasPrefix(lower, p) {
			return false
		}
	}

	compactedSubstrings := []string{
		"compacted by claude",
		"output truncated due to context",
		"content omitted after /compact",
		"summarized to save context",
	}
	for _, sub := range compactedSubstrings {
		if strings.Contains(lower, sub) {
			return false
		}
	}

	return true
}

func extractToolResultText(rawContent json.RawMessage) (string, bool) {
	trimmed := bytes.TrimSpace(rawContent)
	if len(trimmed) == 0 {
		return "", false
	}
	switch trimmed[0] {
	case '"':
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return "", false
		}
		return text, true
	case '[':
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(trimmed, &blocks); err != nil || len(blocks) == 0 {
			return "", false
		}
		var sb strings.Builder
		for i, b := range blocks {
			if b.Type != "text" {
				return "", false
			}
			if i > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(b.Text)
		}
		return sb.String(), true
	}
	return "", false
}

// duplicateReadPlaceholder returns the concise marker replacing redundant Read results.
func duplicateReadPlaceholder(filePath string) string {
	return fmt.Sprintf("[Trimbar: identical content of %s already loaded earlier in context]", filePath)
}

const freezeWholeMessage = int(^uint(0) >> 1)

func findFrozenBoundary(top map[string]json.RawMessage, rawMessages []json.RawMessage) (int, int) {
	lastMsg := -1
	lastBlock := -1

	for i, rawMsg := range rawMessages {
		var msg struct {
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(rawMsg, &msg); err != nil {
			continue
		}
		trimmed := bytes.TrimSpace(msg.Content)
		if len(trimmed) == 0 || trimmed[0] != '[' {
			continue
		}
		var blocks []struct {
			CacheControl *anthropic.CacheControl `json:"cache_control"`
		}
		if err := json.Unmarshal(trimmed, &blocks); err != nil {
			continue
		}
		for j, b := range blocks {
			if b.CacheControl != nil && b.CacheControl.Type != "" {
				lastMsg = i
				lastBlock = j
			}
		}
	}

	// Top-level Anthropic automatic caching (root "cache_control": {"type": "ephemeral"}):
	// freeze all earlier conversation turns before the final user message so prior
	// cached turns are never mutated even when no inline cache_control markers exist.
	if hasTopLevelCacheControl(top) && len(rawMessages) > 0 {
		autoMsg := len(rawMessages) - 1
		if messageRole(rawMessages[len(rawMessages)-1]) == "user" {
			autoMsg = len(rawMessages) - 2
		}
		if autoMsg > lastMsg {
			lastMsg = autoMsg
			lastBlock = freezeWholeMessage
		} else if autoMsg == lastMsg && autoMsg >= 0 {
			lastBlock = freezeWholeMessage
		}
	}

	return lastMsg, lastBlock
}

func hasTopLevelCacheControl(top map[string]json.RawMessage) bool {
	raw, ok := top["cache_control"]
	if !ok {
		return false
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return false
	}
	var cc anthropic.CacheControl
	if err := json.Unmarshal(trimmed, &cc); err != nil {
		return false
	}
	return cc.Type != ""
}

func messageRole(rawMsg json.RawMessage) string {
	var m struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal(rawMsg, &m); err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(m.Role))
}

func isStrictlyAfterFrozen(msgIdx, blockIdx, frozenMsgIdx, frozenBlockIdx int) bool {
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

func trackAssistantToolUse(blockObj map[string]json.RawMessage, seenReads map[string]seenReadEntry, pendingReads map[string]readMeta) {
	var name, id string
	if rawName, ok := blockObj["name"]; ok {
		_ = json.Unmarshal(rawName, &name)
	}
	if rawID, ok := blockObj["id"]; ok {
		_ = json.Unmarshal(rawID, &id)
	}
	rawInput, hasInput := blockObj["input"]
	if !hasInput {
		return
	}

	var in toolInputPath
	if err := json.Unmarshal(rawInput, &in); err != nil {
		return
	}
	rawPath := in.FilePath
	if rawPath == "" {
		rawPath = in.Path
	}
	normPath := normalizePath(rawPath)
	if normPath == "" {
		return
	}

	switch strings.ToLower(name) {
	case "read", "read_file":
		// Only full-file reads (without offset/limit) are eligible for deduplication.
		if in.Offset == 0 && in.Limit == 0 && id != "" {
			pendingReads[id] = readMeta{
				normPath: normPath,
				rawPath:  rawPath,
			}
		}
	case "edit", "write", "multiedit", "notebookedit", "edit_file", "write_file":
		// File was modified -> invalidate prior Read history so the next Read stays intact.
		delete(seenReads, normPath)
	}
}

func replaceToolResultContent(rawContent json.RawMessage, placeholder string) (json.RawMessage, int, bool) {
	trimmed := bytes.TrimSpace(rawContent)
	if len(trimmed) == 0 {
		return nil, 0, false
	}

	switch trimmed[0] {
	case '"':
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return nil, 0, false
		}
		if len(text) <= len(placeholder) {
			return nil, 0, false
		}
		saved := len(text) - len(placeholder)
		encoded, err := json.Marshal(placeholder)
		if err != nil {
			return nil, 0, false
		}
		return encoded, saved, true

	case '[':
		var blocks []map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &blocks); err != nil || len(blocks) == 0 {
			return nil, 0, false
		}
		totalTextLen := 0
		for _, b := range blocks {
			var bt string
			if rawT, ok := b["type"]; ok {
				_ = json.Unmarshal(rawT, &bt)
			}
			// Do not replace non-text payloads (e.g. images/documents).
			if bt != "text" {
				return nil, 0, false
			}
			var txt string
			if rawTxt, ok := b["text"]; ok {
				_ = json.Unmarshal(rawTxt, &txt)
			}
			totalTextLen += len(txt)
		}
		if totalTextLen <= len(placeholder) {
			return nil, 0, false
		}
		saved := totalTextLen - len(placeholder)
		replacement := []map[string]string{
			{"type": "text", "text": placeholder},
		}
		encoded, err := json.Marshal(replacement)
		if err != nil {
			return nil, 0, false
		}
		return encoded, saved, true
	}

	return nil, 0, false
}

func stripANSIFromToolResultContent(rawContent json.RawMessage) (json.RawMessage, int, int, bool) {
	trimmed := bytes.TrimSpace(rawContent)
	if len(trimmed) == 0 {
		return nil, 0, 0, false
	}

	switch trimmed[0] {
	case '"':
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return nil, 0, 0, false
		}
		seqCount, ansiBytes := anthropic.DetectANSI(text)
		if seqCount == 0 || ansiBytes == 0 {
			return nil, 0, 0, false
		}
		cleaned := anthropic.StripANSI(text)
		encoded, err := json.Marshal(cleaned)
		if err != nil {
			return nil, 0, 0, false
		}
		return encoded, seqCount, len(text) - len(cleaned), true

	case '[':
		var blocks []map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &blocks); err != nil {
			return nil, 0, 0, false
		}
		totalSeq := 0
		totalSaved := 0
		changed := false
		for i, b := range blocks {
			var bt string
			if rawT, ok := b["type"]; ok {
				_ = json.Unmarshal(rawT, &bt)
			}
			if bt != "text" {
				continue
			}
			var txt string
			if rawTxt, ok := b["text"]; ok {
				_ = json.Unmarshal(rawTxt, &txt)
			}
			seqCount, ansiBytes := anthropic.DetectANSI(txt)
			if seqCount == 0 || ansiBytes == 0 {
				continue
			}
			cleaned := anthropic.StripANSI(txt)
			encodedTxt, err := json.Marshal(cleaned)
			if err != nil {
				continue
			}
			blocks[i]["text"] = encodedTxt
			totalSeq += seqCount
			totalSaved += len(txt) - len(cleaned)
			changed = true
		}
		if !changed {
			return nil, 0, 0, false
		}
		encoded, err := json.Marshal(blocks)
		if err != nil {
			return nil, 0, 0, false
		}
		return encoded, totalSeq, totalSaved, true
	}

	return nil, 0, 0, false
}

func normalizePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	p = strings.ReplaceAll(p, "\\", "/")
	return path.Clean(p)
}
