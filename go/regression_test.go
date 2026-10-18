package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/translator/builtin"
)

// These fixtures intentionally exceed float64 precision/range. Comparing their
// original JSON lexemes catches corruption that numeric equality would hide.
var regressionNumbers = []string{
	"9007199254740993",
	"1e1000",
	"1e-1000",
	"0.123456789012345678901234567890123456789",
}

type regressionNoNetwork struct{}

func (regressionNoNetwork) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("regression tests must not access the network")
}

func regressionPlugin(t *testing.T) *Plugin {
	t.Helper()
	p := NewPlugin(&http.Client{Transport: regressionNoNetwork{}}, nil)
	t.Cleanup(p.Shutdown)
	if err := p.Configure([]byte(`{"config_yaml":"api_key: user_offline_fixture\nmodels_refresh_interval: '0'"}`)); err != nil {
		t.Fatal(err)
	}
	p.Models = NewModelRegistry([]modelDef{
		{id: "test/reasoner", efforts: []string{"low", "high", "max"}},
		{id: "test/plain"},
	}, &http.Client{Transport: regressionNoNetwork{}})
	return p
}

func regressionJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func regressionBadRequest(t *testing.T, p *Plugin, request pluginapi.ExecutorRequest) {
	t.Helper()
	assertFailure := func(body []byte, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("expected a host-native HTTP 400, got transport error: %v", err)
		}
		var envelope pluginabi.Envelope
		if err := json.Unmarshal(body, &envelope); err != nil {
			t.Fatalf("invalid failure envelope: %v %s", err, body)
		}
		if envelope.OK || envelope.Error == nil || envelope.Error.HTTPStatus != http.StatusBadRequest {
			t.Fatalf("expected unsuccessful HTTP 400 envelope, got %s", body)
		}
	}
	prep, failure, err := p.prepareTurn(request)
	assertFailure(failure, err)
	if len(prep.envelope) != 0 || prep.key != "" {
		t.Fatalf("rejected request retained executable preparation: %+v", prep)
	}
	// Both executor modes must return the same request error before trying the
	// fake transport, whose error would fail assertFailure rather than pass.
	for _, method := range []string{pluginabi.MethodExecutorExecute, pluginabi.MethodExecutorExecuteStream} {
		body, err := p.Handle(method, regressionJSON(t, request))
		assertFailure(body, err)
	}
}

func regressionToolArguments(t *testing.T, body []byte, streaming bool) []string {
	t.Helper()
	var response struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					Function struct{ Arguments string }
				} `json:"tool_calls"`
			}
			Delta struct {
				ToolCalls []struct {
					Function struct{ Arguments string }
				} `json:"tool_calls"`
			}
		}
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("invalid OpenAI output: %v: %s", err, body)
	}
	var arguments []string
	for _, choice := range response.Choices {
		if streaming {
			for _, call := range choice.Delta.ToolCalls {
				arguments = append(arguments, call.Function.Arguments)
			}
		} else {
			for _, call := range choice.Message.ToolCalls {
				arguments = append(arguments, call.Function.Arguments)
			}
		}
	}
	return arguments
}

func TestRegressionToolNumbersSurviveLiveAndBufferedResponses(t *testing.T) {
	for _, number := range regressionNumbers {
		for _, field := range []string{"input", "args"} {
			for _, streaming := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%v", number, field, streaming), func(t *testing.T) {
					want := `{"n":` + number + `}`
					body := `{"type":"tool-call","toolName":"lookup","toolCallId":"c1","` + field + `": { "n": ` + number + ` }}` + "\n" + `{"type":"finish","finishReason":"tool-calls"}` + "\n"
					d := NewEventDecoder("test/reasoner", streaming)
					var arguments []string
					if err := d.Consume(strings.NewReader(body), func(frame []byte) error {
						arguments = append(arguments, regressionToolArguments(t, frame, true)...)
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					if !streaming {
						arguments = regressionToolArguments(t, d.acc.toOpenAIResponse("test/reasoner"), false)
						// The legacy buffered-stream fallback is a third response path.
						var buffered []string
						for _, frame := range d.acc.toOpenAIChunks("test/reasoner") {
							buffered = append(buffered, regressionToolArguments(t, frame, true)...)
						}
						if len(buffered) != 1 || buffered[0] != want {
							t.Errorf("buffered chunks changed tool argument: got %q, want %q", buffered, want)
						}
					}
					if len(arguments) != 1 || arguments[0] != want {
						t.Fatalf("changed tool argument: got %q, want %q", arguments, want)
					}
				})
			}
		}
	}
}

