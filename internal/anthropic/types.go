package anthropic

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// CacheControl represents Anthropic's prompt caching directive on a block or tool.
type CacheControl struct {
	Type string `json:"type"`          // typically "ephemeral"
	TTL  string `json:"ttl,omitempty"` // optional "5m" or "1h"
}

// MessageRequest models the relevant fields of POST /v1/messages.
type MessageRequest struct {
	Model     string           `json:"model"`
	MaxTokens int              `json:"max_tokens"`
	Stream    bool             `json:"stream"`
	System    SystemPrompt     `json:"system,omitempty"`
	Tools     []ToolDefinition `json:"tools,omitempty"`
	Messages  []Message        `json:"messages,omitempty"`
	Metadata  map[string]any   `json:"metadata,omitempty"`
}

// SystemPrompt supports both Anthropic string form ("You are...") and block array form.
type SystemPrompt struct {
	Blocks []ContentBlock
	Raw    json.RawMessage
}

// UnmarshalJSON decodes either a JSON string or an array of ContentBlocks.
func (s *SystemPrompt) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	s.Raw = append([]byte(nil), trimmed...)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		s.Blocks = nil
		return nil
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return err
		}
		s.Blocks = []ContentBlock{{Type: "text", Text: text}}
		return nil
	}
	if trimmed[0] == '[' {
		var blocks []ContentBlock
		if err := json.Unmarshal(trimmed, &blocks); err != nil {
			return err
		}
		s.Blocks = blocks
		return nil
	}
	return fmt.Errorf("unexpected system prompt JSON: %s", string(trimmed[:min(32, len(trimmed))]))
}

// Text concatenates all text blocks in the system prompt.
func (s SystemPrompt) Text() string {
	var buf bytes.Buffer
	for i, b := range s.Blocks {
		if b.Text == "" {
			continue
		}
		if i > 0 && buf.Len() > 0 {
			buf.WriteByte('\n')
		}
		buf.WriteString(b.Text)
	}
	return buf.String()
}

// ToolDefinition models an entry in the "tools" array of /v1/messages.
type ToolDefinition struct {
	Type         string          `json:"type,omitempty"`
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema,omitempty"`
	CacheControl *CacheControl   `json:"cache_control,omitempty"`
}

// Message models a turn in the "messages" array.
type Message struct {
	Role    string         `json:"role"`
	Content MessageContent `json:"content"`
}

// MessageContent supports both string content and array of ContentBlocks.
type MessageContent struct {
	Blocks []ContentBlock
	Raw    json.RawMessage
}

// UnmarshalJSON decodes either a JSON string or an array of ContentBlocks.
func (c *MessageContent) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	c.Raw = append([]byte(nil), trimmed...)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		c.Blocks = nil
		return nil
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return err
		}
		c.Blocks = []ContentBlock{{Type: "text", Text: text}}
		return nil
	}
	if trimmed[0] == '[' {
		var blocks []ContentBlock
		if err := json.Unmarshal(trimmed, &blocks); err != nil {
			return err
		}
		c.Blocks = blocks
		return nil
	}
	return fmt.Errorf("unexpected message content JSON: %s", string(trimmed[:min(32, len(trimmed))]))
}

// ContentBlock represents an element inside system, message.content, or tool_result.content.
type ContentBlock struct {
	Type         string            `json:"type"`
	Text         string            `json:"text,omitempty"`
	Thinking     string            `json:"thinking,omitempty"`
	ID           string            `json:"id,omitempty"`
	Name         string            `json:"name,omitempty"`
	Input        json.RawMessage   `json:"input,omitempty"`
	ToolUseID    string            `json:"tool_use_id,omitempty"`
	Content      ToolResultContent `json:"content,omitempty"`
	IsError      bool              `json:"is_error,omitempty"`
	CacheControl *CacheControl     `json:"cache_control,omitempty"`
}

