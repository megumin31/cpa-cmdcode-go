package main

// OpenAI chat-completions to Command Code's Vercel-style wire messages.
// Pure conversion: all runtime state is supplied by the turn snapshot.
import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"strings"
	"time"
)

const (
	generateRoute      = "/alpha/generate"
	defaultMaxTokens   = 64000
	upstreamTimeout    = 10 * time.Minute
	maxErrorBodySample = 2048
)

type chatRequest struct {
	Model               string        `json:"model"`
	Messages            []chatMessage `json:"messages"`
	Tools               []chatTool    `json:"tools"`
	ToolChoice          any           `json:"tool_choice"`
	Temperature         *float64      `json:"temperature"`
	MaxTokens           *int          `json:"max_tokens"`
	MaxCompletionTokens *int          `json:"max_completion_tokens"`
	ReasoningEffort     *string       `json:"reasoning_effort"`
}
type chatMessage struct {
	Role             string         `json:"role"`
	Content          any            `json:"content"`
	ToolCalls        []chatToolCall `json:"tool_calls"`
	ToolCallID       string         `json:"tool_call_id"`
	Name             string         `json:"name"`
	ReasoningContent any            `json:"reasoning_content"`
	Reasoning        any            `json:"reasoning"`
}

type chatTool struct {
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
}

type chatFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// ---------------------------------------------------------------------------
// Wire envelope
// ---------------------------------------------------------------------------

type wireConfig struct {
	WorkingDir    string   `json:"workingDir"`
	Date          string   `json:"date"`
	Environment   string   `json:"environment"`
	Structure     []string `json:"structure"`
	IsGitRepo     bool     `json:"isGitRepo"`
	CurrentBranch string   `json:"currentBranch"`
	MainBranch    string   `json:"mainBranch"`
	GitStatus     string   `json:"gitStatus"`
	RecentCommits []string `json:"recentCommits"`
}

type wireParams struct {
	Model           string   `json:"model"`
	System          string   `json:"system,omitempty"`
	Messages        []any    `json:"messages"`
	Tools           []any    `json:"tools,omitempty"`
	MaxTokens       int      `json:"max_tokens"`
	Stream          bool     `json:"stream"`
	Temperature     *float64 `json:"temperature,omitempty"`
	ReasoningEffort *string  `json:"reasoning_effort,omitempty"`
}

type generateRequest struct {
	Config         wireConfig `json:"config"`
	Memory         *string    `json:"memory"`
	Taste          any        `json:"taste"`
	Skills         any        `json:"skills"`
	PermissionMode string     `json:"permissionMode"`
	ThreadID       string     `json:"threadId"`
	Mode           string     `json:"mode"`
	Params         wireParams `json:"params"`
}

// ---------------------------------------------------------------------------
// Wire events
func buildEnvelopeWithSnapshot(upstreamModel string, payload []byte, snap turnSnapshot) ([]byte, error) {
	return buildEnvelopeForSession(upstreamModel, payload, snap, newUUID())
}

