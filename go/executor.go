package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type streamIDRequest struct {
	pluginapi.ExecutorRequest
	StreamID string `json:"stream_id"`
}

func decodeExecutorRequest(request []byte) (pluginapi.ExecutorRequest, string, error) {
	var r streamIDRequest
	if err := json.Unmarshal(request, &r); err != nil {
		return r.ExecutorRequest, "", fmt.Errorf("invalid executor request: %w", err)
	}
	return r.ExecutorRequest, r.StreamID, nil
}

type turnPrep struct {
	key      string
	envelope []byte
	config   pluginConfig
	release  func()
}

func (p *Plugin) prepareTurn(req pluginapi.ExecutorRequest) (turnPrep, []byte, error) {
	snap := p.snapshot()
	key := snap.config.resolveAPIKey(req.AuthAttributes)
	if key == "" {
		return turnPrep{}, errorEnvelope("missing_api_key", "cmdcode-go: no API key configured"), nil
	}
	selection := parseModelName(req.Model)
	if selection.base == "" {
		return turnPrep{}, requestErrorEnvelope("missing_model", "cmdcode-go: empty model"), nil
	}
	upstream, known := snap.models.Match(req.Model)
	if !known {
		return turnPrep{}, requestErrorEnvelope("unknown_model", "cmdcode-go: model is not in the current catalog"), nil
	}
	if !snap.models.Allowed(upstream, snap.config) {
		return turnPrep{}, requestErrorEnvelope("model_disabled", "cmdcode-go: model excluded by configuration"), nil
	}
	// Keep the suffix until envelope conversion, where it overrides body effort.
	sessionID := newUUID()
	release := func() {}
	if identity, ok := explicitSessionKey(req); ok {
		var acquired bool
		sessionID, release, acquired = p.sessions.acquire(identity)
		if !acquired {
			raw, _ := json.Marshal(pluginabi.Envelope{OK: false, Error: &pluginabi.Error{Code: "session_busy", Message: "cmdcode-go: session is busy; retry after the active turn finishes", HTTPStatus: http.StatusConflict}})
			return turnPrep{}, raw, nil
		}
	}
	envelope, err := buildEnvelopeForSession(req.Model, req.Payload, snap, sessionID)
	if err != nil {
		release()
		return turnPrep{}, requestErrorEnvelope("invalid_request", err.Error()), nil
	}
	return turnPrep{key: key, envelope: envelope, config: snap.config, release: release}, nil, nil
}

func requestErrorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(pluginabi.Envelope{OK: false, Error: &pluginabi.Error{
		Code: code, Message: message, HTTPStatus: http.StatusBadRequest,
	}})
	return raw
}

// upstreamErrorEnvelope maps gateway failures to host-native errors. The
// HTTPStatus rides into the host scheduler so 429s cool down and 401s do not
// retry instead of everything surfacing as a generic 500.
func upstreamErrorEnvelope(status int, body []byte) []byte {
	code := "upstream_error"
	switch {
	case status == 400:
		code = "bad_request"
	case status == 401:
		code = "unauthorized"
	case status == 402:
		code = "payment_required"
	case status == 403:
		code = "forbidden"
	case status == 404:
		code = "not_found"
	case status == 422:
		code = "unprocessable"
	case status == 429:
		code = "rate_limited"
	case status >= 500:
		code = "upstream_unavailable"
	}
	raw, _ := json.Marshal(pluginabi.Envelope{OK: false, Error: &pluginabi.Error{
		Code: "cmdcode-go:" + code,
		Message: fmt.Sprintf("cmdcode-go: upstream HTTP %d: %s",
			status, sampleBody(body)),
		HTTPStatus: status,
	}})
	return raw
}

func decodeError(err error) ([]byte, error) {
	var status *upstreamStatusError
	if errors.As(err, &status) {
		return upstreamErrorEnvelope(status.status, []byte(status.message)), nil
	}
	return nil, err
}

func (p *Plugin) consumeTurn(ctx context.Context, prep turnPrep) (*accumulated, []byte, error) {
	resp, cancel, err := p.Gateway.Open(ctx, prep.config, prep.envelope, prep.key)
	if err != nil {
		return nil, nil, err
	}
	defer cancel()
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodySample))
		return nil, upstreamErrorEnvelope(resp.StatusCode, body), nil
	}
	d := NewEventDecoder("", false)
	d.MaxEventBytes, d.MaxResponseBytes = p.Gateway.MaxEventBytes, p.Gateway.MaxResponseBytes
	if err := d.Consume(resp.Body, nil); err != nil {
		raw, e := decodeError(err)
		return nil, raw, e
	}
	return &d.acc, nil, nil
}

