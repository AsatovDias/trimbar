package anthropic

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"
)

// StreamResult holds the aggregated telemetry extracted from an Anthropic SSE stream
// (or a non-streaming JSON MessageResponse).
type StreamResult struct {
	MessageID    string
	Model        string
	StopReason   string
	Usage        Usage
	EventCount   int
	Completed    bool
	ErrorType    string
	ErrorMessage string
	FirstTokenAt time.Time
}

// SSEParser incrementally parses an Anthropic Server-Sent Events byte stream
// without buffering the entire response body.
type SSEParser struct {
	lineBuf   []byte
	eventType string
	dataLines []string
	result    StreamResult
}

// NewSSEParser returns a fresh stateful SSE parser.
func NewSSEParser() *SSEParser {
	return &SSEParser{}
}

// Write implements io.Writer, feeding arbitrarily chunked SSE bytes into the parser.
// It always returns len(p), nil so callers can safely attach it to a streaming tee.
func (p *SSEParser) Write(b []byte) (int, error) {
	n := len(b)
	for len(b) > 0 {
		idx := bytes.IndexByte(b, '\n')
		if idx < 0 {
			p.lineBuf = append(p.lineBuf, b...)
			break
		}
		p.lineBuf = append(p.lineBuf, b[:idx]...)
		line := string(bytes.TrimSuffix(p.lineBuf, []byte{'\r'}))
		p.lineBuf = p.lineBuf[:0]
		p.processLine(line)
		b = b[idx+1:]
	}
	return n, nil
}

// Finish flushes any trailing line/event when the underlying stream reaches EOF
// and returns the final StreamResult.
func (p *SSEParser) Finish() StreamResult {
	if len(p.lineBuf) > 0 {
		line := string(bytes.TrimSuffix(p.lineBuf, []byte{'\r'}))
		p.lineBuf = p.lineBuf[:0]
		p.processLine(line)
	}
	p.dispatchEvent()
	return p.result
}

// Result returns a snapshot of the currently parsed StreamResult.
func (p *SSEParser) Result() StreamResult {
	return p.result
}

func (p *SSEParser) processLine(line string) {
	if line == "" {
		p.dispatchEvent()
		return
	}
	// SSE comment line
	if strings.HasPrefix(line, ":") {
		return
	}

	field, value, found := strings.Cut(line, ":")
	if !found {
		return
	}
	// Per SSE spec, strip a single optional leading space after ':'.
	if strings.HasPrefix(value, " ") {
		value = value[1:]
	}

	switch field {
	case "event":
		p.eventType = value
	case "data":
		p.dataLines = append(p.dataLines, value)
	}
}

func (p *SSEParser) dispatchEvent() {
	if len(p.dataLines) == 0 && p.eventType == "" {
		return
	}
	evType := p.eventType
	payload := strings.Join(p.dataLines, "\n")
	p.eventType = ""
	p.dataLines = p.dataLines[:0]

	p.result.EventCount++
	if payload == "" || payload == "[DONE]" {
		return
	}

	dataBytes := []byte(payload)

	// If event: header was omitted, fall back to the JSON "type" field.
	if evType == "" {
		var envelope struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(dataBytes, &envelope); err == nil {
			evType = envelope.Type
		}
	}

	switch evType {
	case "message_start":
		var ev MessageStartEvent
		if err := json.Unmarshal(dataBytes, &ev); err != nil {
			return
		}
		if ev.Message.ID != "" {
			p.result.MessageID = ev.Message.ID
		}
		if ev.Message.Model != "" {
			p.result.Model = ev.Message.Model
		}
		p.result.Usage.InputTokens = ev.Message.Usage.InputTokens
		p.result.Usage.CacheCreationInputTokens = ev.Message.Usage.CacheCreationInputTokens
		p.result.Usage.CacheReadInputTokens = ev.Message.Usage.CacheReadInputTokens
		if ev.Message.Usage.OutputTokens > p.result.Usage.OutputTokens {
			p.result.Usage.OutputTokens = ev.Message.Usage.OutputTokens
		}
		if ev.Message.Usage.CacheCreation != nil {
			cc := *ev.Message.Usage.CacheCreation
			p.result.Usage.CacheCreation = &cc
		}

	case "content_block_delta":
		if p.result.FirstTokenAt.IsZero() {
			p.result.FirstTokenAt = time.Now()
		}

	case "message_delta":
		var ev MessageDeltaEvent
		if err := json.Unmarshal(dataBytes, &ev); err != nil {
			return
		}
		if ev.Delta.StopReason != "" {
			p.result.StopReason = ev.Delta.StopReason
		}
		if ev.Usage != nil {
			if ev.Usage.OutputTokens != nil {
				p.result.Usage.OutputTokens = *ev.Usage.OutputTokens
			}
			// Only update input/cache metrics if explicitly present and non-zero,
			// so partial message_delta usage objects do not zero out message_start counts.
			if ev.Usage.InputTokens != nil && *ev.Usage.InputTokens > 0 {
				p.result.Usage.InputTokens = *ev.Usage.InputTokens
			}
			if ev.Usage.CacheCreationInputTokens != nil && *ev.Usage.CacheCreationInputTokens > 0 {
				p.result.Usage.CacheCreationInputTokens = *ev.Usage.CacheCreationInputTokens
			}
			if ev.Usage.CacheReadInputTokens != nil && *ev.Usage.CacheReadInputTokens > 0 {
				p.result.Usage.CacheReadInputTokens = *ev.Usage.CacheReadInputTokens
			}
			if ev.Usage.CacheCreation != nil {
				cc := *ev.Usage.CacheCreation
				p.result.Usage.CacheCreation = &cc
			}
		}

	case "message_stop":
		p.result.Completed = true

	case "error":
		var ev ErrorEvent
		if err := json.Unmarshal(dataBytes, &ev); err == nil {
			p.result.ErrorType = ev.Error.Type
			p.result.ErrorMessage = ev.Error.Message
		}
	}
}

// ParseJSONResponse parses a non-streaming /v1/messages JSON response into StreamResult.
func ParseJSONResponse(body []byte) (StreamResult, error) {
	var resp MessageResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return StreamResult{}, err
	}
	res := StreamResult{
		MessageID:  resp.ID,
		Model:      resp.Model,
		StopReason: resp.StopReason,
		Usage:      resp.Usage,
		Completed:  resp.Type == "message" || resp.ID != "",
	}
	return res, nil
}
