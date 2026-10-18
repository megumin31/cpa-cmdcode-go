package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func mustEnvelope(t *testing.T, payload string) map[string]any {
	t.Helper()
	raw, err := buildEnvelope("deepseek/deepseek-v4-flash", []byte(payload))
	if err != nil {
		t.Fatalf("buildEnvelope: %v", err)
	}
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	return env
}

func paramsOf(t *testing.T, env map[string]any) map[string]any {
	t.Helper()
	params, ok := env["params"].(map[string]any)
	if !ok {
		t.Fatalf("envelope has no params: %v", env)
	}
	return params
}

func TestBuildEnvelopeForcesStream(t *testing.T) {
	env := mustEnvelope(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	params := paramsOf(t, env)
	if params["stream"] != true {
		t.Fatalf("stream = %v, want true (gateway rejects false)", params["stream"])
	}
	if params["model"] != "deepseek/deepseek-v4-flash" {
		t.Fatalf("model = %v", params["model"])
	}
	for _, key := range []string{"config", "permissionMode", "threadId", "mode"} {
		if _, ok := env[key]; !ok {
			t.Fatalf("envelope missing %q", key)
		}
	}
	cfg := env["config"].(map[string]any)
	for _, key := range []string{"workingDir", "date", "environment", "structure", "isGitRepo"} {
		if _, ok := cfg[key]; !ok {
			t.Fatalf("config missing %q", key)
		}
	}
}

func TestBuildEnvelopeSystemExtraction(t *testing.T) {
	env := mustEnvelope(t, `{"model":"m","messages":[
		{"role":"system","content":"be terse"},
		{"role":"user","content":"hi"}]}`)
	params := paramsOf(t, env)
	if params["system"] != "be terse" {
		t.Fatalf("system = %v", params["system"])
	}
	messages := params["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("messages = %v, want only the user turn", messages)
	}
}

func TestMaxTokensClampedToModelCap(t *testing.T) {
	resetDynamicState(t)
	defaultPlugin.Models = NewModelRegistry([]modelDef{
		{id: "test/small", display: "Small", context: 262000, output: 32768, gatewayOutput: 32768},
		{id: "test/large", display: "Large", context: 1000000, output: 131072},
	}, nil)
	build := func(model, payload string) float64 {
		t.Helper()
		raw, err := buildEnvelope(model, []byte(payload))
		if err != nil {
			t.Fatalf("buildEnvelope: %v", err)
		}
		var env map[string]any
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("unmarshal envelope: %v", err)
		}
		got, ok := env["params"].(map[string]any)["max_tokens"].(float64)
		if !ok {
			t.Fatalf("max_tokens missing: %v", env)
		}
		return got
	}
	hi := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	// Fixed fixture tests clamping independently of upstream roster changes.
	if got := build("test/small", hi); got != 32768 {
		t.Errorf("default budget for test/small = %v, want 32768 (model output cap)", got)
	}
	lo := `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":1024}`
	if got := build("test/small", lo); got != 1024 {
		t.Errorf("small explicit budget = %v, want 1024", got)
	}
	big := `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":200000}`
	if got := build("test/small", big); got != 32768 {
		t.Errorf("oversized budget = %v, want clamped 32768", got)
	}
	if got := build("test/large", hi); got != 64000 {
		t.Errorf("default budget for deepseek-v4-flash = %v, want 64000", got)
	}
	if got := build("unknown/model-xyz", hi); got != 64000 {
		t.Errorf("default budget for unknown model = %v, want 64000", got)
	}
}

func TestBuildEnvelopeToolRoundTrip(t *testing.T) {
	env := mustEnvelope(t, `{"model":"m","messages":[
		{"role":"user","content":"what time is it?"},
		{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_time","arguments":"{\"tz\":\"UTC\"}"}}]},
		{"role":"tool","tool_call_id":"call_1","name":"get_time","content":"12:00"}],
		"tools":[{"type":"function","function":{"name":"get_time","description":"clock","parameters":{"type":"object"}}}]}`)
	params := paramsOf(t, env)
	messages := params["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("messages = %v, want user+assistant+tool", messages)
	}
	assistant := messages[1].(map[string]any)
	parts := assistant["content"].([]any)
	toolCall := parts[len(parts)-1].(map[string]any)
	if toolCall["type"] != "tool-call" || toolCall["toolCallId"] != "call_1" || toolCall["toolName"] != "get_time" {
		t.Fatalf("tool-call part = %v", toolCall)
	}
	toolMsg := messages[2].(map[string]any)
	if toolMsg["role"] != "tool" {
		t.Fatalf("third message role = %v, want tool", toolMsg["role"])
	}
	result := toolMsg["content"].([]any)[0].(map[string]any)
	output := result["output"].(map[string]any)
	if output["value"] != "12:00" {
		t.Fatalf("tool-result output = %v", output)
	}
	tools := params["tools"].([]any)
	schema := tools[0].(map[string]any)
	if _, ok := schema["input_schema"]; !ok {
		t.Fatalf("tool missing input_schema: %v", schema)
	}
}

