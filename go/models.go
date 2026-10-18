package main

import (
	"fmt"
	"strings"

	"cpa-cmdcode-go/internal/buildinfo"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type modelDef struct {
	id, display, description string
	context, output          int64
	outputSource             string
	contextSource            string
	gatewayOutput            int64
	vision                   *bool
	efforts                  []string
}

func modelBool(v bool) *bool { return &v }

func shortModelName(id string) string { return id[strings.LastIndex(id, "/")+1:] }

// A snapshot owns its slices and indexes; callers never mutate it.
type modelSnapshot struct {
	defs    []modelDef
	byID    map[string]int
	byShort map[string]int
}

func newModelSnapshot(defs []modelDef) *modelSnapshot {
	s := &modelSnapshot{defs: append([]modelDef(nil), defs...), byID: map[string]int{}, byShort: map[string]int{}}
	for i := range s.defs {
		d := &s.defs[i]
		d.efforts = append([]string(nil), d.efforts...)
		if d.vision != nil {
			v := *d.vision
			d.vision = &v
		}
		s.byID[strings.ToLower(d.id)] = i
		short := strings.ToLower(shortModelName(d.id))
		if _, exists := s.byShort[short]; exists {
			s.byShort[short] = -1
		} else {
			s.byShort[short] = i
		}
	}
	return s
}

type modelSelection struct {
	base   string
	suffix *string
}

// parseModelName follows CPA's terminal model(value) syntax. Namespace and
// suffix handling is shared by lookup, routing, filtering and execution.
func parseModelName(name string) modelSelection {
	name = strings.TrimSpace(name)
	selection := modelSelection{base: name}
	if at := strings.LastIndex(name, "("); at >= 0 && strings.HasSuffix(name, ")") {
		value := strings.ToLower(strings.TrimSpace(name[at+1 : len(name)-1]))
		selection.base = strings.TrimSpace(name[:at])
		selection.suffix = &value
	}
	for _, prefix := range []string{"cpa-cmdcode-go/", ProviderKey + "/"} {
		if strings.HasPrefix(strings.ToLower(selection.base), prefix) {
			selection.base = selection.base[len(prefix):]
			break
		}
	}
	return selection
}

func (s *modelSnapshot) Match(name string) (string, bool) {
	key := strings.ToLower(parseModelName(name).base)
	if i, ok := s.byID[key]; ok {
		return s.defs[i].id, true
	}
	if !strings.Contains(key, "/") {
		if i, ok := s.byShort[key]; ok && i >= 0 {
			return s.defs[i].id, true
		}
	}
	return "", false
}

func (s *modelSnapshot) Allowed(id string, cfg pluginConfig) bool {
	for _, x := range cfg.DisableModels {
		if canonical, ok := s.Match(x); ok && canonical == id {
			return false
		}
	}
	if len(cfg.Models) == 0 {
		return true
	}
	for _, x := range cfg.Models {
		if canonical, ok := s.Match(x); ok && canonical == id {
			return true
		}
	}
	return false
}

func (s *modelSnapshot) Resolve(name string) string {
	if id, ok := s.Match(name); ok {
		return id
	}
	return parseModelName(name).base
}

func (s *modelSnapshot) Definition(id string) (modelDef, bool) {
	key := strings.ToLower(parseModelName(id).base)
	i, ok := s.byID[key]
	if !ok {
		return modelDef{}, false
	}
	return s.defs[i], true
}

func gatewayEffort(level string) bool {
	switch level {
	case "low", "medium", "high", "xhigh", "max":
		return true
	}
	return false
}

func supportedEfforts(d modelDef) []string {
	var levels []string
	for _, level := range d.efforts {
		if gatewayEffort(level) {
			levels = append(levels, level)
		}
	}
	return levels
}

func (s *modelSnapshot) reasoningEffort(selection modelSelection, body *string) (*string, error) {
	requested := body
	if selection.suffix != nil {
		requested = selection.suffix // CPA gives an explicit suffix precedence.
	}
	if requested == nil {
		return nil, nil
	}
	level := strings.ToLower(strings.TrimSpace(*requested))
	// Automatic/default mode means leave selection to the gateway. An empty
	// suffix is a malformed instruction, rather than an accidental default.
	if level == "auto" || level == "default" || level == "-1" || (level == "" && selection.suffix == nil) {
		return nil, nil
	}
	// Disable/minimal requests cannot be represented by this gateway contract.
	// Omit them for compatibility, leaving the provider default in effect.
	// This is deliberately NOT a promise that reasoning has been disabled.
	if level == "none" || level == "off" || level == "minimal" {
		return nil, nil
	}
	if gatewayEffort(level) {
		if d, ok := s.Definition(selection.base); ok {
			// An empty catalog column is not a verified denial of capability.
			// Preserve the original gateway-compatible level in that case.
			if len(d.efforts) == 0 {
				return &level, nil
			}
			for _, supported := range supportedEfforts(d) {
				if level == supported {
					return &level, nil
				}
			}
			// Match the official client: unsupported levels use the model default.
			return nil, nil
		}
	}
	return nil, fmt.Errorf("cmdcode-go: reasoning effort %q is unsupported for model %q", level, selection.base)
}

func (s *modelSnapshot) Registered(cfg pluginConfig) []pluginapi.ModelInfo {
	result := make([]pluginapi.ModelInfo, 0, len(s.defs))
	for _, d := range s.defs {
		short := shortModelName(d.id)
		// Remote catalogs reject collisions. Also fail closed for any compiled
		// snapshot that cannot route its public short name unambiguously.
		if short == "" || s.byShort[strings.ToLower(short)] < 0 {
			continue
		}
		if !s.Allowed(d.id, cfg) {
			continue
		}
		inputs := []string{"text"}
		if d.vision != nil && *d.vision {
			inputs = append(inputs, "image")
		}
		// Unknown/fallback budgets are not advertised as verified model limits.
		context := d.context
		if d.contextSource == "fallback" {
			context = 0
		}
		output := d.output
		if d.outputSource == "fallback" {
			output = 0
		}
		if d.gatewayOutput > 0 {
			output = d.gatewayOutput
		}
		// Only the public ID is shortened; d.id remains the gateway request ID.
		modelID := ProviderKey + "/" + short
		m := pluginapi.ModelInfo{
			ID: modelID, Object: "model", OwnedBy: ProviderKey, Type: ProviderKey,
			DisplayName: d.display, Name: modelID, Description: d.description,
			ContextLength: context, InputTokenLimit: context,
			MaxCompletionTokens: output, OutputTokenLimit: output,
			SupportedGenerationMethods: []string{"chat"},
			SupportedInputModalities:   inputs, SupportedOutputModalities: []string{"text"},
			SupportedParameters: []string{"temperature", "max_tokens", "tools", "stream"},
			UserDefined:         true,
		}
		if levels := supportedEfforts(d); len(levels) > 0 {
			m.Thinking = &pluginapi.ThinkingSupport{Levels: levels}
			m.SupportedParameters = append(m.SupportedParameters, "reasoning_effort")
		}
		result = append(result, m)
	}
	return result
}

func registration() map[string]any {
	return map[string]any{
		"schema_version": pluginabi.SchemaVersion,
		"metadata": map[string]any{
			"Name":             buildinfo.Provider,
			"Version":          buildinfo.Version,
			"Author":           "cmdcode-go contributors",
			"GitHubRepository": "https://github.com/megumin31/cpa-cmdcode-go",
			"Description":      "CommandCode Go ($1/mo) plan via the CLI /alpha/generate gateway. Unofficial.",
			"ConfigFields": []map[string]any{
				{"Name": "api_key", "Type": "string", "Description": "CommandCode user_ key. Falls back to COMMANDCODE_KEY env and ~/.commandcode/auth.json."},
				{"Name": "base_url", "Type": "string", "Description": "Gateway base URL. Default https://api.commandcode.ai."},
				{"Name": "cli_version", "Type": "string", "Description": "x-command-code-version fingerprint. Default tracks the reversed CLI release."},
				{"Name": "project_slug", "Type": "string", "Description": "x-project-slug fingerprint header."},
				{"Name": "permission_mode", "Type": "string", "Description": "Envelope permissionMode. Default standard."},
				{"Name": "models_url", "Type": "string", "Description": "Remote models.json URL (Action-maintained). Default tracks this repo's main branch; empty = default."},
				{"Name": "models_file", "Type": "string", "Description": "Local models.json path override (reloaded on change). Takes precedence over models_url."},
				{"Name": "models_refresh_interval", "Type": "string", "Description": "Remote refresh interval (Go duration, default 6h; 0 disables remote refresh)."},
			},
		},
		"capabilities": map[string]any{
			"model_registrar":            true,
			"model_provider":             true,
			"model_router":               true,
			"executor":                   true,
			"executor_model_scope":       "both",
			"executor_input_formats":     []string{"chat-completions"},
			"executor_output_formats":    []string{"chat-completions"},
			"response_before_translator": true,
			"response_after_translator":  true,
		},
	}
}
