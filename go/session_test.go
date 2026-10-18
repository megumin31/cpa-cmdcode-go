package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/translator/builtin"
)

func sessionRequest() pluginapi.ExecutorRequest {
	return pluginapi.ExecutorRequest{AuthID: "account-a", Model: "cmdcode-go/reasoner", Metadata: map[string]any{"caller_scope": "host-caller-a"}, Headers: http.Header{"X-Session-Id": {"conversation-a"}}, Payload: []byte(`{"messages":[{"role":"user","content":"hello"}]}`)}
}
func prepID(t *testing.T, p *Plugin, r pluginapi.ExecutorRequest) (string, turnPrep) {
	t.Helper()
	prep, raw, err := p.prepareTurn(r)
	if err != nil || raw != nil {
		t.Fatalf("prepare failed: %v %s", err, raw)
	}
	var e struct{ ThreadID string }
	if json.Unmarshal(prep.envelope, &e) != nil || e.ThreadID == "" {
		t.Fatal("missing identity")
	}
	return e.ThreadID, prep
}

func TestExplicitSessionEvidence(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*pluginapi.ExecutorRequest)
		want   bool
	}{
		{"header", func(r *pluginapi.ExecutorRequest) {}, true},
		{"header lower case", func(r *pluginapi.ExecutorRequest) { r.Headers = http.Header{"x-session-id": {"conversation-a"}} }, true},
		{"body", func(r *pluginapi.ExecutorRequest) {
			r.Headers = nil
			r.OriginalRequest = []byte(`{"session_id":"conversation-a"}`)
		}, true},
		{"host execution", func(r *pluginapi.ExecutorRequest) {
			r.Headers = nil
			r.Metadata["execution_session_id"] = "conversation-a"
		}, true},
		{"canonical alone", func(r *pluginapi.ExecutorRequest) {
			r.Headers = nil
			r.Metadata["canonical_session_id"] = "ctx:v1:derived"
		}, false},
		{"LCP alone", func(r *pluginapi.ExecutorRequest) { r.Headers = nil; r.Metadata["lcp_affinity_session_id"] = "lcp" }, false},
		{"derived alone", func(r *pluginapi.ExecutorRequest) { r.Headers = nil; r.Metadata["derived_session_id"] = "hash" }, false},
		{"explicit plus inferred", func(r *pluginapi.ExecutorRequest) { r.Metadata["canonical_session_id"] = "ctx:v1:derived" }, true},
		{"missing caller", func(r *pluginapi.ExecutorRequest) { delete(r.Metadata, "caller_scope") }, false},
		{"missing auth", func(r *pluginapi.ExecutorRequest) { r.AuthID = "" }, false},
		{"client caller cannot scope", func(r *pluginapi.ExecutorRequest) {
			r.Metadata = nil
			r.OriginalRequest = []byte(`{"metadata":{"caller_scope":"fake"}}`)
		}, false},
		{"multiple equal headers", func(r *pluginapi.ExecutorRequest) {
			r.Headers["X-Session-Id"] = []string{"conversation-a", "conversation-a"}
		}, false},
		{"joined header", func(r *pluginapi.ExecutorRequest) { r.Headers["X-Session-Id"] = []string{"a,b"} }, false},
		{"case aliases conflict", func(r *pluginapi.ExecutorRequest) { r.Headers["x-session-id"] = []string{"other"} }, false},
		{"body header conflict", func(r *pluginapi.ExecutorRequest) { r.OriginalRequest = []byte(`{"session_id":"other"}`) }, false},
		{"execution conflict", func(r *pluginapi.ExecutorRequest) { r.Metadata["execution_session_id"] = "other" }, false},
		{"invalid type", func(r *pluginapi.ExecutorRequest) { r.OriginalRequest = []byte(`{"session_id":123}`) }, false},
		{"oversize", func(r *pluginapi.ExecutorRequest) { r.Headers["X-Session-Id"] = []string{strings.Repeat("a", 257)} }, false},
		{"control", func(r *pluginapi.ExecutorRequest) { r.Headers["X-Session-Id"] = []string{"bad\nvalue"} }, false},
		{"spaces", func(r *pluginapi.ExecutorRequest) { r.Headers["X-Session-Id"] = []string{" trimmed "} }, false},
		{"duplicate JSON", func(r *pluginapi.ExecutorRequest) {
			r.OriginalRequest = []byte(`{"session_id":"conversation-a","session_id":"other"}`)
		}, false},
		{"nested duplicate", func(r *pluginapi.ExecutorRequest) {
			r.OriginalRequest = []byte(`{"metadata":{"session_id":"conversation-a","session_id":"other"}}`)
		}, false},
		{"cache user parent request not sessions", func(r *pluginapi.ExecutorRequest) {
			r.Headers = http.Header{"X-Client-Request-Id": {"req"}}
			r.OriginalRequest = []byte(`{"prompt_cache_key":"cache","parent_session_id":"parent","metadata":{"user_id":"user"}}`)
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := sessionRequest()
			tc.mutate(&r)
			_, ok := explicitSessionKey(r)
			if ok != tc.want {
				t.Fatalf("reuse=%v want %v", ok, tc.want)
			}
		})
	}
}