func buildEnvelopeForSession(upstreamModel string, payload []byte, snap turnSnapshot, sessionID string) ([]byte, error) {
	var chat chatRequest
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if err := decoder.Decode(&chat); err != nil {
		return nil, fmt.Errorf("cmdcode-go: invalid openai chat payload: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("cmdcode-go: chat payload must contain exactly one JSON value")
	}
	selection := parseModelName(upstreamModel)
	if canonical, ok := snap.models.Match(upstreamModel); ok {
		selection.base = canonical
	}
	effort, err := snap.models.reasoningEffort(selection, chat.ReasoningEffort)
	if err != nil {
		return nil, err
	}
	system, messages, err := toWireMessages(chat.Messages)
	if err != nil {
		return nil, err
	}
	tools, err := toWireTools(chat.Tools)
	if err != nil {
		return nil, err
	}
	if toolChoiceNone(chat.ToolChoice) {
		tools = []any{}
	}
	params := wireParams{
		Model:           selection.base,
		System:          system,
		Messages:        messages,
		Tools:           tools,
		MaxTokens:       snap.models.maxTokens(chat, selection.base),
		Stream:          true, // the gateway rejects stream:false
		Temperature:     chat.Temperature,
		ReasoningEffort: effort,
	}
	req := generateRequest{
		Config: wireConfig{
			WorkingDir:    ".",
			Date:          time.Now().Format("2006-01-02"),
			Environment:   runtime.GOOS,
			Structure:     []string{},
			CurrentBranch: "",
			MainBranch:    "",
			GitStatus:     "",
			RecentCommits: []string{},
		},
		PermissionMode: snap.config.permissionMode(),
		ThreadID:       sessionID,
		Mode:           "agent",
		Params:         params,
	}
	return json.Marshal(req)
}

// Explicit user budgets are clamped only by a known gateway cap. Reference
// metadata and fallbacks choose a default, not a fabricated hard limit.
func (s *modelSnapshot) maxTokens(chat chatRequest, id string) int {
	d, known := s.Definition(id)
	want := defaultMaxTokens
	explicit := false
	if chat.MaxTokens != nil && *chat.MaxTokens > 0 {
		want, explicit = *chat.MaxTokens, true
	} else if chat.MaxCompletionTokens != nil && *chat.MaxCompletionTokens > 0 {
		want, explicit = *chat.MaxCompletionTokens, true
	}
	if known {
		if !explicit && d.output > 0 {
			if d.outputSource == "fallback" {
				// A configured fallback is a request default, never a model cap.
				want = int(d.output)
			} else {
				want = min(want, int(d.output))
			}
		}
		if d.gatewayOutput > 0 {
			want = min(want, int(d.gatewayOutput))
		}
	}
	return want
}

// toWireMessages converts OpenAI messages to Vercel ModelMessage parts.
// A single OpenAI user turn carrying tool results fans out to a tool-role
// message followed by a user-role message, mirroring the CLI mapping.
func toWireMessages(messages []chatMessage) (string, []any, error) {
	var system strings.Builder
	out := []any{}
	toolNames := map[string]string{}
	for _, msg := range messages {
		switch msg.Role {
		case "system", "developer":
			if text := messageText(msg.Content); text != "" {
				if system.Len() > 0 {
					system.WriteString("\n\n")
				}
				system.WriteString(text)
			}
		case "assistant":
			parts := []any{}
			if reasoning := messageReasoning(msg); reasoning != "" {
				parts = append(parts, map[string]any{"type": "reasoning", "text": reasoning})
			}
			if text := messageText(msg.Content); text != "" {
				parts = append(parts, map[string]any{"type": "text", "text": text})
			}
			for _, call := range msg.ToolCalls {
				input, err := parseToolInput(call.Function.Arguments)
				if err != nil {
					return "", nil, fmt.Errorf("cmdcode-go: tool %q arguments: %w", call.Function.Name, err)
				}
				id := call.ID
				if id == "" {
					id = newID()
				}
				toolNames[id] = call.Function.Name
				parts = append(parts, map[string]any{
					"type": "tool-call", "toolCallId": id,
					"toolName": call.Function.Name, "input": input,
				})
			}
			out = append(out, map[string]any{"role": "assistant", "content": parts})
		case "tool":
			name := msg.Name
			if name == "" {
				name = toolNames[msg.ToolCallID]
			}
			if name == "" {
				name = "unknown"
			}
			out = append(out, map[string]any{"role": "tool", "content": []any{
				map[string]any{
					"type": "tool-result", "toolCallId": msg.ToolCallID,
					"toolName": name,
					"output":   map[string]any{"type": "text", "value": toolResultText(msg.Content)},
				},
			}})
		default: // user and anything else
			userParts := orderedUserParts(msg.Content)
			if len(userParts) > 0 {
				out = append(out, map[string]any{"role": "user", "content": userParts})
			}
		}
	}
	if out == nil {
		out = []any{}
	}
	return system.String(), out, nil
}

// messageText flattens string or part-array content to plain text.
func messageText(content any) string {
	texts, _ := messageRichText(content)
	return strings.Join(texts, "")
}

// messageReasoning collects thinking-channel text for an assistant turn.
// Thinking models (deepseek-v4.1-flash and kin) reject multi-turn history
// that drops it ("reasoning_content in the thinking mode must be passed
// back"), so every spelling the ecosystem emits is accepted: top-level
// reasoning_content / reasoning (string, {text/content/reasoning_content}
// object, or array of such), plus content-array parts typed reasoning,
// reasoning_content, thinking, or reasoning_text.
func messageReasoning(msg chatMessage) string {
	var sb strings.Builder
	if s := reasoningFieldText(msg.ReasoningContent); s != "" {
		sb.WriteString(s)
	}
	if s := reasoningFieldText(msg.Reasoning); s != "" {
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(s)
	}
	if s := contentReasoningText(msg.Content); s != "" {
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(s)
	}
	return sb.String()
}

// contentReasoningText extracts reasoning text embedded in a content array.
func contentReasoningText(content any) string {
	parts, ok := content.([]any)
	if !ok {
		return ""
	}
	var sb strings.Builder
	for _, item := range parts {
		part, ok := item.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := part["type"].(string)
		switch typ {
		case "reasoning", "reasoning_content", "reasoning_text", "thinking":
			if s := reasoningPartText(part); s != "" {
				if sb.Len() > 0 {
					sb.WriteString("\n")
				}
				sb.WriteString(s)
			}
		}
	}
	return sb.String()
}

// reasoningPartText reads one content-array reasoning part. Providers disagree
// on the payload key (text, content, reasoning_content, reasoning), so try
// them in order before falling back to nested objects.
func reasoningPartText(part map[string]any) string {
	for _, key := range []string{"text", "content", "reasoning_content", "reasoning", "thinking"} {
		if s := reasoningFieldText(part[key]); s != "" {
			return s
		}
	}
	return ""
}

// reasoningFieldText coerces a reasoning-typed JSON value to plain text.
func reasoningFieldText(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []any:
		var sb strings.Builder
		for _, item := range t {
			if s := reasoningFieldText(item); s != "" {
				if sb.Len() > 0 {
					sb.WriteString("\n")
				}
				sb.WriteString(s)
			}
		}
		return sb.String()
	case map[string]any:
		for _, key := range []string{"text", "content", "reasoning_content", "reasoning", "thinking", "value"} {
			if raw, ok := t[key]; ok {
				if s := reasoningFieldText(raw); s != "" {
					return s
				}
			}
		}
		return ""
	default:
		return ""
	}
}