func (p *Plugin) executeNonStream(ctx context.Context, request []byte) ([]byte, error) {
	req, _, err := decodeExecutorRequest(request)
	if err != nil {
		return nil, err
	}
	prep, raw, err := p.prepareTurn(req)
	if raw != nil || err != nil {
		return raw, err
	}
	defer prep.release()
	acc, raw, err := p.consumeTurn(ctx, prep)
	if raw != nil || err != nil {
		return raw, err
	}
	return okEnvelope(pluginapi.ExecutorResponse{Payload: acc.toOpenAIResponse(req.Model), Headers: http.Header{"Content-Type": {"application/json"}}})
}

func (p *Plugin) executeStream(ctx context.Context, request []byte) ([]byte, error) {
	req, id, err := decodeExecutorRequest(request)
	if err != nil {
		return nil, err
	}
	prep, raw, err := p.prepareTurn(req)
	if raw != nil || err != nil {
		return raw, err
	}
	transferred := false
	defer func() {
		if !transferred {
			prep.release()
		}
	}()
	if id == "" {
		acc, raw, err := p.consumeTurn(ctx, prep)
		if raw != nil || err != nil {
			return raw, err
		}
		chunks := []pluginapi.ExecutorStreamChunk{}
		for _, f := range acc.toOpenAIChunks(req.Model) {
			chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: f})
		}
		return okEnvelope(map[string]any{"headers": map[string][]string{"content-type": {"text/event-stream"}}, "chunks": chunks})
	}
	// Streaming outlives the initiating RPC, so it gets a sibling task rooted
	// in the plugin lifetime, not the short-lived RPC context.
	streamCtx, done, err := p.begin(p.root)
	if err != nil {
		return nil, err
	}
	resp, cancel, err := p.Gateway.Open(streamCtx, prep.config, prep.envelope, prep.key)
	if err != nil {
		done()
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodySample))
		resp.Body.Close()
		cancel()
		done()
		return upstreamErrorEnvelope(resp.StatusCode, body), nil
	}
	transferred = true
	go func() { defer done(); defer prep.release(); p.relayStream(streamCtx, id, req.Model, resp.Body, cancel) }()
	return okEnvelope(map[string]any{"headers": map[string][]string{"content-type": {"text/event-stream"}}, "chunks": []pluginapi.ExecutorStreamChunk{}})
}

const countResponseID = "cmdcode-go-count-tokens-v1"

// countTokens returns an estimate, not a tokenizer result. The pinned host
// sends count results through its normal completion translator, not its token
// count translator, and loses the original format before this RPC. The marker
// and prompt usage survive OpenAI -> Claude conversion; normalizeAfter then
// restores Claude's count-only response. Same-format callers use input_tokens.
func (p *Plugin) countTokens(request []byte) ([]byte, error) {
	req, _, err := decodeExecutorRequest(request)
	if err != nil {
		return nil, err
	}
	snap := p.snapshot()
	upstream, known := snap.models.Match(req.Model)
	if !known {
		return requestErrorEnvelope("unknown_model", "cmdcode-go: model is not in the current catalog"), nil
	}
	if !snap.models.Allowed(upstream, snap.config) {
		return requestErrorEnvelope("model_disabled", "cmdcode-go: model excluded by configuration"), nil
	}
	payload := req.Payload
	if len(req.OriginalRequest) > 0 {
		payload = req.OriginalRequest
	}
	estimate := len(payload)/4 + 8
	if estimate < 0 {
		estimate = 0
	}
	payload, err = json.Marshal(map[string]any{
		"id": countResponseID, "input_tokens": estimate,
		"usage": map[string]int{"prompt_tokens": estimate},
	})
	if err != nil {
		return nil, err
	}
	return okEnvelope(pluginapi.ExecutorResponse{
		Payload:  payload,
		Headers:  http.Header{"Content-Type": {"application/json"}, "X-Cmdcode-Token-Count": {"estimate"}},
		Metadata: map[string]any{"token_count_estimated": true},
	})
}

func sampleBody(body []byte) string {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > maxErrorBodySample {
		trimmed = trimmed[:maxErrorBodySample]
	}
	if len(trimmed) == 0 {
		return "(empty body)"
	}
	return string(trimmed)
}