func regressionHistoryPayload(t *testing.T, arguments string) []byte {
	t.Helper()
	return regressionJSON(t, map[string]any{"messages": []any{
		map[string]any{"role": "assistant", "tool_calls": []any{
			map[string]any{"id": "c1", "type": "function", "function": map[string]string{"name": "lookup", "arguments": arguments}},
		}},
	}})
}

func TestRegressionHistoricalToolNumbersRemainExact(t *testing.T) {
	p := regressionPlugin(t)
	for _, number := range regressionNumbers {
		t.Run(number, func(t *testing.T) {
			raw, err := buildEnvelopeWithSnapshot("test/reasoner", regressionHistoryPayload(t, `{ "n": `+number+` }`), p.snapshot())
			if err != nil {
				t.Fatal(err)
			}
			var envelope struct {
				Params struct {
					Messages []struct {
						Content []struct{ Input json.RawMessage }
					}
				}
			}
			if err := json.Unmarshal(raw, &envelope); err != nil {
				t.Fatal(err)
			}
			if len(envelope.Params.Messages) != 1 || len(envelope.Params.Messages[0].Content) != 1 {
				t.Fatalf("missing tool history: %s", raw)
			}
			got := envelope.Params.Messages[0].Content[0].Input
			if want := `{"n":` + number + `}`; string(got) != want {
				t.Fatalf("history changed: got %s, want %s", got, want)
			}
		})
	}
}

func TestRegressionMalformedHistoricalArgumentsFail(t *testing.T) {
	p := regressionPlugin(t)
	for _, args := range []string{`{"n":}`, `{"n":1} {"extra":2}`, `not JSON`, `{"n":NaN}`, `{"n":1e}`} {
		t.Run(args, func(t *testing.T) {
			payload := regressionHistoryPayload(t, args)
			if raw, err := buildEnvelopeWithSnapshot("test/reasoner", payload, p.snapshot()); err == nil {
				t.Fatalf("malformed tool history silently accepted: %s", raw)
			}
			regressionBadRequest(t, p, pluginapi.ExecutorRequest{Model: "cmdcode-go/reasoner", Payload: payload})
		})
	}
}

func TestRegressionInvalidSchemasReturnBadRequest(t *testing.T) {
	p := regressionPlugin(t)
	for _, schema := range []string{`[]`, `42`, `"not an object schema"`, `{"type":"array"}`, `{"type":null}`} {
		t.Run(schema, func(t *testing.T) {
			payload := regressionJSON(t, map[string]any{
				"messages": []any{map[string]string{"role": "user", "content": "hello"}},
				"tools":    []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "parameters": json.RawMessage(schema)}}},
			})
			regressionBadRequest(t, p, pluginapi.ExecutorRequest{Model: "cmdcode-go/reasoner", Payload: payload})
		})
	}
}

func TestRegressionSchemaNumbersRemainExactWhenAddingType(t *testing.T) {
	p := regressionPlugin(t)
	for _, number := range regressionNumbers {
		for _, doubleEncoded := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/string=%v", number, doubleEncoded), func(t *testing.T) {
				schema := []byte(`{"properties":{"n":{"type":"number","default":` + number + `,"minimum":` + number + `}},"additionalProperties":false}`)
				if doubleEncoded {
					schema = regressionJSON(t, string(schema))
				}
				payload := regressionJSON(t, map[string]any{
					"messages": []any{map[string]string{"role": "user", "content": "hello"}},
					"tools":    []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "parameters": json.RawMessage(schema)}}},
				})
				raw, err := buildEnvelopeWithSnapshot("test/reasoner", payload, p.snapshot())
				if err != nil {
					t.Fatal(err)
				}
				var env struct {
					Params struct {
						Tools []struct {
							Schema struct {
								Type                 string
								Properties           map[string]struct{ Default, Minimum json.RawMessage }
								AdditionalProperties *bool
							} `json:"input_schema"`
						}
					}
				}
				if err := json.Unmarshal(raw, &env); err != nil {
					t.Fatal(err)
				}
				if len(env.Params.Tools) != 1 {
					t.Fatalf("missing tool: %s", raw)
				}
				got := env.Params.Tools[0].Schema
				if got.Type != "object" || string(got.Properties["n"].Default) != number || string(got.Properties["n"].Minimum) != number || got.AdditionalProperties == nil || *got.AdditionalProperties {
					t.Fatalf("schema normalization changed semantics: %s", raw)
				}
			})
		}
	}
}