func TestSessionReuseIsolationAndStateless(t *testing.T) {
	p := regressionPlugin(t)
	r := sessionRequest()
	first, prep := prepID(t, p, r)
	prep.release()
	again, prep := prepID(t, p, r)
	prep.release()
	if first != again {
		t.Fatal("sequential session changed")
	}
	r.AuthID = "account-b"
	other, prep := prepID(t, p, r)
	prep.release()
	if other == first {
		t.Fatal("account isolation failed")
	}
	r.AuthID = "account-a"
	r.Metadata["caller_scope"] = "host-caller-b"
	other, prep = prepID(t, p, r)
	prep.release()
	if other == first {
		t.Fatal("tenant isolation failed")
	}
	r.Metadata = nil
	a, prep := prepID(t, p, r)
	prep.release()
	b, prep := prepID(t, p, r)
	prep.release()
	if a == b {
		t.Fatal("unscoped request reused")
	}
}

func TestSessionBusyAndInvalidTurnRelease(t *testing.T) {
	p := regressionPlugin(t)
	r := sessionRequest()
	id, prep := prepID(t, p, r)
	_, raw, err := p.prepareTurn(r)
	var e pluginabi.Envelope
	_ = json.Unmarshal(raw, &e)
	if err != nil || e.Error == nil || e.Error.HTTPStatus != 409 || e.Error.Code != "session_busy" {
		t.Fatal("expected explicit busy error")
	}
	prep.release()
	prep.release()
	id2, prep := prepID(t, p, r)
	prep.release()
	if id != id2 {
		t.Fatal("busy changed identity")
	}
	r.Payload = []byte(`not json`)
	_, _, _ = p.prepareTurn(r)
	r.Payload = []byte(`{"messages":[]}`)
	_, prep = prepID(t, p, r)
	prep.release()
}

func TestSessionStoreCapacityTTLAndAtomicity(t *testing.T) {
	s := newSessionStore()
	s.capacity = 2
	now := time.Unix(1000, 0)
	s.now = func() time.Time { return now }
	s.ttl = time.Minute
	a := sessionKey{"c", "a", "a"}
	b := sessionKey{"c", "a", "b"}
	c := sessionKey{"c", "a", "c"}
	first, release, ok := s.acquire(a)
	if !ok {
		t.Fatal("acquire")
	}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, r, ok := s.acquire(a); ok {
				r()
				t.Error("concurrent acquire succeeded")
			}
		}()
	}
	wg.Wait()
	now = now.Add(2 * time.Minute)
	_, rb, ok := s.acquire(b)
	if !ok {
		t.Fatal("second")
	}
	if _, _, ok := s.acquire(c); ok {
		t.Fatal("evicted active entries")
	}
	rb()
	_, rc, ok := s.acquire(c)
	if !ok {
		t.Fatal("idle eviction")
	}
	rc()
	release()
	same, ra, ok := s.acquire(a)
	if !ok || same != first {
		t.Fatal("active entry expired")
	}
	ra()
	now = now.Add(2 * time.Minute)
	fresh, rf, ok := s.acquire(a)
	if !ok || fresh == first {
		t.Fatal("TTL not applied")
	}
	rf()
	if len(s.entries) > 2 {
		t.Fatal("unbounded")
	}
}

type sessionTransport func(*http.Request) (*http.Response, error)

