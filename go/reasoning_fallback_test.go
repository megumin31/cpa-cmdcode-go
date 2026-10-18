package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/translator/builtin"
)

// Exercise both executor modes through the real request preparation and gateway
// serialization. All traffic terminates at this local fake gateway.
func TestReasoningFallbackExecutors(t *testing.T) {
	for _, model := range []string{"xiaomi/mimo-v2.6-pro", "test/reasoner"} {
		for _, effort := range []string{"none", "off", "minimal", "auto", "default", "low", "medium", "high", "xhigh", "max"} {
			for _, suffix := range []bool{false, true} {
				for _, method := range []string{pluginabi.MethodExecutorExecute, pluginabi.MethodExecutorExecuteStream} {
					t.Run(fmt.Sprintf("%s/%s/suffix=%v/%s", model, effort, suffix, method), func(t *testing.T) {
						calls := 0
						var got generateRequest
						server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							calls++
							if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
								t.Error(err)
							}
							fmt.Fprintln(w, `{"type":"finish","finishReason":"stop"}`)
						}))
						defer server.Close()
						p := NewPlugin(nil, nil)
						defer p.Shutdown()
						if err := p.Configure(configRequest("api_key: user_offline_fixture\nmodels_refresh_interval: '0'\nbase_url: " + server.URL + "\n")); err != nil {
							t.Fatal(err)
						}
						p.Models = NewModelRegistry([]modelDef{{id: "xiaomi/mimo-v2.6-pro"}, {id: "test/reasoner", efforts: []string{"low", "high", "max"}}}, &http.Client{Transport: regressionNoNetwork{}})
						selected := ProviderKey + "/" + shortModelName(model)
						payload := map[string]any{"messages": []any{}, "reasoning_effort": effort}
						if suffix {
							selected += "(" + effort + ")"
							payload["reasoning_effort"] = "high"
						}
						route := p.routeModel(regressionJSON(t, pluginapi.ModelRouteRequest{RequestedModel: selected}))
						if !route.Handled {
							t.Fatal("fallback rejected at routing")
						}
						raw, err := p.Handle(method, regressionJSON(t, pluginapi.ExecutorRequest{Model: selected, Payload: regressionJSON(t, payload)}))
						if err != nil {
							t.Fatal(err)
						}
						var result pluginabi.Envelope
						if err = json.Unmarshal(raw, &result); err != nil || !result.OK {
							t.Fatalf("execution failed: %v %s", err, raw)
						}
						if calls != 1 {
							t.Fatalf("gateway calls=%d; fallback must not retry", calls)
						}
						want := ""
						if gatewayEffort(effort) && (model == "xiaomi/mimo-v2.6-pro" || effort == "low" || effort == "high" || effort == "max") {
							want = effort
						}
						actual := ""
						if got.Params.ReasoningEffort != nil {
							actual = *got.Params.ReasoningEffort
						}
						if actual != want {
							t.Fatalf("effort=%q want=%q", actual, want)
						}
						if got.Params.Model != model {
							t.Fatalf("unstripped model=%q", got.Params.Model)
						}
					})
				}
			}
		}
	}
}

func TestReasoningMissingAndMalformed(t *testing.T) {
	p := regressionPlugin(t)
	for _, payload := range []string{`{"messages":[]}`, `{"messages":[],"reasoning_effort":null}`} {
		raw, err := buildEnvelopeWithSnapshot("test/reasoner", []byte(payload), p.snapshot())
		if err != nil {
			t.Fatal(err)
		}
		var got generateRequest
		if err = json.Unmarshal(raw, &got); err != nil || got.Params.ReasoningEffort != nil {
			t.Fatalf("missing/null must remain omitted: %v %s", err, raw)
		}
	}
	for _, value := range []string{`123`, `true`, `[]`, `{}`, `"meduim"`} {
		t.Run(value, func(t *testing.T) {
			regressionBadRequest(t, p, pluginapi.ExecutorRequest{Model: "cmdcode-go/reasoner", Payload: []byte(`{"messages":[],"reasoning_effort":` + value + `}`)})
		})
	}
}

func TestReasoningResponsesTranslationFallback(t *testing.T) {
	p := regressionPlugin(t)
	p.Models = NewModelRegistry([]modelDef{{id: "xiaomi/mimo-v2.6-pro"}}, &http.Client{Transport: regressionNoNetwork{}})
	const model = "cmdcode-go/mimo-v2.6-pro"
	for _, effort := range []string{"none", "low", "medium", "high", "xhigh", "max"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", effort, stream), func(t *testing.T) {
				original := regressionJSON(t, map[string]any{"model": model, "input": "hello", "reasoning": map[string]any{"effort": effort}, "stream": stream})
				payload := builtin.Registry().TranslateRequest(sdktranslator.FormatOpenAIResponse, sdktranslator.FormatOpenAI, model, original, stream)
				prep, failure, err := p.prepareTurn(pluginapi.ExecutorRequest{Model: model, Payload: payload})
				if err != nil || len(failure) > 0 {
					t.Fatalf("Responses rejected: %v %s", err, failure)
				}
				var got generateRequest
				if err = json.Unmarshal(prep.envelope, &got); err != nil {
					t.Fatal(err)
				}
				actual := ""
				if got.Params.ReasoningEffort != nil {
					actual = *got.Params.ReasoningEffort
				}
				want := effort
				if effort == "none" {
					want = ""
				}
				if actual != want {
					t.Fatalf("effort=%q want=%q translated=%s", actual, want, payload)
				}
			})
		}
	}
}