func TestRegressionThinkingSuffixAndBodyPrecedence(t *testing.T) {
	p := regressionPlugin(t)
	tests := []struct{ name, model, bodyEffort, wantEffort string }{
		{"namespace", "cmdcode-go/reasoner(high)", "", "high"},
		{"trimmed", " cmdcode-go/reasoner(high) ", "", "high"},
		{"case-insensitive", "CMDCODE-GO/REASONER(HIGH)", "", "high"},
		// CPA v7.2.149 internal/thinking/apply.go gives the suffix priority.
		{"suffix-over-body", "cmdcode-go/reasoner(high)", "low", "high"},
		{"suffix-over-invalid-body", "cmdcode-go/reasoner(high)", "unsupported", "high"},
		{"body-only", "cmdcode-go/reasoner", "max", "max"},
		{"no-effort", "cmdcode-go/reasoner", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			payload := map[string]any{"messages": []any{map[string]string{"role": "user", "content": "hello"}}}
			if tc.bodyEffort != "" {
				payload["reasoning_effort"] = tc.bodyEffort
			}
			prep, failure, err := p.prepareTurn(pluginapi.ExecutorRequest{Model: tc.model, Payload: regressionJSON(t, payload)})
			if err != nil || len(failure) != 0 {
				t.Fatalf("prepare failed: %v %s", err, failure)
			}
			var env generateRequest
			if err := json.Unmarshal(prep.envelope, &env); err != nil {
				t.Fatal(err)
			}
			if env.Params.Model != "test/reasoner" {
				t.Errorf("namespace/suffix leaked upstream: %q", env.Params.Model)
			}
			gotEffort := ""
			if env.Params.ReasoningEffort != nil {
				gotEffort = *env.Params.ReasoningEffort
			}
			if gotEffort != tc.wantEffort {
				t.Errorf("effort = %q, want %q", gotEffort, tc.wantEffort)
			}
		})
	}
}

func TestRegressionUnsupportedEffortsAndUnknownModelsFail(t *testing.T) {
	p := regressionPlugin(t)
	tests := []struct{ model, effort string }{
		{"cmdcode-go/reasoner(ultra)", ""},
		{"cmdcode-go/reasoner(8192)", ""},
		{"cmdcode-go/reasoner()", ""},
		{"cmdcode-go/reasoner", "ultra"},
		{"cmdcode-go/test/unknown(high)", ""},
		{"test/unknown", ""},
		{"", ""},
	}
	for _, tc := range tests {
		t.Run(tc.model+"/"+tc.effort, func(t *testing.T) {
			payload := map[string]any{"messages": []any{map[string]string{"role": "user", "content": "hello"}}}
			if tc.effort != "" {
				payload["reasoning_effort"] = tc.effort
			}
			regressionBadRequest(t, p, pluginapi.ExecutorRequest{Model: tc.model, Payload: regressionJSON(t, payload)})
		})
	}
}

func TestRegressionDefaultEffortsAreExplicitlySupported(t *testing.T) {
	p := regressionPlugin(t)
	for _, level := range []string{"auto", "default", "-1", ""} {
		t.Run("body="+level, func(t *testing.T) {
			// Include the empty string explicitly, unlike an absent property.
			payload := regressionJSON(t, map[string]any{"messages": []any{}, "reasoning_effort": level})
			prep, failure, err := p.prepareTurn(pluginapi.ExecutorRequest{Model: "cmdcode-go/reasoner", Payload: payload})
			if err != nil || len(failure) != 0 {
				t.Fatalf("default effort rejected: %v %s", err, failure)
			}
			var env generateRequest
			if err := json.Unmarshal(prep.envelope, &env); err != nil || env.Params.ReasoningEffort != nil {
				t.Fatalf("default effort must be omitted upstream: %v %s", err, prep.envelope)
			}
		})
		if level == "" {
			continue // Empty suffix is a malformed instruction, tested above.
		}
		t.Run("suffix="+level, func(t *testing.T) {
			prep, failure, err := p.prepareTurn(pluginapi.ExecutorRequest{
				Model:   "cmdcode-go/reasoner(" + level + ")",
				Payload: []byte(`{"messages":[],"reasoning_effort":"high"}`),
			})
			if err != nil || len(failure) != 0 {
				t.Fatalf("default suffix rejected: %v %s", err, failure)
			}
			var env generateRequest
			if err := json.Unmarshal(prep.envelope, &env); err != nil || env.Params.ReasoningEffort != nil || env.Params.Model != "test/reasoner" {
				t.Fatalf("default suffix did not override body effort: %v %s", err, prep.envelope)
			}
		})
	}
}