func (f sessionTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestSessionCABIResponsesHeaderAndUnchangedHistory(t *testing.T) {
	p := regressionPlugin(t)
	r := sessionRequest()
	r.SourceFormat = "chat-completions"
	r.Format = "chat-completions" // Host has already translated Responses.
	r.OriginalRequest = []byte(`{"input":[{"role":"user","content":"hello"}],"model":"test/reasoner"}`)
	r.Payload = []byte(`{"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"checking","reasoning_content":"historical reasoning","tool_calls":[{"id":"call1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call1","content":"result"},{"role":"user","content":"continue"}]}`)
	original := bytes.Clone(r.Payload)
	raw := regressionJSON(t, streamIDRequest{ExecutorRequest: r, StreamID: "stream"})
	decoded, stream, err := decodeExecutorRequest(raw)
	if err != nil || stream != "stream" || decoded.AuthID != r.AuthID || decoded.Metadata["caller_scope"] != r.Metadata["caller_scope"] {
		t.Fatal("ABI lost execution fields")
	}
	id, prep := prepID(t, p, decoded)
	defer prep.release()
	baseline, err := buildEnvelopeWithSnapshot(r.Model, r.Payload, p.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var before, after map[string]json.RawMessage
	_ = json.Unmarshal(baseline, &before)
	_ = json.Unmarshal(prep.envelope, &after)
	if !bytes.Equal(before["params"], after["params"]) || !bytes.Equal(original, r.Payload) {
		t.Fatal("session layer changed messages or reasoning")
	}
	seen := 0
	p.Gateway = NewGatewayClient(&http.Client{Transport: sessionTransport(func(req *http.Request) (*http.Response, error) {
		seen++
		var body struct{ ThreadID string }
		_ = json.NewDecoder(req.Body).Decode(&body)
		if body.ThreadID != id || req.Header.Get("x-session-id") != id {
			t.Error("header/body identity mismatch")
		}
		return nil, errors.New("offline failure")
	})})
	for i := 0; i < 2; i++ {
		_, cancel, _ := p.Gateway.Open(context.Background(), prep.config, prep.envelope, prep.key)
		if cancel != nil {
			cancel()
		}
	}
	if seen != 2 {
		t.Fatal("retry did not execute")
	}
}

func TestSessionExecuteFailureReleases(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "nonstream", true: "stream"}[stream], func(t *testing.T) {
			p := regressionPlugin(t)
			r := sessionRequest()
			raw := regressionJSON(t, streamIDRequest{ExecutorRequest: r, StreamID: "test-stream"})
			if stream {
				_, _ = p.executeStream(context.Background(), raw)
			} else {
				_, _ = p.executeNonStream(context.Background(), raw)
			}
			_, prep := prepID(t, p, r)
			prep.release()
		})
	}
}

func TestSessionStreamHoldsLeaseUntilBodyClosed(t *testing.T) {
	p := regressionPlugin(t)
	reader, writer := io.Pipe()
	p.host = hostCallbacks{emit: func(string, []byte) error { return nil }, close: func(string, string) error { return nil }}
	p.Gateway = NewGatewayClient(&http.Client{Transport: sessionTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: reader, Header: http.Header{}}, nil
	})})
	r := sessionRequest()
	raw := regressionJSON(t, streamIDRequest{ExecutorRequest: r, StreamID: "stream"})
	if _, err := p.executeStream(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	key, _ := explicitSessionKey(r)
	if _, _, ok := p.sessions.acquire(key); ok {
		t.Fatal("stream lease released early")
	}
	_ = writer.Close()
	p.tasks.Wait()
	_, release, ok := p.sessions.acquire(key)
	if !ok {
		t.Fatal("stream lease not released")
	}
	release()
}

func TestSessionDuplicateCaseHeaderRejected(t *testing.T) {
	r := sessionRequest()
	r.Headers["x-session-id"] = []string{"conversation-a"}
	if _, ok := explicitSessionKey(r); ok {
		t.Fatal("duplicate case-folded header accepted")
	}
}

