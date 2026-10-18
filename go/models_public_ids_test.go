package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestPublicModelIDsKeepUpstreamIdentity(t *testing.T) {
	raw, err := os.ReadFile("../models.json")
	if err != nil {
		t.Fatal(err)
	}
	downloaded, _, err := parseModelsFile(raw)
	if err != nil {
		t.Fatal(err)
	}
	for name, defs := range map[string][]modelDef{"compiled": modelTable, "downloaded": downloaded} {
		t.Run(name, func(t *testing.T) {
			p := regressionPlugin(t)
			p.Models = NewModelRegistry(defs, &http.Client{Transport: regressionNoNetwork{}})
			p.Models.Configure(p.Config.Snapshot())
			models := p.Models.Snapshot().Registered(pluginConfig{})
			if len(models) != len(defs) {
				t.Fatalf("registered %d of %d models", len(models), len(defs))
			}
			seen := map[string]bool{}
			for i, model := range models {
				upstream := defs[i].id
				want := ProviderKey + "/" + upstream[strings.LastIndex(upstream, "/")+1:]
				if model.ID != want || model.Name != want || strings.Count(model.ID, "/") != 1 {
					t.Fatalf("public ID/Name = %q/%q, want %q", model.ID, model.Name, want)
				}
				if seen[strings.ToLower(model.ID)] {
					t.Fatalf("duplicate public ID: %s", model.ID)
				}
				seen[strings.ToLower(model.ID)] = true
				for _, alias := range []string{model.ID, "cpa-cmdcode-go/" + strings.TrimPrefix(model.ID, ProviderKey+"/"), ProviderKey + "/" + upstream, upstream} {
					if got, ok := p.Models.Snapshot().Match(alias); !ok || got != upstream {
						t.Fatalf("Match(%q) = %q, %v; want %q", alias, got, ok, upstream)
					}
					prep, failure, err := p.prepareTurn(pluginapi.ExecutorRequest{Model: alias, Payload: []byte(`{"messages":[]}`)})
					if err != nil || len(failure) != 0 {
						t.Fatalf("prepare %q: %v %s", alias, err, failure)
					}
					prep.release()
					var envelope generateRequest
					if err := json.Unmarshal(prep.envelope, &envelope); err != nil || envelope.Params.Model != upstream {
						t.Fatalf("upstream identity changed for %q: %v %s", alias, err, prep.envelope)
					}
				}
			}
			// All host-facing listing methods must expose exactly the same IDs.
			for _, method := range []string{pluginabi.MethodModelRegister, pluginabi.MethodModelStatic, pluginabi.MethodModelForAuth} {
				body, err := p.Handle(method, nil)
				if err != nil {
					t.Fatal(err)
				}
				var envelope struct {
					OK     bool
					Result pluginapi.ModelResponse
				}
				if err := json.Unmarshal(body, &envelope); err != nil || !envelope.OK || len(envelope.Result.Models) != len(models) {
					t.Fatalf("%s returned unexpected models: %v %s", method, err, body)
				}
				for i, model := range envelope.Result.Models {
					if model.ID != models[i].ID || model.Name != models[i].Name {
						t.Fatalf("%s exposed inconsistent public identity: %+v", method, model)
					}
				}
			}
		})
	}
}

func TestPublicModelAliasesShareFiltersAndReasoning(t *testing.T) {
	aliases := []string{
		"cmdcode-go/reasoner(high)",
		"cmdcode-go/test/reasoner(high)",
		"cpa-cmdcode-go/reasoner(high)",
		"cpa-cmdcode-go/test/reasoner(high)",
		"test/reasoner(high)",
		"reasoner(high)",
		"CMDCODE-GO/REASONER(HIGH)",
	}
	for _, filter := range aliases {
		t.Run(filter, func(t *testing.T) {
			p := regressionPlugin(t)
			if err := p.Configure(regressionJSON(t, map[string]string{"config_yaml": "models: ['" + filter + "']"})); err != nil {
				t.Fatal(err)
			}
			registered := p.Models.Snapshot().Registered(p.Config.Snapshot())
			if len(registered) != 1 || registered[0].ID != "cmdcode-go/reasoner" {
				t.Fatalf("allowlist did not select the short public ID: %+v", registered)
			}
			for _, alias := range aliases {
				if route := p.routeModel(regressionJSON(t, pluginapi.ModelRouteRequest{RequestedModel: alias})); !route.Handled {
					t.Fatalf("allowed alias not routed: %s", alias)
				}
				prep, failure, err := p.prepareTurn(pluginapi.ExecutorRequest{Model: alias, Payload: []byte(`{"messages":[],"reasoning_effort":"low"}`)})
				if err != nil || len(failure) != 0 {
					t.Fatalf("prepare %q: %v %s", alias, err, failure)
				}
				prep.release()
				var envelope generateRequest
				if err := json.Unmarshal(prep.envelope, &envelope); err != nil || envelope.Params.Model != "test/reasoner" || envelope.Params.ReasoningEffort == nil || *envelope.Params.ReasoningEffort != "high" {
					t.Fatalf("alias lost upstream ID or suffix precedence: %v %s", err, prep.envelope)
				}
			}
			if err := p.Configure(regressionJSON(t, map[string]string{"config_yaml": "disable_models: ['" + filter + "']"})); err != nil {
				t.Fatal(err)
			}
			if got := p.Models.Snapshot().Registered(p.Config.Snapshot()); len(got) != 0 {
				t.Fatalf("denylist did not override allowlist: %+v", got)
			}
			for _, alias := range aliases {
				if route := p.routeModel(regressionJSON(t, pluginapi.ModelRouteRequest{RequestedModel: alias})); route.Handled {
					t.Fatalf("denied alias was routed: %s", alias)
				}
				regressionBadRequest(t, p, pluginapi.ExecutorRequest{Model: alias, Payload: []byte(`{"messages":[]}`)})
			}
		})
	}
}

func TestPublicModelIDsRejectShortNameCollisions(t *testing.T) {
	for _, ids := range [][]string{
		{"one/shared", "two/shared"},
		{"one/Shared", "two/SHARED"},
		{"shared", "two/shared"},
		{"one/"},
	} {
		t.Run(strings.Join(ids, "+"), func(t *testing.T) {
			var models []remoteModel
			var defs []modelDef
			for _, id := range ids {
				models = append(models, remoteModel{ID: id})
				defs = append(defs, modelDef{id: id})
			}
			if _, _, err := parseModelsFile(regressionJSON(t, remoteModelFile{SchemaVersion: modelsSchemaVersion, Models: models})); err == nil {
				t.Fatal("invalid public names accepted from catalog")
			}
			// Compiled snapshots must not advertise an ambiguous or empty ID,
			// even when a filter happens to allow just one conflicting model.
			snapshot := newModelSnapshot(defs)
			for _, cfg := range []pluginConfig{{}, {Models: []string{ids[0]}}} {
				if got := snapshot.Registered(cfg); len(got) != 0 {
					t.Fatalf("invalid public names advertised: %+v", got)
				}
			}
		})
	}
}
