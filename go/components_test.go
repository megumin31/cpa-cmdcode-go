package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func configRequest(text string) []byte {
	b, _ := json.Marshal(map[string]string{"config_yaml": text})
	return b
}

func TestConfigStoreYAMLAndAtomicity(t *testing.T) {
	s := &ConfigStore{}
	cfg, err := s.Update(configRequest("models: [foo, bar]\napi_key: 'hello # literal' # comment\nbase_url: https://example.com\n"))
	if err != nil || len(cfg.Models) != 2 || cfg.Models[0] != "foo" || cfg.APIKey != "hello # literal" {
		t.Fatalf("config: %+v %v", cfg, err)
	}
	cfg.Models[0] = "mutated"
	if s.Snapshot().Models[0] != "foo" {
		t.Fatal("snapshot aliases config")
	}
	for _, bad := range []string{"models: [foo\n", "base_url: file:///tmp/key", "models_refresh_interval: nonsense", "api_key: [secret]", "models: [true]", "models: []\nmodels: []", "---\napi_key: x\n---\napi_key: y"} {
		if _, err := s.Update(configRequest(bad)); err == nil {
			t.Fatalf("accepted invalid config: %s", bad)
		}
		if s.Snapshot().APIKey != "hello # literal" {
			t.Fatal("failed config partially applied")
		}
	}
	cfg, err = s.Update(configRequest("models: null\n"))
	if err != nil || len(cfg.Models) != 0 || cfg.APIKey != "hello # literal" {
		t.Fatal("clear/preserve semantics", cfg, err)
	}
}

func TestRegistryInitialFailureCooldown(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(503) }))
	defer srv.Close()
	r := NewModelRegistry(modelTable, nil)
	now := time.Now()
	r.now = func() time.Time { return now }
	r.Configure(pluginConfig{ModelsURL: srv.URL})
	_ = r.Refresh(context.Background())
	_ = r.Refresh(context.Background())
	if requests.Load() != 1 {
		t.Fatalf("requests=%d", requests.Load())
	}
	now = now.Add(modelsMinRetryInterval + time.Second)
	_ = r.Refresh(context.Background())
	if requests.Load() != 2 {
		t.Fatal("cooldown never expires")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func fixtureResponse(body string) *http.Response {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}
}

func TestRegistrySingleFlightAndStalePublish(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		if req.URL.Host == "old" {
			close(entered)
			<-release
			return fixtureResponse(remoteFixture("")), nil
		}
		return fixtureResponse(strings.ReplaceAll(remoteFixture(""), "test/remote-only", "test/new-source")), nil
	})}
	r := NewModelRegistry(modelTable, client)
	r.Configure(pluginConfig{ModelsURL: "http://old"})
	done := make(chan struct{})
	go func() { defer close(done); _ = r.Refresh(context.Background()) }()
	<-entered
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("duplicate in-flight download")
	}
	r.Configure(pluginConfig{ModelsURL: "http://new"})
	err := r.Refresh(context.Background())
	close(release)
	<-done
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Snapshot().Match("cmdcode-go/new-source"); !ok {
		t.Fatal("stale response overwrote new snapshot")
	}
	if _, ok := r.Snapshot().Match("cmdcode-go/remote-only"); ok {
		t.Fatal("old source published")
	}
}

func TestRegistrySnapshotIsolation(t *testing.T) {
	defs := []modelDef{{id: "test/a", efforts: []string{"high"}, vision: modelBool(true)}}
	r := NewModelRegistry(defs, nil)
	defs[0].efforts[0] = "low"
	*defs[0].vision = false
	d, _ := r.Snapshot().Definition("test/a")
	if d.efforts[0] != "high" || !*d.vision {
		t.Fatal("constructor aliases input")
	}
}

func TestRegistryRejectsCorruptBudgetsAndCollisions(t *testing.T) {
	for _, entry := range []string{
		`{"id":"test/x","context":100,"output":101}`,
		`{"id":"test/x","context":-1,"output":1}`,
		`{"id":"","context":100,"output":10}`,
		`{"id":"other/remote-only","context":100,"output":10}`,
	} {
		if _, _, err := parseModelsFile([]byte(remoteFixture("," + entry))); err == nil {
			t.Fatalf("accepted %s", entry)
		}
	}
}