func TestSessionTerminalPathsRelease(t *testing.T) {
	const complete = "{\"type\":\"text-delta\",\"text\":\"hello\"}\n{\"type\":\"finish\",\"finishReason\":\"stop\"}\n"
	for _, tc := range []struct {
		name, body string
		status     int
		emitError  bool
	}{
		{"success", complete, 200, false}, {"http_error", "offline upstream error", 503, false},
		{"decoder_error", "{bad json}\n", 200, false}, {"callback_error", complete, 200, true},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: "/nonstream", true: "/stream"}[stream], func(t *testing.T) {
				p := regressionPlugin(t)
				var terminal string
				var emits int
				p.host = hostCallbacks{emit: func(string, []byte) error {
					emits++
					if tc.emitError {
						return errors.New("offline callback error")
					}
					return nil
				}, close: func(_ string, message string) error { terminal = message; return nil }}
				p.Gateway = NewGatewayClient(&http.Client{Transport: sessionTransport(func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body)), Header: http.Header{}}, nil
				})})
				r := sessionRequest()
				key, _ := explicitSessionKey(r)
				raw := regressionJSON(t, streamIDRequest{ExecutorRequest: r, StreamID: "s"})
				var response []byte
				var callErr error
				if stream {
					response, callErr = p.executeStream(context.Background(), raw)
					p.tasks.Wait()
				} else {
					response, callErr = p.executeNonStream(context.Background(), raw)
				}
				if tc.status != 200 {
					var envelope pluginabi.Envelope
					_ = json.Unmarshal(response, &envelope)
					if envelope.Error == nil || envelope.Error.HTTPStatus != tc.status {
						t.Fatal("HTTP error fixture did not reach expected path")
					}
				} else if tc.name == "decoder_error" {
					if stream && terminal == "" || !stream && callErr == nil {
						t.Fatal("decoder error was not exercised")
					}
				} else if stream && tc.emitError {
					if emits == 0 || terminal == "" {
						t.Fatal("emit failure was not exercised")
					}
				} else if callErr != nil || terminal != "" {
					t.Fatal("successful fixture failed")
				}
				_, release, ok := p.sessions.acquire(key)
				if !ok {
					t.Fatal("terminal path retained session")
				}
				release()
			})
		}
	}
}

func TestSessionCancellationReleasesStream(t *testing.T) {
	p := regressionPlugin(t)
	p.host = hostCallbacks{emit: func(string, []byte) error { return nil }, close: func(string, string) error { return nil }}
	p.Gateway = NewGatewayClient(&http.Client{Transport: sessionTransport(func(r *http.Request) (*http.Response, error) {
		reader, writer := io.Pipe()
		go func() { <-r.Context().Done(); _ = writer.CloseWithError(r.Context().Err()) }()
		return &http.Response{StatusCode: 200, Body: reader, Header: http.Header{}}, nil
	})})
	r := sessionRequest()
	raw := regressionJSON(t, streamIDRequest{ExecutorRequest: r, StreamID: "s"})
	if _, err := p.executeStream(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	p.cancel()
	p.tasks.Wait()
	key, _ := explicitSessionKey(r)
	_, release, ok := p.sessions.acquire(key)
	if !ok {
		t.Fatal("cancellation retained session")
	}
	release()
}

func TestSessionResponsesThroughPinnedHostTranslation(t *testing.T) {
	p := regressionPlugin(t)
	var first string
	for _, original := range []string{
		`{"model":"test/reasoner","input":[{"role":"user","content":"hello"}]}`,
		`{"model":"test/reasoner","input":[{"role":"user","content":"hello"},{"role":"assistant","content":"answer"},{"role":"user","content":"continue"}]}`,
	} {
		r := sessionRequest()
		r.OriginalRequest = []byte(original)
		r.Payload = builtin.Registry().TranslateRequest(sdktranslator.FormatOpenAIResponse, sdktranslator.FormatOpenAI, r.Model, r.OriginalRequest, false)
		r.SourceFormat = sdktranslator.FormatOpenAI.String()
		r.Format = sdktranslator.FormatOpenAI.String()
		r.Metadata["canonical_session_id"] = "conversation-a"
		decoded, _, err := decodeExecutorRequest(regressionJSON(t, r))
		if err != nil {
			t.Fatal(err)
		}
		id, prep := prepID(t, p, decoded)
		prep.release()
		if first != "" && id != first {
			t.Fatal("Responses continuation changed UUID")
		}
		first = id
	}
}