func TestRegressionAdvertisedThinkingLevelsAreRepresentable(t *testing.T) {
	snapshot := newModelSnapshot([]modelDef{
		{id: "test/mixed", efforts: []string{"off", "low", "none", "high", "minimal", "ultra"}},
		{id: "test/unsupported-only", efforts: []string{"off", "none", "ultra"}},
	})
	models := snapshot.Registered(pluginConfig{})
	if len(models) != 2 || models[0].Thinking == nil || strings.Join(models[0].Thinking.Levels, ",") != "low,high" {
		t.Fatalf("advertised unsupported/missing thinking levels: %+v", models)
	}
	if models[1].Thinking != nil {
		t.Fatalf("advertised unusable thinking capability: %+v", models[1].Thinking)
	}
	for _, parameter := range models[1].SupportedParameters {
		if parameter == "reasoning_effort" {
			t.Fatal("advertised reasoning_effort without a representable level")
		}
	}
	for _, model := range newModelSnapshot(modelTable).Registered(pluginConfig{}) {
		if model.Thinking == nil {
			continue
		}
		for _, level := range model.Thinking.Levels {
			switch level {
			case "low", "medium", "high", "xhigh", "max":
			default:
				t.Errorf("catalog model %s advertised unsupported level %q", model.ID, level)
			}
		}
	}
}

func TestRegressionSuffixedModelsCannotBypassFilters(t *testing.T) {
	for _, config := range []string{
		"disable_models: [cmdcode-go/reasoner]",
		"disable_models: ['cmdcode-go/reasoner(high)']",
		"models: [cmdcode-go/plain]",
	} {
		t.Run(config, func(t *testing.T) {
			p := regressionPlugin(t)
			if err := p.Configure(regressionJSON(t, map[string]string{"config_yaml": config})); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"cmdcode-go/reasoner", "cmdcode-go/reasoner(high)", "CMDCODE-GO/REASONER(HIGH)"} {
				route := p.routeModel(regressionJSON(t, pluginapi.ModelRouteRequest{RequestedModel: name}))
				if route.Handled {
					t.Errorf("disabled model routed: %s", name)
				}
				regressionBadRequest(t, p, pluginapi.ExecutorRequest{Model: name, Payload: []byte(`{"messages":[]}`)})
			}
		})
	}
	p := regressionPlugin(t)
	if err := p.Configure([]byte(`{"config_yaml":"models: ['cmdcode-go/reasoner(high)']"}`)); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cmdcode-go/reasoner", "cmdcode-go/reasoner(high)", "CMDCODE-GO/REASONER(HIGH)"} {
		if route := p.routeModel(regressionJSON(t, pluginapi.ModelRouteRequest{RequestedModel: name})); !route.Handled {
			t.Errorf("allowed suffixed model not routed: %s", name)
		}
	}
}

func TestRegressionCountTokensUsesSDKPayload(t *testing.T) {
	p := regressionPlugin(t)
	translated := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	original := []byte(`{"model":"test/reasoner","messages":[{"role":"user","content":"original payload to count"}]}`)
	for _, originalRequest := range [][]byte{nil, original} {
		request := pluginapi.ExecutorRequest{Model: "cmdcode-go/reasoner", Payload: translated, OriginalRequest: originalRequest, SourceFormat: "chat-completions", Format: "chat-completions"}
		raw, err := p.Handle(pluginabi.MethodExecutorCountTokens, regressionJSON(t, request))
		if err != nil {
			t.Fatal(err)
		}
		var env struct {
			OK     bool
			Result pluginapi.ExecutorResponse
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatal(err)
		}
		if !env.OK || len(env.Result.Payload) == 0 || !json.Valid(env.Result.Payload) {
			t.Fatalf("host cannot decode a count payload: %s", raw)
		}
		if env.Result.Headers.Get("Content-Type") != "application/json" {
			t.Errorf("count payload lacks JSON content type: %#v", env.Result.Headers)
		}
		if env.Result.Headers.Get("X-Cmdcode-Token-Count") != "estimate" || env.Result.Metadata["token_count_estimated"] != true {
			t.Errorf("heuristic count is not disclosed as an estimate: headers=%#v metadata=%#v", env.Result.Headers, env.Result.Metadata)
		}
		var count struct {
			InputTokens *int `json:"input_tokens"`
		}
		if err := json.Unmarshal(env.Result.Payload, &count); err != nil {
			t.Fatal(err)
		}
		counted := translated
		if len(originalRequest) > 0 {
			counted = originalRequest
		}
		if want := len(counted)/4 + 8; count.InputTokens == nil || *count.InputTokens != want {
			t.Errorf("count does not expose the estimated input_tokens=%d: %s", want, env.Result.Payload)
		}
	}
}