func TestMessagePartsPreserveOrder(t *testing.T) {
	p := NewPlugin(nil, nil)
	defer p.Shutdown()
	raw, err := buildEnvelopeWithSnapshot("test/model", []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"first"},{"type":"image_url","image_url":{"url":"https://example.org/a.png"}},{"type":"text","text":"second"}]}]}`), p.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var env map[string]any
	_ = json.Unmarshal(raw, &env)
	parts := env["params"].(map[string]any)["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if parts[1].(map[string]any)["type"] != "image" {
		t.Fatalf("reordered: %s", raw)
	}
}

func TestDecoderModesAgree(t *testing.T) {
	tests := []struct {
		name, body string
		bad        bool
	}{
		{"valid", "{\"type\":\"text-delta\",\"text\":\"hello\"}\n{\"type\":\"finish\",\"finishReason\":\"stop\"}\n", false},
		{"malformed", "{\"type\":\"text-delta\",BAD}\n{\"type\":\"finish\",\"finishReason\":\"stop\"}\n", true},
		{"missing-type", "{}\n", true},
		{"truncated", "{\"type\":\"text-delta\",\"text\":\"partial\"}\n", true},
		{"unknown", "{\"type\":\"future-extension\"}\n{\"type\":\"finish\",\"finishReason\":\"stop\"}", false},
		{"after-finish", "{\"type\":\"finish\",\"finishReason\":\"stop\"}\n{\"type\":\"error\",\"error\":\"late\"}", false},
		{"upstream-error", "{\"type\":\"error\",\"error\":{\"statusCode\":429,\"message\":\"slow down\"}}", true},
	}
	for _, tc := range tests {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%v", tc.name, streaming), func(t *testing.T) {
				d := NewEventDecoder("m", streaming)
				err := d.Consume(strings.NewReader(tc.body), func([]byte) error { return nil })
				if (err != nil) != tc.bad {
					t.Fatalf("err=%v", err)
				}
			})
		}
	}
}

func TestDecoderBoundsAndStreamingRetention(t *testing.T) {
	d := NewEventDecoder("m", true)
	d.MaxEventBytes = 100
	if err := d.Consume(strings.NewReader(strings.Repeat("x", 101)), nil); err == nil {
		t.Fatal("event bound")
	}
	d = NewEventDecoder("m", true)
	d.MaxResponseBytes = 20
	if err := d.Consume(strings.NewReader(strings.Repeat("\n", 21)), nil); err == nil {
		t.Fatal("response bound")
	}
	d = NewEventDecoder("m", true)
	for i := 0; i < 100; i++ {
		if _, _, err := d.push([]byte(`{"type":"text-delta","text":"hello"}`)); err != nil {
			t.Fatal(err)
		}
	}
	if d.acc.text.Len() != 0 || !d.acc.seenContent {
		t.Fatal("stream duplicates full response")
	}
}

func TestStreamingUsageIncludesCache(t *testing.T) {
	d := NewEventDecoder("m", true)
	frames, done, err := d.push([]byte(`{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":20,"outputTokens":2,"inputTokenDetails":{"cacheReadTokens":10}}}`))
	if err != nil || !done || !bytes.Contains(frames[len(frames)-1], []byte(`"cached_tokens":10`)) {
		t.Fatalf("frames=%s err=%v", frames, err)
	}
}

func TestModelCapabilitiesAndBudgetSemantics(t *testing.T) {
	s := newModelSnapshot([]modelDef{
		{id: "test/fallback", context: 1000000, output: 32768, outputSource: "fallback", vision: modelBool(true), efforts: []string{"low", "high"}},
		{id: "test/confirmed", context: 1000000, output: 32768, gatewayOutput: 32768},
	})
	want := 100000
	if got := s.maxTokens(chatRequest{MaxTokens: &want}, "test/fallback"); got != want {
		t.Fatal("fallback clamped explicit budget", got)
	}
	if got := s.maxTokens(chatRequest{}, "test/fallback"); got != 32768 {
		t.Fatal("missing default budget", got)
	}
	if got := s.maxTokens(chatRequest{MaxTokens: &want}, "test/confirmed"); got != 32768 {
		t.Fatal("confirmed cap ignored", got)
	}
	m := s.Registered(pluginConfig{})[0]
	if m.OutputTokenLimit != 0 || len(m.SupportedInputModalities) != 2 || len(m.Thinking.Levels) != 2 {
		t.Fatalf("%+v", m)
	}
}