// orderedUserParts preserves text/image adjacency and relative ordering.
func orderedUserParts(content any) []any {
	if text, ok := content.(string); ok {
		if text == "" {
			return nil
		}
		return []any{map[string]any{"type": "text", "text": text}}
	}
	var out []any
	if parts, ok := content.([]any); ok {
		for _, item := range parts {
			part, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch part["type"] {
			case "text", "input_text":
				if text, ok := part["text"].(string); ok && text != "" {
					out = append(out, map[string]any{"type": "text", "text": text})
				}
			case "image_url":
				if image := toWireImage(part["image_url"]); image != nil {
					out = append(out, image)
				}
			}
		}
	}
	return out
}

func messageRichText(content any) ([]string, []any) {
	var texts []string
	var images []any
	switch c := content.(type) {
	case nil:
	case string:
		if c != "" {
			texts = append(texts, c)
		}
	case []any:
		for _, item := range c {
			part, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch part["type"] {
			case "text":
				if s, _ := part["text"].(string); s != "" {
					texts = append(texts, s)
				}
			case "image_url":
				if img := toWireImage(part["image_url"]); img != nil {
					images = append(images, img)
				}
			case "input_text":
				if s, _ := part["text"].(string); s != "" {
					texts = append(texts, s)
				}
			}
		}
	}
	return texts, images
}

