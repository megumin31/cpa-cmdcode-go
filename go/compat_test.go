package main

// Legacy regression helpers route through the real components. Production
// has one composition root and no package-level mutable config/registry state.
import (
	"bytes"
	"context"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var defaultPlugin = NewPlugin(nil, nil)

func getConfig() pluginConfig { return defaultPlugin.Config.Snapshot() }
func setConfig(c pluginConfig) {
	defaultPlugin.stateMu.Lock()
	defer defaultPlugin.stateMu.Unlock()
	defaultPlugin.Config.mu.Lock()
	defaultPlugin.Config.value = cloneConfig(c)
	defaultPlugin.Config.mu.Unlock()
	defaultPlugin.Models.Configure(c)
}
func configure(b []byte)                              { _ = defaultPlugin.Configure(b) }
func handleMethod(m string, b []byte) ([]byte, error) { return defaultPlugin.Handle(m, b) }
func activeModelTable() []modelDef                    { return defaultPlugin.Models.Snapshot().defs }
func registeredModels() []pluginapi.ModelInfo {
	s := defaultPlugin.snapshot()
	return s.models.Registered(s.config)
}
func matchModel(n string) (string, bool)       { return defaultPlugin.Models.Snapshot().Match(n) }
func modelAllowed(n string) bool               { s := defaultPlugin.snapshot(); return s.models.Allowed(n, s.config) }
func resolveAPIKey(a map[string]string) string { return getConfig().resolveAPIKey(a) }
func resolveBaseURL() string                   { return getConfig().resolveBaseURL() }
func cliVersion() string                       { return getConfig().cliVersion() }
func projectSlug() string                      { return getConfig().projectSlug() }
func permissionMode() string                   { return getConfig().permissionMode() }
func maybeRefreshModels()                      { _ = defaultPlugin.Models.Refresh(context.Background()) }
func modelOutputCap(n string) int {
	d, _ := defaultPlugin.Models.Snapshot().Definition(n)
	return int(d.output)
}
func buildEnvelope(n string, b []byte) ([]byte, error) {
	return buildEnvelopeWithSnapshot(n, b, defaultPlugin.snapshot())
}
func parseStream(b []byte) (*accumulated, error) {
	d := NewEventDecoder("", false)
	err := d.Consume(bytes.NewReader(b), nil)
	if err != nil {
		return nil, err
	}
	return &d.acc, nil
}

type liveStream = EventDecoder

func newLiveStream(n string) *EventDecoder { return NewEventDecoder(n, true) }
