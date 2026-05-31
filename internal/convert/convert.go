package convert

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)


// ── OpenAI / Anthropic → DeepSeek prompt ─────────────────────────────────────

func injectTools(system string, tools []map[string]any) string {
	if len(tools) == 0 {
		return system
	}
	var b strings.Builder
	b.WriteString(system)
	b.WriteString("\n\n<tools>\n")
	for _, t := range tools {
		enc, _ := json.MarshalIndent(t, "", "  ")
		b.Write(enc)
		b.WriteByte('\n')
	}
	b.WriteString("</tools>\n\n")
	b.WriteString("When you need to call a tool, output a single XML block:\n")
	b.WriteString("<tool_call>\n{\"name\": \"tool_name\", \"arguments\": {...}}\n</tool_call>\n\n")
	b.WriteString("After the tool result is provided, continue your response.")
	return b.String()
}

// MessagesToPrompt converts OpenAI-style messages to a (systemPrompt, userPrompt) pair.
// tools may be nil. Returns empty userPrompt if no user message is found.
func MessagesToPrompt(messages []map[string]any, tools []map[string]any) (string, string) {
	var systemParts, historyParts []string
	var lastUser string

	for _, m := range messages {
		if m["role"] == "system" {
			systemParts = append(systemParts, extractText(m["content"]))
			break
		}
	}

	if len(tools) > 0 {
		base := strings.Join(systemParts, "\n\n")
		systemParts = []string{injectTools(base, tools)}
	}

	nonSystem := make([]map[string]any, 0, len(messages))
	for _, m := range messages {
		if m["role"] != "system" {
			nonSystem = append(nonSystem, m)
		}
	}

	for i, m := range nonSystem {
		role := fmt.Sprint(m["role"])
		content := extractText(m["content"])
		if i == len(nonSystem)-1 && role == "user" {
			lastUser = content
			break
		}
		switch role {
		case "user":
			historyParts = append(historyParts, "User: "+content)
		case "assistant":
			historyParts = append(historyParts, "Assistant: "+content)
		case "tool":
			id, _ := m["tool_call_id"].(string)
			historyParts = append(historyParts, "Tool result ("+id+"): "+content)
		}
	}

	if len(historyParts) > 0 {
		systemParts = append(systemParts, "Conversation history:\n"+strings.Join(historyParts, "\n\n"))
	}

	return strings.Join(systemParts, "\n\n"), lastUser
}

func extractText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, item := range v {
			switch block := item.(type) {
			case string:
				parts = append(parts, block)
			case map[string]any:
				switch block["type"] {
				case "text":
					parts = append(parts, fmt.Sprint(block["text"]))
				case "tool_result":
					parts = append(parts, extractText(block["content"]))
				}
			}
		}
		return strings.Join(parts, "")
	}
	return fmt.Sprint(content)
}

// ── Tool call parsing ─────────────────────────────────────────────────────────

var toolCallRe = regexp.MustCompile(`(?s)<tool_call>\s*(\{.*?\})\s*</tool_call>`)

// ParseToolCalls extracts <tool_call> blocks from model output.
// Returns (cleanText, toolCalls) where toolCalls is in OpenAI format.
func ParseToolCalls(text string) (string, []map[string]any) {
	var calls []map[string]any
	idx := 0
	clean := toolCallRe.ReplaceAllStringFunc(text, func(m string) string {
		sub := toolCallRe.FindStringSubmatch(m)
		if len(sub) < 2 {
			return m
		}
		var parsed map[string]any
		if json.Unmarshal([]byte(sub[1]), &parsed) != nil {
			return m
		}
		args := parsed["arguments"]
		argsJSON, _ := json.Marshal(args)
		id := fmt.Sprintf("call_%08x", idx)
		idx++
		calls = append(calls, map[string]any{
			"id":   id,
			"type": "function",
			"function": map[string]any{
				"name":      fmt.Sprint(parsed["name"]),
				"arguments": string(argsJSON),
			},
		})
		return ""
	})
	return strings.TrimSpace(clean), calls
}

// ── DeepSeek → OpenAI ────────────────────────────────────────────────────────

func ChunkToOpenAI(text, model, completionID string) map[string]any {
	return map[string]any{
		"id":      completionID,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{"role": "assistant", "content": text},
			"finish_reason": nil,
		}},
	}
}