// The v7.2.149 host runs CountTokens through the ordinary nonstream translator
// (internal/pluginhost/adapters_executors.go:952-977), not TranslateTokenCount.
// It also replaces SourceFormat with the selected executor input format before
// building ExecutorRequest. Exercise those exact SDK entry points here.
type regressionTranslatorHooks struct {
	t *testing.T
	p *Plugin
}

func (h regressionTranslatorHooks) NormalizeRequest(_ context.Context, _, _ sdktranslator.Format, _ string, body []byte, _ bool) []byte {
	return body
}
func (h regressionTranslatorHooks) TranslateRequest(context.Context, sdktranslator.Format, sdktranslator.Format, string, []byte, bool) ([]byte, bool) {
	return nil, false
}
func (h regressionTranslatorHooks) TranslateResponse(context.Context, sdktranslator.Format, sdktranslator.Format, string, []byte, []byte, []byte, bool) ([]byte, bool) {
	return nil, false
}
func (h regressionTranslatorHooks) normalize(method string, from, to sdktranslator.Format, model string, original, request, body []byte, stream bool) []byte {
	h.t.Helper()
	raw, err := h.p.Handle(method, regressionJSON(h.t, pluginapi.ResponseTransformRequest{
		FromFormat: from.String(), ToFormat: to.String(), Model: model,
		OriginalRequest: original, TranslatedRequest: request, Body: body, Stream: stream,
	}))
	if err != nil {
		h.t.Fatal(err)
	}
	var env struct {
		OK     bool
		Result pluginapi.PayloadResponse
	}
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		h.t.Fatalf("normalizer failed: %v %s", err, raw)
	}
	return env.Result.Body
}
func (h regressionTranslatorHooks) NormalizeResponseBefore(_ context.Context, from, to sdktranslator.Format, model string, original, request, body []byte, stream bool) []byte {
	return h.normalize(pluginabi.MethodResponseNormalizeBefore, from, to, model, original, request, body, stream)
}
func (h regressionTranslatorHooks) NormalizeResponseAfter(_ context.Context, from, to sdktranslator.Format, model string, original, request, body []byte, stream bool) []byte {
	return h.normalize(pluginabi.MethodResponseNormalizeAfter, from, to, model, original, request, body, stream)
}

