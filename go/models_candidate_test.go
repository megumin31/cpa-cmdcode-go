package main

import (
	"os"
	"testing"
)

// The data publisher validates a candidate through the real runtime parser,
// without rebuilding/releasing the plugin or changing the pinned fallback.
func TestPublishedModelCandidate(t *testing.T) {
	path := os.Getenv("MODELS_CANDIDATE_PATH")
	if path == "" {
		path = "../data/models.snapshot.json"
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	raw, err := readBounded(f, maxRosterBytes)
	if err != nil {
		t.Fatal(err)
	}
	defs, _, err := parseModelsFile(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(newModelSnapshot(defs).Registered(pluginConfig{})); got != len(defs) {
		t.Fatalf("candidate registers %d of %d models", got, len(defs))
	}
}