func FinishToOpenAI(fullText, model, completionID string, toolCalls []map[string]any) map[string]any {
	delta := map[string]any{}
	finish := "stop"
	if len(toolCalls) > 0 {
		delta["tool_calls"] = toolCalls
		finish = "tool_calls"
	} else {
		delta["content"] = fullText
	}
	return map[string]any{
		"id":      completionID,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": finish,
		}},
	}
}

func ResponseToOpenAI(fullText, model, completionID string, toolCalls []map[string]any) map[string]any {
	msg := map[string]any{"role": "assistant"}
	finish := "stop"
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
		msg["content"] = nil
		finish = "tool_calls"
	} else {
		msg["content"] = fullText
	}
	return map[string]any{
		"id":      completionID,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       msg,
			"finish_reason": finish,
		}},
		"usage": map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
	}
}

// ── DeepSeek → Anthropic ─────────────────────────────────────────────────────

func StartToAnthropic(model, msgID string) []map[string]any {
	return []map[string]any{
		{
			"type": "message_start",
			"message": map[string]any{
				"id":            msgID,
				"type":          "message",
				"role":          "assistant",
				"content":       []any{},
				"model":         model,
				"stop_reason":   nil,
				"stop_sequence": nil,
				"usage":         map[string]any{"input_tokens": 1, "output_tokens": 1},
			},
		},
		{"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "text", "text": ""}},
	}
}

func ChunkToAnthropic(text string) []map[string]any {
	return []map[string]any{
		{"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": text}},
	}
}

func FinishToAnthropic(stopReason string, toolCalls []map[string]any) []map[string]any {
	var events []map[string]any
	for i, tc := range toolCalls {
		fn, _ := tc["function"].(map[string]any)
		argsStr, _ := fn["arguments"].(string)
		events = append(events,
			map[string]any{"type": "content_block_start", "index": i + 1,
				"content_block": map[string]any{
					"type":  "tool_use",
					"id":    tc["id"],
					"name":  fn["name"],
					"input": map[string]any{},
				}},
			map[string]any{"type": "content_block_delta", "index": i + 1,
				"delta": map[string]any{
					"type":         "input_json_delta",
					"partial_json": argsStr,
				}},
			map[string]any{"type": "content_block_stop", "index": i + 1},
		)
		stopReason = "tool_use"
	}
	events = append(events,
		map[string]any{"type": "content_block_stop", "index": 0},
		map[string]any{"type": "message_delta",
			"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
			"usage": map[string]any{"output_tokens": 1}},
		map[string]any{"type": "message_stop"},
	)
	return events
}

func BuildAnthropicResponse(text string, toolCalls []map[string]any, msgID string) map[string]any {
	var content []any
	if text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	for _, tc := range toolCalls {
		fn, _ := tc["function"].(map[string]any)
		argsStr, _ := fn["arguments"].(string)
		var argsObj any
		json.Unmarshal([]byte(argsStr), &argsObj) //nolint:errcheck
		content = append(content, map[string]any{
			"type":  "tool_use",
			"id":    tc["id"],
			"name":  fn["name"],
			"input": argsObj,
		})
	}
	stopReason := "end_turn"
	if len(toolCalls) > 0 {
		stopReason = "tool_use"
	}
	return map[string]any{
		"id":            msgID,
		"type":          "message",
		"role":          "assistant",
		"content":       content,
		"model":         "deepseek-chat",
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage":         map[string]any{"input_tokens": 1, "output_tokens": 1},
	}
}

// AnthropicMessagesToOpenAI translates Anthropic-style messages to OpenAI format.
func AnthropicMessagesToOpenAI(messages []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(messages))
	for _, m := range messages {
		role, _ := m["role"].(string)
		out = append(out, map[string]any{
			"role":    role,
			"content": extractText(m["content"]),
		})
	}
	return out
}

// AnthropicToolToOpenAI converts an Anthropic tool definition to OpenAI format.
func AnthropicToolToOpenAI(tool map[string]any) map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        fmt.Sprint(tool["name"]),
			"description": fmt.Sprint(tool["description"]),
			"parameters":  tool["input_schema"],
		},
	}
}