func TestRegressionClaudeCountThroughPinnedHostTranslation(t *testing.T) {
	p := regressionPlugin(t)
	var registrationData struct {
		Capabilities struct {
			Before bool `json:"response_before_translator"`
			After  bool `json:"response_after_translator"`
		}
	}
	if err := json.Unmarshal(regressionJSON(t, registration()), &registrationData); err != nil {
		t.Fatal(err)
	}
	if !registrationData.Capabilities.Before || !registrationData.Capabilities.After {
		t.Fatal("host registration must enable the actual before/after normalizers")
	}
	registry := builtin.Registry()
	registry.SetPluginHooks(regressionTranslatorHooks{t: t, p: p})
	t.Cleanup(func() { registry.SetPluginHooks(nil) })
	const model = "cmdcode-go/reasoner"
	original := []byte(`{"model":"cmdcode-go/reasoner","messages":[{"role":"user","content":"count this Claude request"}]}`)
	translated := registry.TranslateRequest(sdktranslator.FormatClaude, sdktranslator.FormatOpenAI, model, original, false)
	// Both formats are OpenAI after host prepareExecutorCall, despite the
	// original caller being Claude. Testing SourceFormat=claude masks this bug.
	req := pluginapi.ExecutorRequest{
		Model: model, Format: sdktranslator.FormatOpenAI.String(), SourceFormat: sdktranslator.FormatOpenAI.String(),
		Payload: translated, OriginalRequest: original,
	}
	raw, err := p.Handle(pluginabi.MethodExecutorCountTokens, regressionJSON(t, req))
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		OK     bool
		Result pluginapi.ExecutorResponse
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || !envelope.OK || len(envelope.Result.Payload) == 0 {
		t.Fatalf("invalid SDK count result: %v %s", err, raw)
	}
	clientBody := registry.TranslateNonStream(context.Background(), sdktranslator.FormatOpenAI, sdktranslator.FormatClaude, model, original, translated, envelope.Result.Payload, nil)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(clientBody, &fields); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprint(len(original)/4 + 8)
	if len(fields) != 1 || string(fields["input_tokens"]) != want {
		t.Fatalf("Claude count endpoint must return only input_tokens=%s, got %s", want, clientBody)
	}

	// A regular completion still needs the full Claude message response.
	normal := []byte(`{"id":"ordinary-response","model":"test/reasoner","choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":2}}`)
	clientBody = registry.TranslateNonStream(context.Background(), sdktranslator.FormatOpenAI, sdktranslator.FormatClaude, model, original, translated, normal, nil)
	var message struct {
		Type  string
		Usage struct {
			InputTokens int `json:"input_tokens"`
		}
	}
	if err := json.Unmarshal(clientBody, &message); err != nil || message.Type != "message" || message.Usage.InputTokens != 7 {
		t.Fatalf("count adaptation corrupted an ordinary completion: %v %s", err, clientBody)
	}
}

func TestRegressionCountNormalizerIsNarrow(t *testing.T) {
	p := regressionPlugin(t)
	hooks := regressionTranslatorHooks{t: t, p: p}
	marked := []byte(`{"id":"cmdcode-go-count-tokens-v1","type":"message","usage":{"input_tokens":11}}`)
	tests := []struct {
		name     string
		from, to sdktranslator.Format
		stream   bool
		body     []byte
	}{
		{"stream", sdktranslator.FormatOpenAI, sdktranslator.FormatClaude, true, marked},
		{"wrong-source", sdktranslator.FormatGemini, sdktranslator.FormatClaude, false, marked},
		{"wrong-target", sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAI, false, marked},
		{"unmarked-message", sdktranslator.FormatOpenAI, sdktranslator.FormatClaude, false, []byte(`{"id":"ordinary","type":"message","usage":{"input_tokens":11}}`)},
		{"missing-message-type", sdktranslator.FormatOpenAI, sdktranslator.FormatClaude, false, []byte(`{"id":"cmdcode-go-count-tokens-v1","usage":{"input_tokens":11}}`)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := hooks.NormalizeResponseAfter(context.Background(), tc.from, tc.to, "test/reasoner", nil, nil, tc.body, tc.stream)
			if !bytes.Equal(got, tc.body) {
				t.Fatalf("unrelated response changed: got %s, want %s", got, tc.body)
			}
		})
	}
}