// ToolResultContent supports both a string and an array of nested ContentBlocks.
type ToolResultContent struct {
	Text   string
	Blocks []ContentBlock
	Raw    json.RawMessage
}

// UnmarshalJSON decodes tool_result.content as either a string or []ContentBlock.
func (t *ToolResultContent) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	t.Raw = append([]byte(nil), trimmed...)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return err
		}
		t.Text = text
		return nil
	}
	if trimmed[0] == '[' {
		var blocks []ContentBlock
		if err := json.Unmarshal(trimmed, &blocks); err != nil {
			return err
		}
		t.Blocks = blocks
		return nil
	}
	return nil
}

// CombinedText returns the textual content of a tool_result block.
func (t ToolResultContent) CombinedText() string {
	if len(t.Blocks) == 0 {
		return t.Text
	}
	var buf bytes.Buffer
	if t.Text != "" {
		buf.WriteString(t.Text)
	}
	for _, b := range t.Blocks {
		if b.Text != "" {
			if buf.Len() > 0 {
				buf.WriteByte('\n')
			}
			buf.WriteString(b.Text)
		}
	}
	return buf.String()
}

// ByteLen returns the size in bytes of the tool_result payload.
func (t ToolResultContent) ByteLen() int {
	if text := t.CombinedText(); len(text) > 0 {
		return len(text)
	}
	return len(t.Raw)
}

// CacheCreation represents Anthropic's breakdown of cache creation tokens by TTL.
type CacheCreation struct {
	Ephemeral5mInputTokens int64 `json:"ephemeral_5m_input_tokens"`
	Ephemeral1hInputTokens int64 `json:"ephemeral_1h_input_tokens"`
}

// Usage represents the Anthropic API usage block returned in SSE and JSON responses.
type Usage struct {
	InputTokens              int64          `json:"input_tokens"`
	OutputTokens             int64          `json:"output_tokens"`
	CacheCreationInputTokens int64          `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64          `json:"cache_read_input_tokens"`
	CacheCreation            *CacheCreation `json:"cache_creation,omitempty"`
}

// TotalInputTokens returns uncached input + cache creation + cache read tokens.
func (u Usage) TotalInputTokens() int64 {
	return u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
}

// CacheHitRatio returns cache_read_input_tokens / TotalInputTokens in [0, 1].
func (u Usage) CacheHitRatio() float64 {
	total := u.TotalInputTokens()
	if total <= 0 {
		return 0
	}
	return float64(u.CacheReadInputTokens) / float64(total)
}

// MessageResponse models a non-streaming /v1/messages JSON response.
type MessageResponse struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Role       string `json:"role"`
	Model      string `json:"model"`
	StopReason string `json:"stop_reason"`
	Usage      Usage  `json:"usage"`
}

// MessageStartEvent models Anthropic's SSE "message_start" payload.
type MessageStartEvent struct {
	Type    string          `json:"type"`
	Message MessageResponse `json:"message"`
}

// MessageDeltaEvent models Anthropic's SSE "message_delta" payload.
type MessageDeltaEvent struct {
	Type  string `json:"type"`
	Delta struct {
		StopReason   string  `json:"stop_reason"`
		StopSequence *string `json:"stop_sequence"`
	} `json:"delta"`
	Usage *DeltaUsage `json:"usage,omitempty"`
}

// DeltaUsage uses pointers so omitted fields in message_delta never clobber
// input/cache token counts captured in message_start.
type DeltaUsage struct {
	InputTokens              *int64         `json:"input_tokens,omitempty"`
	OutputTokens             *int64         `json:"output_tokens,omitempty"`
	CacheCreationInputTokens *int64         `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     *int64         `json:"cache_read_input_tokens,omitempty"`
	CacheCreation            *CacheCreation `json:"cache_creation,omitempty"`
}

// ErrorEvent models an Anthropic SSE "error" event or top-level HTTP API error payload.
type ErrorEvent struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}
