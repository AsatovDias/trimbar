package trimmer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"github.com/trimbar/trimbar/internal/anthropic"
)

// TrimResult describes the outcome of cache-safe active trimming on a /v1/messages request.
type TrimResult struct {
	Body              []byte   // Trimmed JSON payload (or original bytes untouched if Modified == false)
	Modified          bool     // True only when at least one post-cache_control block was changed
	FrozenMessageIdx  int      // Index of the last message containing cache_control (-1 if none in messages)
	FrozenBlockIdx    int      // Index of the last block containing cache_control within FrozenMessageIdx (-1 if none)
	TrimmedBytes      int      // Exact byte reduction achieved across modified blocks
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
//        by an intervening Edit/Write) with a short placeholder.
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

	// Pass 1: Locate the exact frozen prefix boundary (last cache_control in messages).
	frozenMsgIdx, frozenBlockIdx := findFrozenBoundary(rawMessages)
	res.FrozenMessageIdx = frozenMsgIdx
	res.FrozenBlockIdx = frozenBlockIdx

	// Pass 2: Walk messages chronologically from index 0 to track full-file Reads and
	// Edit/Write invalidations across the entire history, while ONLY mutating
	// tool_result blocks strictly after (frozenMsgIdx, frozenBlockIdx).
	seenReads := make(map[string]bool)
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

				// Determine if this tool_result corresponds to a full-file Read call.
				meta, isTrackedRead := pendingReads[toolUseID]
				isDuplicateRead := false
				if isTrackedRead {
					delete(pendingReads, toolUseID)
					if !isErr {
						if seenReads[meta.normPath] {
							isDuplicateRead = true
						} else {
							seenReads[meta.normPath] = true
						}
					}
				}

				// Frozen prefix protection: NEVER mutate anything at or before cache_control!
				if !isAfterFrozen {
					continue
				}

				rawResultContent, hasResultContent := blockObj["content"]
				if !hasResultContent {
					continue
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

// duplicateReadPlaceholder returns the concise marker replacing redundant Read results.
func duplicateReadPlaceholder(filePath string) string {
	return fmt.Sprintf("[Trimbar: identical content of %s already loaded earlier in context]", filePath)
}

func findFrozenBoundary(rawMessages []json.RawMessage) (int, int) {
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

	return lastMsg, lastBlock
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

func trackAssistantToolUse(blockObj map[string]json.RawMessage, seenReads map[string]bool, pendingReads map[string]readMeta) {
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