func TestParseStreamTextUsage(t *testing.T) {
	ndjson := strings.Join([]string{
		`{"type":"text-delta","id":"txt-0","text":"Hello"}`,
		`{"type":"text-delta","id":"txt-0","text":" world"}`,
		`{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":100,"outputTokens":20,"inputTokenDetails":{"cacheReadTokens":40,"cacheWriteTokens":5}}}`,
	}, "\n")
	acc, err := parseStream([]byte(ndjson))
	if err != nil {
		t.Fatalf("parseStream: %v", err)
	}
	raw := acc.toOpenAIResponse("deepseek/deepseek-v4-flash")
	var resp map[string]any
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "Hello world" {
		t.Fatalf("content = %v", msg["content"])
	}
	usage := resp["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(100) || usage["completion_tokens"] != float64(20) {
		t.Fatalf("usage = %v", usage)
	}
	details := usage["prompt_tokens_details"].(map[string]any)
	if details["cached_tokens"] != float64(40) {
		t.Fatalf("cached tokens = %v", details)
	}
}

func TestParseStreamToolCall(t *testing.T) {
	ndjson := strings.Join([]string{
		`{"type":"tool-call","toolName":"get_time","toolCallId":"call_9","input":{"tz":"UTC"}}`,
		`{"type":"finish","finishReason":"tool-calls","totalUsage":{"inputTokens":50,"outputTokens":10,"inputTokenDetails":{}}}`,
	}, "\n")
	acc, err := parseStream([]byte(ndjson))
	if err != nil {
		t.Fatalf("parseStream: %v", err)
	}
	var resp map[string]any
	if err := json.Unmarshal(acc.toOpenAIResponse("m"), &resp); err != nil {
		t.Fatal(err)
	}
	choice := resp["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %v", choice["finish_reason"])
	}
	call := choice["message"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	fn := call["function"].(map[string]any)
	if call["id"] != "call_9" || fn["name"] != "get_time" || fn["arguments"] != `{"tz":"UTC"}` {
		t.Fatalf("tool call = %v", call)
	}
}

func TestParseStreamReasoningAndChunks(t *testing.T) {
	ndjson := strings.Join([]string{
		`{"type":"reasoning-delta","text":"let me think"}`,
		`{"type":"text-delta","text":"done"}`,
		`{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":10,"outputTokens":5,"inputTokenDetails":{}}}`,
	}, "\n")
	acc, err := parseStream([]byte(ndjson))
	if err != nil {
		t.Fatalf("parseStream: %v", err)
	}
	if acc.reasoning.String() != "let me think" {
		t.Fatalf("reasoning = %q", acc.reasoning.String())
	}
	frames := acc.toOpenAIChunks("m")
	if len(frames) != 3 { // reasoning + text + final; host appends [DONE]
		t.Fatalf("got %d frames: %q", len(frames), frames)
	}
	for _, frame := range frames {
		if bytes.HasPrefix(frame, []byte("data:")) {
			t.Fatalf("frame must be raw JSON, host adds SSE framing: %q", frame)
		}
		var obj map[string]any
		if err := json.Unmarshal(frame, &obj); err != nil {
			t.Fatalf("frame is not JSON: %q", frame)
		}
	}
	if !strings.Contains(string(frames[0]), "reasoning_content") {
		t.Fatalf("reasoning not mapped to reasoning_content: %q", frames[0])
	}
	if !strings.Contains(string(frames[2]), `"finish_reason":"stop"`) {
		t.Fatalf("final frame = %q", frames[2])
	}
}