// toWireImage maps an OpenAI image_url to a ModelMessage image part. Data
// URLs pass through with their media type; remote URLs are forwarded as-is
// best-effort since the CLI bundle shows no client-side fetch.
func toWireImage(raw any) any {
	url := ""
	switch v := raw.(type) {
	case string:
		url = v
	case map[string]any:
		url, _ = v["url"].(string)
	}
	if url == "" {
		return nil
	}
	mime := "image/png"
	if strings.HasPrefix(url, "data:") {
		if semi := strings.Index(url, ";"); semi > 5 {
			mime = url[5:semi]
		}
	}
	return map[string]any{"type": "image", "image": url, "mimeType": mime}
}

func toolResultText(content any) string {
	switch c := content.(type) {
	case nil:
		return ""
	case string:
		return c
	case []any:
		var sb strings.Builder
		for _, item := range c {
			part, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if s, _ := part["text"].(string); s != "" {
				if sb.Len() > 0 {
					sb.WriteString("\n")
				}
				sb.WriteString(s)
			}
		}
		return sb.String()
	default:
		raw, err := json.Marshal(content)
		if err != nil {
			return ""
		}
		return string(raw)
	}
}

func parseToolInput(args string) (json.RawMessage, error) {
	trimmed := strings.TrimSpace(args)
	if trimmed == "" {
		return json.RawMessage(`{}`), nil
	}
	return compactJSON(json.RawMessage(trimmed))
}

func toWireTools(tools []chatTool) ([]any, error) {
	out := []any{}
	for _, tool := range tools {
		if tool.Type != "" && tool.Type != "function" {
			continue
		}
		if tool.Function.Name == "" {
			continue
		}
		schema, err := normalizeSchema(tool.Function.Parameters)
		if err != nil {
			return nil, fmt.Errorf("cmdcode-go: tool %q schema: %w", tool.Function.Name, err)
		}
		out = append(out, map[string]any{
			"name": tool.Function.Name, "description": tool.Function.Description,
			"input_schema": schema,
		})
	}
	return out, nil
}

// normalizeSchema accepts a missing/null no-argument schema and adds the
// gateway's required object type to typeless records. Raw field values retain
// numeric precision. Invalid or non-object schemas fail instead of silently
// replacing the client's constraints with an unrestricted object.
func normalizeSchema(raw json.RawMessage) (json.RawMessage, error) {
	empty := json.RawMessage(`{"type":"object","properties":{}}`)
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return empty, nil
	}
	// Some clients serialize the schema object as a JSON string. Decode only
	// that wrapper; its contents still must be an object and retain raw values.
	if trimmed[0] == '"' {
		var inner string
		if err := json.Unmarshal(trimmed, &inner); err != nil {
			return nil, fmt.Errorf("invalid encoded schema: %w", err)
		}
		trimmed = bytes.TrimSpace([]byte(inner))
		if len(trimmed) == 0 || trimmed[0] != '{' {
			return nil, fmt.Errorf("expected an object JSON Schema")
		}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &fields); err != nil {
		return nil, fmt.Errorf("expected an object JSON Schema: %w", err)
	}
	if typ, exists := fields["type"]; exists {
		var value string
		if err := json.Unmarshal(typ, &value); err != nil || value != "object" {
			return nil, fmt.Errorf("gateway requires schema type object")
		}
		return compactJSON(trimmed)
	}
	fields["type"] = json.RawMessage(`"object"`)
	return json.Marshal(fields)
}

// toolChoiceNone reports an explicit "no tools" directive, either the string
// form or {"type": "none"}. Anything else (auto/required/specific) keeps the
// declared tools: the gateway has no forced-call mode, so "required" degrades
// to offering them like the official CLI does.
func toolChoiceNone(choice any) bool {
	switch c := choice.(type) {
	case nil:
		return false
	case string:
		return strings.EqualFold(strings.TrimSpace(c), "none")
	case map[string]any:
		if t, _ := c["type"].(string); strings.EqualFold(strings.TrimSpace(t), "none") {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