func TestPreparedTurnUsesOneConfigSnapshot(t *testing.T) {
	var header string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header = r.Header.Get("x-command-code-version")
		fmt.Fprintln(w, `{"type":"finish","finishReason":"stop"}`)
	}))
	defer srv.Close()
	p := NewPlugin(nil, nil)
	defer p.Shutdown()
	if err := p.Configure(configRequest("api_key: first\nbase_url: " + srv.URL + "\ncli_version: old\n")); err != nil {
		t.Fatal(err)
	}
	prep, raw, err := p.prepareTurn(pluginapi.ExecutorRequest{Model: "cmdcode-go/deepseek-v4-flash", Payload: []byte(`{"messages":[]}`)})
	if raw != nil || err != nil {
		t.Fatal(string(raw), err)
	}
	_ = p.Configure(configRequest("base_url: http://unreachable.invalid\ncli_version: new\n"))
	if _, raw, err = p.consumeTurn(context.Background(), prep); raw != nil || err != nil {
		t.Fatal(string(raw), err)
	}
	if header != "old" {
		t.Fatal("request mixed config generations")
	}
}

func streamRequest(id string) []byte {
	r := streamIDRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		Model: "cmdcode-go/deepseek-v4-flash", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
	}, StreamID: id}
	b, _ := json.Marshal(r)
	return b
}
func waitSignal(t *testing.T, c <-chan struct{}) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for lifecycle signal")
	}
}

func TestShutdownCancelsIdleStream(t *testing.T) {
	upstreamCanceled := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(upstreamCanceled)
	}))
	defer srv.Close()
	var closes atomic.Int32
	callbacks := hostCallbacks{emit: func(string, []byte) error { return nil }, close: func(string, string) error { closes.Add(1); return nil }}
	p := NewPlugin(nil, &callbacks)
	defer p.Shutdown()
	_ = p.Configure(configRequest("api_key: test\nbase_url: " + srv.URL + "\n"))
	if _, err := p.Handle("executor.execute_stream", streamRequest("idle")); err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	go func() { p.Shutdown(); close(stopped) }()
	waitSignal(t, stopped)
	waitSignal(t, upstreamCanceled)
	if closes.Load() != 1 {
		t.Fatalf("close callbacks=%d", closes.Load())
	}
	if _, err := p.Handle("model.route", nil); err == nil {
		t.Fatal("accepted work after shutdown")
	}
}

func TestShutdownWaitsForHostCallback(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `{"type":"text-delta","text":"hi"}`)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	callbacks := hostCallbacks{emit: func(string, []byte) error { close(entered); <-release; return errors.New("consumer closed") }, close: func(string, string) error { return nil }}
	p := NewPlugin(nil, &callbacks)
	_ = p.Configure(configRequest("api_key: test\nbase_url: " + srv.URL + "\n"))
	if _, err := p.Handle("executor.execute_stream", streamRequest("blocked")); err != nil {
		close(release)
		p.Shutdown()
		t.Fatal(err)
	}
	waitSignal(t, entered)
	stopped := make(chan struct{})
	go func() { p.Shutdown(); close(stopped) }()
	select {
	case <-stopped:
		close(release)
		t.Fatal("shutdown returned before callback")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	waitSignal(t, stopped)
}

func TestConcurrentBeginAndShutdown(t *testing.T) {
	p := NewPlugin(nil, nil)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, done, err := p.begin(nil)
			if err == nil {
				<-ctx.Done()
				done()
			}
		}()
	}
	p.Shutdown()
	wg.Wait()
}

func TestReadBounded(t *testing.T) {
	if _, err := readBounded(strings.NewReader("12345"), 4); err == nil {
		t.Fatal("oversize accepted")
	}
}

func TestCompiledRosterMatchesPublishedMetadata(t *testing.T) {
	raw, err := os.ReadFile("../models.json")
	if err != nil {
		t.Fatal(err)
	}
	defs, _, err := parseModelsFile(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(newModelSnapshot(defs).defs, newModelSnapshot(modelTable).defs) {
		t.Fatal("compiled fallback and published JSON metadata diverged")
	}
}