func TestNormalizeSchema(t *testing.T) {
	for _, raw := range []string{``, `null`} {
		got, err := normalizeSchema(json.RawMessage(raw))
		if err != nil || string(got) != `{"type":"object","properties":{}}` {
			t.Fatalf("no-arg schema %q: %s %v", raw, got, err)
		}
	}
	for _, raw := range []string{`[]`, `42`, `"str"`, `{bad`, `{"type":"array"}`, `{"type":null}`} {
		if _, err := normalizeSchema(json.RawMessage(raw)); err == nil {
			t.Errorf("accepted invalid schema %q", raw)
		}
	}
	obj := `{"type":"object","properties":{"a":{"type":"string"}}}`
	if got, err := normalizeSchema(json.RawMessage(obj)); err != nil || string(got) != obj {
		t.Errorf("valid schema: %s %v", got, err)
	}
	bare := `{"properties":{"a":{"type":"string"}}}`
	raw, err := normalizeSchema(json.RawMessage(bare))
	if err != nil {
		t.Fatal(err)
	}
	var gotBare map[string]any
	if err := json.Unmarshal(raw, &gotBare); err != nil {
		t.Fatal(err)
	}
	if gotBare["type"] != "object" {
		t.Errorf("typeless schema type = %v", gotBare["type"])
	}
	if props, _ := json.Marshal(gotBare["properties"]); string(props) != `{"a":{"type":"string"}}` {
		t.Errorf("properties rewritten: %s", props)
	}
	env := mustEnvelope(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"t","parameters":null}}]}`)
	tool := paramsOf(t, env)["tools"].([]any)[0].(map[string]any)
	wire := tool["input_schema"].(map[string]any)
	if wire["type"] != "object" {
		t.Fatalf("input_schema = %v", wire)
	}
}

func TestToolChoiceNoneStripsTools(t *testing.T) {
	withTools := `{"model":"m","messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"t","parameters":{}}}]}`
	env := mustEnvelope(t, withTools)
	if len(paramsOf(t, env)["tools"].([]any)) != 1 {
		t.Fatalf("tools dropped without directive: %v", env)
	}
	for _, choice := range []string{`"none"`, `{"type":"none"}`} {
		payload := `{"model":"m","messages":[{"role":"user","content":"hi"}],
			"tools":[{"type":"function","function":{"name":"t","parameters":{}}}],
			"tool_choice":` + choice + `}`
		env := mustEnvelope(t, payload)
		if tools := paramsOf(t, env)["tools"]; tools != nil {
			if list, ok := tools.([]any); !ok || len(list) != 0 {
				t.Fatalf("tool_choice %s kept tools: %v", choice, tools)
			}
		}
	}
}