func regressionWait(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestRegressionRuntimeReloadResetsStateAndRejectsDoubleInit(t *testing.T) {
	var runtime pluginRuntime
	var installs, clears int
	for cycle := 0; cycle < 3; cycle++ {
		if !runtime.Initialize(func() { installs++ }) {
			t.Fatal("fresh initialization rejected")
		}
		first := runtime.active
		if cfg := first.Config.Snapshot(); cfg.APIKey != "" || len(cfg.DisableModels) != 0 {
			t.Fatalf("previous configuration leaked into new instance: %#v", cfg)
		}
		if err := first.Configure([]byte(`{"config_yaml":"api_key: user_offline_fixture\nmodels_refresh_interval: '0'\ndisable_models: [deepseek/deepseek-v4-flash]"}`)); err != nil {
			t.Fatal(err)
		}
		if runtime.Initialize(func() { t.Error("double init overwrote host callbacks") }) || runtime.active != first {
			t.Fatal("active instance replaced by double initialization")
		}
		if _, err := runtime.Handle(pluginabi.MethodExecutorIdentifier, nil); err != nil {
			t.Fatal(err)
		}
		runtime.Shutdown(func() { clears++ })
		runtime.Shutdown(func() { t.Error("duplicate shutdown cleared host twice") })
		if runtime.active != nil {
			t.Fatal("shutdown retained active instance")
		}
		if _, err := runtime.Handle(pluginabi.MethodExecutorIdentifier, nil); err == nil {
			t.Fatal("call accepted after shutdown")
		}
	}
	if installs != 3 || clears != 3 {
		t.Fatalf("installs=%d clears=%d", installs, clears)
	}
}

func TestRegressionRuntimeDrainsOldCallbacksBeforeReinit(t *testing.T) {
	var runtime pluginRuntime
	var generation atomic.Int32
	if !runtime.Initialize(func() { generation.Store(1) }) {
		t.Fatal("initialization failed")
	}
	p := runtime.active
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(func() { unblock(); runtime.Shutdown(nil) })
	entered := make(chan struct{})
	callbackDone := make(chan struct{})
	p.host.emit = func(string, []byte) error {
		close(entered)
		<-release
		if generation.Load() != 1 {
			t.Error("old callback observed replaced/cleared host")
		}
		return nil
	}
	ctx, done, err := p.begin(nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		defer done()
		_ = p.host.emit("old-stream", nil)
		close(callbackDone)
	}()
	regressionWait(t, entered, "old callback")
	shutdownDone := make(chan struct{})
	go func() {
		runtime.Shutdown(func() {
			select {
			case <-callbackDone:
			default:
				t.Error("host cleared before callback completed")
			}
			generation.Store(0)
		})
		close(shutdownDone)
	}()
	regressionWait(t, ctx.Done(), "shutdown cancellation")
	initDone := make(chan struct{})
	var reinitialized bool
	go func() {
		reinitialized = runtime.Initialize(func() { generation.Store(2) })
		close(initDone)
	}()
	select {
	case <-shutdownDone:
		t.Fatal("shutdown did not drain blocked callback")
	case <-initDone:
		t.Fatal("reinitialization crossed old callback lifetime")
	case <-time.After(20 * time.Millisecond):
	}
	unblock()
	regressionWait(t, shutdownDone, "shutdown drain")
	regressionWait(t, initDone, "reinitialization")
	if !reinitialized || generation.Load() != 2 {
		t.Fatal("reinitialization failed after drain")
	}
	if _, err := runtime.Handle(pluginabi.MethodExecutorIdentifier, nil); err != nil {
		t.Fatal(err)
	}
}

func TestRegressionRuntimeConcurrentCallsAndLifecycle(t *testing.T) {
	var runtime pluginRuntime
	if !runtime.Initialize(nil) {
		t.Fatal("initialization failed")
	}
	defer runtime.Shutdown(nil)
	start := make(chan struct{})
	var calls sync.WaitGroup
	for i := 0; i < 16; i++ {
		calls.Add(1)
		go func() {
			defer calls.Done()
			<-start
			for j := 0; j < 100; j++ {
				body, err := runtime.Handle(pluginabi.MethodExecutorIdentifier, nil)
				if err == nil && !bytes.Contains(body, []byte(`"identifier":"cmdcode-go"`)) {
					t.Errorf("unexpected identifier response: %s", body)
				}
			}
		}()
	}
	close(start)
	for cycle := 0; cycle < 30; cycle++ {
		runtime.Shutdown(nil)
		if !runtime.Initialize(nil) {
			t.Fatal("serialized reinitialization rejected")
		}
	}
	calls.Wait()
}

func TestRegressionToolResultNumbersRemainExact(t *testing.T) {
	p := regressionPlugin(t)
	for _, number := range regressionNumbers {
		t.Run(number, func(t *testing.T) {
			want := `{"n":` + number + `}`
			payload := []byte(`{"messages":[{"role":"tool","tool_call_id":"c1","name":"lookup","content":` + want + `}]}`)
			raw, err := buildEnvelopeWithSnapshot("test/reasoner", payload, p.snapshot())
			if err != nil {
				t.Fatal(err)
			}
			var envelope struct {
				Params struct {
					Messages []struct {
						Content []struct{ Output struct{ Value string } }
					}
				}
			}
			if err := json.Unmarshal(raw, &envelope); err != nil {
				t.Fatal(err)
			}
			messages := envelope.Params.Messages
			if len(messages) != 1 || len(messages[0].Content) != 1 || messages[0].Content[0].Output.Value != want {
				t.Fatalf("tool result number changed: %s", raw)
			}
		})
	}
}
