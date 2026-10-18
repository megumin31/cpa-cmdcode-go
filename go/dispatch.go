package main

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func (p *Plugin) Handle(method string, request []byte) ([]byte, error) {
	ctx, done, err := p.begin(nil)
	if err != nil {
		return nil, err
	}
	defer done()
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if err := p.Configure(request); err != nil {
			return errorEnvelope("invalid_config", err.Error()), nil
		}
		return okEnvelope(registration())
	case pluginabi.MethodModelRegister:
		if err := p.Models.Refresh(ctx); err != nil {
			log.Printf("cmdcode-go: model refresh failed: %v", err)
		}
		snap := p.snapshot()
		return okEnvelope(pluginapi.ModelRegistrationResponse{Provider: ProviderKey, Models: snap.models.Registered(snap.config)})
	case pluginabi.MethodModelStatic, pluginabi.MethodModelForAuth:
		if err := p.Models.Refresh(ctx); err != nil {
			log.Printf("cmdcode-go: model refresh failed: %v", err)
		}
		snap := p.snapshot()
		return okEnvelope(pluginapi.ModelResponse{Provider: ProviderKey, Models: snap.models.Registered(snap.config)})
	case pluginabi.MethodModelRoute:
		return okEnvelope(p.routeModel(request))
	case pluginabi.MethodExecutorIdentifier:
		return okEnvelope(map[string]string{"identifier": ProviderKey})
	case pluginabi.MethodExecutorExecute:
		return p.executeNonStream(ctx, request)
	case pluginabi.MethodExecutorExecuteStream:
		return p.executeStream(ctx, request)
	case pluginabi.MethodExecutorCountTokens:
		return p.countTokens(request)
	case pluginabi.MethodResponseNormalizeBefore:
		return normalizeBefore(request)
	case pluginabi.MethodResponseNormalizeAfter:
		return normalizeAfter(request)
	case pluginabi.MethodExecutorHTTPRequest:
		return okEnvelope(pluginapi.ExecutorHTTPResponse{
			StatusCode: http.StatusNotImplemented,
			Headers:    http.Header{"Content-Type": []string{"application/json"}},
			Body:       []byte(`{"error":"cmdcode-go has no generic HTTP bridge"}`),
		})
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func (p *Plugin) routeModel(request []byte) pluginapi.ModelRouteResponse {
	var req pluginapi.ModelRouteRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return pluginapi.ModelRouteResponse{Handled: false}
	}
	snap := p.snapshot()
	if canonical, ok := snap.models.Match(req.RequestedModel); ok && snap.models.Allowed(canonical, snap.config) {
		selection := parseModelName(req.RequestedModel)
		selection.base = canonical
		if _, err := snap.models.reasoningEffort(selection, nil); err != nil {
			return pluginapi.ModelRouteResponse{Handled: false}
		}
		return pluginapi.ModelRouteResponse{Handled: true, TargetKind: pluginapi.ModelRouteTargetSelf, Reason: "cmdcode-go model"}
	}
	return pluginapi.ModelRouteResponse{Handled: false}
}