func TestNormalizeReasoningEffort(t *testing.T) {
	s := newModelSnapshot([]modelDef{{id: "test/model", efforts: []string{"low", "medium", "high", "xhigh", "max", "off"}}})
	for _, level := range []string{"low", "medium", "high", "xhigh", "max", "  High  "} {
		got, err := s.reasoningEffort(parseModelName("test/model"), &level)
		if err != nil || got == nil || *got != strings.ToLower(strings.TrimSpace(level)) {
			t.Errorf("level %q: %v %v", level, got, err)
		}
	}
	for _, invalid := range []string{"ultra", "8192"} {
		if _, err := s.reasoningEffort(parseModelName("test/model"), &invalid); err == nil {
			t.Errorf("silently changed effort %q", invalid)
		}
	}
	for _, defaultLevel := range []string{"auto", "default", "-1", "", "none", "off", "minimal"} {
		if got, err := s.reasoningEffort(parseModelName("test/model"), &defaultLevel); err != nil || got != nil {
			t.Errorf("default effort: %v %v", got, err)
		}
	}
	env := mustEnvelope(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"HIGH"}`)
	if paramsOf(t, env)["reasoning_effort"] != "high" {
		t.Fatalf("effort not normalized: %v", env)
	}
	if _, err := buildEnvelope("deepseek/deepseek-v4-flash", []byte(`{"messages":[],"reasoning_effort":"none"}`)); err != nil {
		t.Fatal("disable fallback rejected", err)
	}
}

func TestParseStreamError(t *testing.T) {
	if _, err := parseStream([]byte(`{"type":"error","error":"boom"}`)); err == nil {
		t.Fatal("want error for error event")
	}
	if _, err := parseStream([]byte(`   `)); err == nil {
		t.Fatal("want error for empty stream")
	}
}
func TestParseStreamTruncated(t *testing.T) {
	before := usageTurns.truncated.Load()
	ndjson := strings.Join([]string{
		`{"type":"text-delta","text":"partial"}`,
		`{"type":"text-delta","text":" more"}`,
	}, "\n")
	if _, err := parseStream([]byte(ndjson)); err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("err = %v, want truncation failure", err)
	}
	if got := usageTurns.truncated.Load() - before; got != 1 {
		t.Fatalf("truncated turns = %d, want 1", got)
	}
}

func TestStreamZeroFallbackUsage(t *testing.T) {
	with, without := usageTurns.with.Load(), usageTurns.without.Load()
	ndjson := strings.Join([]string{
		`{"type":"text-delta","text":"hi"}`,
		`{"type":"finish","finishReason":"stop"}`,
	}, "\n")
	acc, err := parseStream([]byte(ndjson))
	if err != nil {
		t.Fatalf("parseStream: %v", err)
	}
	if acc.hasUsage {
		t.Fatal("missing totalUsage must not set hasUsage")
	}
	// Buffered path: the final chunk carries zero usage, never omits it.
	frames := acc.toOpenAIChunks("m")
	var final map[string]any
	if err := json.Unmarshal(frames[len(frames)-1], &final); err != nil {
		t.Fatal(err)
	}
	usage, ok := final["usage"].(map[string]any)
	if !ok || usage["prompt_tokens"] != float64(0) || usage["completion_tokens"] != float64(0) {
		t.Fatalf("final usage = %v, want zeros", final["usage"])
	}
	// Live path: the same guarantee on the terminal tail.
	s := newLiveStream("m")
	liveFrames, done := feedAll(t, s, []string{
		`{"type":"text-delta","text":"hi"}`,
		`{"type":"finish","finishReason":"stop"}`,
	})
	if !done {
		t.Fatal("finish event must terminate the stream")
	}
	var liveFinal map[string]any
	if err := json.Unmarshal(liveFrames[len(liveFrames)-1], &liveFinal); err != nil {
		t.Fatal(err)
	}
	if _, ok := liveFinal["usage"].(map[string]any); !ok {
		t.Fatalf("live final usage missing: %q", liveFrames[len(liveFrames)-1])
	}
	if got := usageTurns.without.Load() - without; got != 2 {
		t.Fatalf("zero-fallback turns = %d, want 2 (buffered + live)", got)
	}
	if got := usageTurns.with.Load() - with; got != 0 {
		t.Fatalf("with-usage turns = %d, want 0", got)
	}
}

func TestEnvelopeAdversarial(t *testing.T) {
	big := strings.Repeat("x", 100000)
	cases := map[string]string{
		"empty":          `{}`,
		"null-content":   `{"model":"m","messages":[{"role":"user","content":null}]}`,
		"unknown-role":   `{"model":"m","messages":[{"role":"funct","content":"hi"}]}`,
		"huge-text":      `{"model":"m","messages":[{"role":"user","content":"` + big + `"}]}`,
		"broken-args":    `{"model":"m","messages":[{"role":"assistant","content":"","tool_calls":[{"id":"1","type":"function","function":{"name":"t","arguments":"{bad"}}]}]}`,
		"numeric-part":   `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"weird"}]}]}`,
		"unicode-nul":    "{\"model\":\"m\",\"messages\":[{\"role\":\"user\",\"content\":\"hi \\u0000 🎉\"}]}",
		"developer-role": `{"model":"m","messages":[{"role":"developer","content":"sys"},{"role":"user","content":"hi"}]}`,
	}
	for name, payload := range cases {
		raw, err := buildEnvelope("m", []byte(payload))
		if name == "broken-args" {
			if err == nil {
				t.Error("accepted malformed historical tool arguments")
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		var env map[string]any
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Errorf("%s: not JSON: %v", name, err)
			continue
		}
		if paramsOf(t, env)["stream"] != true {
			t.Errorf("%s: stream not forced", name)
		}
	}
}

func TestConfigureFromHostYAML(t *testing.T) {
	prev := getConfig()
	defer setConfig(prev)
	setConfig(pluginConfig{})
	// Shape mirrors pluginhost runtimeConfigYAML as the host really sends
	// it: {"config_yaml": "<base64>"} (Go []byte JSON encoding).
	yaml := "enabled: true\npriority: 1\napi_key: \"user_test123\"\nbase_url: https://staging-api.commandcode.ai\ncli_version: 9.9.9\n# comment\nproject_slug: e2e\npermission_mode: standard\n"
	payload := `{"config_yaml":"` + base64.StdEncoding.EncodeToString([]byte(yaml)) + `"}`
	raw, err := handleMethod("plugin.reconfigure", []byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"ok":true`) {
		t.Fatalf("reconfigure = %s", raw)
	}
	cfg := getConfig()
	if cfg.APIKey != "user_test123" || cfg.BaseURL != "https://staging-api.commandcode.ai" ||
		cfg.CLIVersion != "9.9.9" || cfg.ProjectSlug != "e2e" || cfg.Permission != "standard" {
		t.Fatalf("config = %+v", cfg)
	}
	// Plain-text payloads stay accepted (older hosts, manual tests).
	setConfig(pluginConfig{})
	if _, err := handleMethod("plugin.reconfigure", []byte(`{"config_yaml":"api_key: user_plain"}`)); err != nil {
		t.Fatal(err)
	}
	if getConfig().APIKey != "user_plain" {
		t.Fatalf("plain config = %+v", getConfig())
	}
}

func TestMatchModel(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"deepseek/deepseek-v4-flash", "deepseek/deepseek-v4-flash", true},
		{"cmdcode-go/deepseek/deepseek-v4-flash", "deepseek/deepseek-v4-flash", true},
		{"cpa-cmdcode-go/deepseek/deepseek-v4-flash", "deepseek/deepseek-v4-flash", true},
		{"cmdcode-go/moonshotai/Kimi-K3", "moonshotai/Kimi-K3", true},
		{"cpa-cmdcode-go/moonshotai/Kimi-K3", "moonshotai/Kimi-K3", true},
		{"Kimi-K3", "moonshotai/Kimi-K3", true},
		{"cmdcode-go/Kimi-K3", "moonshotai/Kimi-K3", true},
		{"gpt-5.6-luna", "gpt-5.6-luna", true},
		{"cmdcode-go/gpt-5.6-luna", "gpt-5.6-luna", true},
		{"__foreign_test_model__", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		got, ok := matchModel(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("matchModel(%q) = (%q,%v), want (%q,%v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestNormalizeBefore(t *testing.T) {
	call := func(from, to string, stream bool, body string) string {
		t.Helper()
		raw, err := handleMethod("response.normalize_before", []byte(
			`{"FromFormat":`+strconv.Quote(from)+`,"ToFormat":`+strconv.Quote(to)+
				`,"Stream":`+strconv.FormatBool(stream)+`,"Body":"`+
				base64.StdEncoding.EncodeToString([]byte(body))+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		var env struct {
			OK     bool `json:"ok"`
			Result struct {
				Body string `json:"Body"`
			} `json:"result"`
		}
		if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
			t.Fatalf("normalize = %s, err %v", raw, err)
		}
		decoded, err := base64.StdEncoding.DecodeString(env.Result.Body)
		if err != nil {
			t.Fatal(err)
		}
		return string(decoded)
	}
	// Claude streaming gets the missing prefix; everything else is untouched.
	if got := call("openai", "claude", true, `{"a":1}`); got != `data: {"a":1}` {
		t.Fatalf("claude stream = %q", got)
	}
	for _, tc := range []struct {
		from, to string
		stream   bool
		body     string
	}{
		{"openai", "openai", true, `{"a":1}`},
		{"openai", "openai-response", true, `{"a":1}`},
		{"openai", "claude", false, `{"a":1}`},
		{"openai", "claude", true, `data: {"a":1}`},
		{"openai", "claude", true, `data: [DONE]`},
		{"openai", "gemini", true, `{"a":1}`},
	} {
		if got := call(tc.from, tc.to, tc.stream, tc.body); got != tc.body {
			t.Fatalf("passthrough %v = %q", tc, got)
		}
	}
}

func TestResolveAPIKeyPrecedence(t *testing.T) {
	prev := getConfig()
	defer setConfig(prev)
	setConfig(pluginConfig{})
	t.Setenv("COMMANDCODE_KEY", "user_env")
	t.Setenv("COMMANDCODE_API_KEY", "")
	t.Setenv("COMMAND_CODE_API_KEY", "")
	if got := resolveAPIKey(map[string]string{"api_key": "user_attr"}); got != "user_attr" {
		t.Fatalf("auth attrs should win, got %q", got)
	}
	if got := resolveAPIKey(nil); got != "user_env" {
		t.Fatalf("env should win over auth file, got %q", got)
	}
	setConfig(pluginConfig{APIKey: "user_cfg"})
	if got := resolveAPIKey(nil); got != "user_cfg" {
		t.Fatalf("config should win over env, got %q", got)
	}
}

func TestModelFiltering(t *testing.T) {
	prev := getConfig()
	defer setConfig(prev)
	full := len(registeredModels())
	if full == 0 {
		t.Fatal("want builtin roster without filters")
	}
	for _, m := range registeredModels() {
		if !strings.HasPrefix(m.ID, "cmdcode-go/") {
			t.Fatalf("model %s missing cmdcode-go/ prefix", m.ID)
		}
	}
	// Allowlist via flexible names.
	setConfig(pluginConfig{Models: []string{"Kimi-K3", "cmdcode-go/deepseek/deepseek-v4-flash"}})
	got := registeredModels()
	if len(got) != 2 {
		t.Fatalf("allowlist registry = %d models, want 2", len(got))
	}
	raw, _ := handleMethod("model.route", []byte(`{"RequestedModel":"gpt-5.6-luna"}`))
	if strings.Contains(string(raw), `"Handled":true`) {
		t.Fatalf("non-allowlisted model must not be claimed: %s", raw)
	}
	// Denylist wins over the default-all.
	setConfig(pluginConfig{DisableModels: []string{"gpt-5.6-luna"}})
	for _, m := range registeredModels() {
		if m.ID == "cmdcode-go/gpt-5.6-luna" || m.ID == "gpt-5.6-luna" {
			t.Fatal("denied model still registered")
		}
	}
	if len(registeredModels()) != full-1 {
		t.Fatalf("denylist registry = %d, want %d", len(registeredModels()), full-1)
	}
	// Block-style YAML lists.
	setConfig(pluginConfig{})
	raw, err := handleMethod("plugin.reconfigure", []byte(`{"config_yaml":"models:\n  - Kimi-K3\n  - gpt-5.6-luna\n"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = raw
	if len(registeredModels()) != 2 {
		t.Fatalf("block-list registry = %d, want 2", len(registeredModels()))
	}
	// Flow-style inline list.
	setConfig(pluginConfig{})
	if _, err := handleMethod("plugin.reconfigure", []byte(`{"config_yaml":"disable_models: Kimi-K3, gpt-5.6-luna"}`)); err != nil {
		t.Fatal(err)
	}
	if modelAllowed("moonshotai/Kimi-K3") || !modelAllowed("deepseek/deepseek-v4-flash") {
		t.Fatal("flow-style denylist misapplied")
	}
}

func TestRouteModelContract(t *testing.T) {
	raw, err := handleMethod("model.route", []byte(`{"RequestedModel":"cmdcode-go/moonshotai/Kimi-K3"}`))
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		OK     bool `json:"ok"`
		Result struct {
			Handled    bool   `json:"Handled"`
			TargetKind string `json:"TargetKind"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK || !env.Result.Handled || env.Result.TargetKind != "self" {
		t.Fatalf("route = %s", raw)
	}
	raw, _ = handleMethod("model.route", []byte(`{"RequestedModel":"__foreign_test_model__"}`))
	if strings.Contains(string(raw), `"Handled":true`) {
		t.Fatalf("foreign model must not be claimed: %s", raw)
	}
}

func TestRegistrationCapabilities(t *testing.T) {
	raw, err := handleMethod("plugin.register", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"executor":true`, `"model_router":true`, `"chat-completions"`, `"response_before_translator":true`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("registration missing %s: %s", want, raw)
		}
	}
}

func TestExecuteWithoutKey(t *testing.T) {
	prev := getConfig()
	defer setConfig(prev)
	setConfig(pluginConfig{})
	t.Setenv("HOME", t.TempDir())
	t.Setenv("COMMANDCODE_CONFIG_DIR", t.TempDir())
	t.Setenv("COMMANDCODE_KEY", "")
	t.Setenv("COMMANDCODE_API_KEY", "")
	t.Setenv("COMMAND_CODE_API_KEY", "")
	raw, err := handleMethod("executor.execute", []byte(`{"Model":"m","Payload":"e30="}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "missing_api_key") {
		t.Fatalf("want missing_api_key, got %s", raw)
	}
}

func TestUUIDIdentities(t *testing.T) {
	// The gateway 400s non-UUID threadId/session-id; pin RFC 4122 v4 shape.
	matched, err := regexp.MatchString(
		`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`, newUUID())
	if err != nil || !matched {
		t.Fatalf("newUUID() = %q, want RFC 4122 v4", newUUID())
	}
	env := mustEnvelope(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	thread, _ := env["threadId"].(string)
	if matched, _ := regexp.MatchString(`^[0-9a-f-]{36}$`, thread); !matched {
		t.Fatalf("envelope threadId = %q, want UUID", thread)
	}
}

func feedAll(t *testing.T, s *liveStream, lines []string) ([][]byte, bool) {
	t.Helper()
	var frames [][]byte
	done := false
	for _, line := range lines {
		got, d, fatal := s.push([]byte(line))
		if fatal != nil {
			t.Fatalf("push(%q): %v", line, fatal)
		}
		frames = append(frames, got...)
		done = done || d
	}
	return frames, done
}

func TestRetryableUpstreamError(t *testing.T) {
	_, err := parseStream([]byte(
		`{"type":"error","error":{"isRetryable":true,"message":"Service temporarily unavailable.","statusCode":503,"type":"server_error"}}`))
	var statusErr *upstreamStatusError
	if !errors.As(err, &statusErr) || statusErr.status != 503 {
		t.Fatalf("err = %v, want status 503", err)
	}
	raw := upstreamErrorEnvelope(statusErr.status, []byte(statusErr.message))
	var env struct {
		Error struct {
			HTTPStatus int `json:"http_status"`
		} `json:"error"`
	}
	if jsonErr := json.Unmarshal(raw, &env); jsonErr != nil || env.Error.HTTPStatus != 503 {
		t.Fatalf("envelope = %s", raw)
	}
}

func TestLiveStreamIncremental(t *testing.T) {
	s := newLiveStream("m")
	frames, done := feedAll(t, s, []string{
		`{"type":"reasoning-delta","text":"hmm"}`,
		`{"type":"text-delta","text":"hi"}`,
		`{"type":"tool-call","toolName":"t","toolCallId":"c1","input":{"a":1}}`,
		`{"type":"finish","finishReason":"tool-calls","totalUsage":{"inputTokens":5,"outputTokens":3,"inputTokenDetails":{}}}`,
	})
	if !done {
		t.Fatal("finish event must terminate the stream")
	}
	if len(frames) != 4 { // reasoning + text + tool + final; host appends [DONE]
		t.Fatalf("got %d frames: %q", len(frames), frames)
	}
	joined := ""
	for _, f := range frames[:3] {
		if bytes.HasPrefix(f, []byte("data:")) {
			t.Fatalf("frame must be raw JSON: %q", f)
		}
		joined += string(f)
	}
	for _, want := range []string{"reasoning_content", `"content":"hi"`, `"index":0`, `"name":"t"`} {
		if !strings.Contains(joined, want) {
			t.Fatalf("frames missing %s: %q", want, joined)
		}
	}
	roles := strings.Count(joined, `"role":"assistant"`)
	if roles != 1 {
		t.Fatalf("role emitted %d times, want exactly once", roles)
	}
	if !strings.Contains(string(frames[3]), `"finish_reason":"tool_calls"`) {
		t.Fatalf("final frame = %q", frames[3])
	}
}

func TestLiveStreamTruncatedFinish(t *testing.T) {
	s := newLiveStream("m")
	if _, _, fatal := s.push([]byte(`{"type":"text-delta","text":"partial"}`)); fatal != nil {
		t.Fatal(fatal)
	}
	// A missing finish event is a truncated turn (official harness: 502
	// retryable), never a synthesized normal stop — even with partial
	// content there is no authoritative usage or stop reason.
	if _, err := s.finish(); err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("partial truncated err = %v, want truncation failure", err)
	}
	if _, err := newLiveStream("m").finish(); err == nil {
		t.Fatal("empty truncated stream must stay an error")
	}
}

func TestLiveStreamFatal(t *testing.T) {
	s := newLiveStream("m")
	if _, _, fatal := s.push([]byte(`{"type":"error","error":"nope"}`)); fatal == nil {
		t.Fatal("error event must be fatal")
	}
}

func TestUpstreamErrorEnvelope(t *testing.T) {
	raw := upstreamErrorEnvelope(429, []byte("slow down"))
	var env struct {
		OK    bool `json:"ok"`
		Error struct {
			Code       string `json:"code"`
			Message    string `json:"message"`
			HTTPStatus int    `json:"http_status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if env.OK || env.Error.HTTPStatus != 429 || !strings.Contains(env.Error.Code, "rate_limited") {
		t.Fatalf("envelope = %s", raw)
	}
}

func resetDynamicState(t *testing.T) {
	t.Helper()
	previous := defaultPlugin
	defaultPlugin = NewPlugin(nil, nil)
	t.Cleanup(func() { defaultPlugin.Shutdown(); defaultPlugin = previous })
}

func remoteFixture(extra string) string {
	return `{"schema_version":1,"source_cli_version":"9.9.9","models":[` +
		`{"id":"deepseek/deepseek-v4-flash","display":"Flash","context":1000000,"output":131072},` +
		`{"id":"test/remote-only","display":"Remote","context":262144,"output":65536},` +
		`{"id":"test/remote-small","display":"","context":0,"output":0}` +
		extra + `]}`
}

func TestRemoteModelsFetchAndCache(t *testing.T) {
	resetDynamicState(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(remoteFixture("")))
	}))
	setConfig(pluginConfig{ModelsURL: srv.URL})
	maybeRefreshModels()
	got := registeredModels()
	if len(got) != 3 {
		t.Fatalf("remote registry = %d models, want 3", len(got))
	}
	if _, ok := matchModel("test/remote-only"); !ok {
		t.Fatal("remote-only model not routed")
	}
	if c := modelOutputCap("test/remote-small"); c != 0 {
		t.Fatalf("unknown output cap = %d, want 0", c)
	}
	for _, m := range got {
		if m.ID == "cmdcode-go/remote-small" && m.DisplayName != "test/remote-small" {
			t.Fatalf("fallback display = %q, want id", m.DisplayName)
		}
	}
	// TTL must serve the snapshot without network: point at a dead URL.
	srv.Close()
	setConfig(pluginConfig{ModelsURL: "http://127.0.0.1:1/unreachable"})
	maybeRefreshModels()
	if len(registeredModels()) != 3 {
		t.Fatal("TTL cache not served after server went away")
	}
}

func TestRemoteModelsInvalidFallsBack(t *testing.T) {
	cases := map[string]string{
		"garbage":      `{bad json`,
		"wrong schema": `{"schema_version":999,"models":[]}`,
		"too few":      `{"schema_version":1,"models":[]}`,
	}
	for name, body := range cases {
		resetDynamicState(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(body))
		}))
		setConfig(pluginConfig{ModelsURL: srv.URL})
		maybeRefreshModels()
		srv.Close()
		if got, want := len(registeredModels()), len(modelTable); got != want {
			t.Errorf("%s: registry = %d, want compiled %d", name, got, want)
		}
	}
}

func TestDocumentedRosterCanRetireLegacyAnchor(t *testing.T) {
	raw := []byte(`{"schema_version":1,"source":"command-code Go plan page","models":[{"id":"test/new-go-model","context":200000,"output":32768}]}`)
	defs, _, err := parseModelsFile(raw)
	if err != nil || len(defs) != 1 {
		t.Fatalf("documented roster without legacy anchor: %v, %v", defs, err)
	}
}

func TestRemoteModelsFile(t *testing.T) {
	resetDynamicState(t)
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte(remoteFixture("")), 0o600); err != nil {
		t.Fatal(err)
	}
	setConfig(pluginConfig{ModelsFile: path})
	maybeRefreshModels()
	if len(registeredModels()) != 3 {
		t.Fatalf("file registry = %d models, want 3", len(registeredModels()))
	}
	// Same mtime content is not reloaded; bumped mtime is.
	if err := os.WriteFile(path, []byte(remoteFixture(
		`,{"id":"test/remote-extra","display":"Extra","context":1000,"output":1000}`,
	)), 0o600); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
	maybeRefreshModels()
	if len(registeredModels()) != 4 {
		t.Fatalf("reloaded registry = %d models, want 4", len(registeredModels()))
	}
	if _, ok := matchModel("test/remote-extra"); !ok {
		t.Fatal("reloaded model not routed")
	}
}

func TestRemoteModelsDisabled(t *testing.T) {
	resetDynamicState(t)
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte(remoteFixture("")), 0o600); err != nil {
		t.Fatal(err)
	}
	setConfig(pluginConfig{ModelsFile: path})
	maybeRefreshModels()
	if len(registeredModels()) != 3 {
		t.Fatalf("file registry = %d models, want 3", len(registeredModels()))
	}
	setConfig(pluginConfig{ModelsRefreshInterval: "0"})
	maybeRefreshModels()
	if got, want := len(registeredModels()), len(modelTable); got != want {
		t.Fatalf("disabled registry = %d, want compiled %d", got, want)
	}
}

func TestConfigureRosterKeys(t *testing.T) {
	prev := getConfig()
	defer setConfig(prev)
	setConfig(pluginConfig{})
	yaml := "models_url: https://example.com/m.json\nmodels_file: /tmp/m.json\nmodels_refresh_interval: 12h\n"
	payload := `{"config_yaml":"` + base64.StdEncoding.EncodeToString([]byte(yaml)) + `"}`
	raw, err := handleMethod("plugin.reconfigure", []byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	_ = raw
	cfg := getConfig()
	if cfg.ModelsURL != "https://example.com/m.json" || cfg.ModelsFile != "/tmp/m.json" ||
		cfg.ModelsRefreshInterval != "12h" {
		t.Fatalf("roster config = %+v", cfg)
	}
}

func TestWireForwardsReasoningContent(t *testing.T) {
	env := mustEnvelope(t, `{"model":"m","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"done","reasoning_content":"let me think"},
		{"role":"user","content":"continue"}]}`)
	messages := paramsOf(t, env)["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("messages = %v, want 3", messages)
	}
	assistant := messages[1].(map[string]any)
	parts := assistant["content"].([]any)
	if len(parts) == 0 || parts[0].(map[string]any)["type"] != "reasoning" {
		t.Fatalf("first part = %v, want reasoning", parts)
	}
	if parts[0].(map[string]any)["text"] != "let me think" {
		t.Fatalf("reasoning part = %v", parts[0])
	}
}

func TestWireForwardsReasoningVariants(t *testing.T) {
	env := mustEnvelope(t, `{"model":"m","messages":[
		{"role":"assistant","content":[{"type":"reasoning","text":"r1"},{"type":"text","text":"hi"}]},
		{"role":"assistant","content":null,"reasoning":{"text":"r2"}}]}`)
	messages := paramsOf(t, env)["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("messages = %v, want 2", messages)
	}
	for i, want := range []string{"r1", "r2"} {
		parts := messages[i].(map[string]any)["content"].([]any)
		if len(parts) == 0 || parts[0].(map[string]any)["type"] != "reasoning" {
			t.Fatalf("msg %d parts = %v, want reasoning first", i, parts)
		}
		if parts[0].(map[string]any)["text"] != want {
			t.Fatalf("msg %d reasoning = %v, want %q", i, parts[0], want)
		}
	}
}

func TestNonStreamResponseKeepsReasoning(t *testing.T) {
	acc := &accumulated{finish: "stop"}
	acc.reasoning.WriteString("thinking trace")
	acc.text.WriteString("final")
	var resp map[string]any
	if err := json.Unmarshal(acc.toOpenAIResponse("m"), &resp); err != nil {
		t.Fatal(err)
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["reasoning_content"] != "thinking trace" {
		t.Fatalf("message = %v, want reasoning_content preserved", msg)
	}
	if msg["content"] != "final" {
		t.Fatalf("message content = %v", msg["content"])
	}
}
