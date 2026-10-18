package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	modelsSchemaVersion          = 1
	defaultModelsURL             = "https://raw.githubusercontent.com/megumin31/cpa-cmdcode-go/main/models.json"
	defaultRefreshInterval       = 6 * time.Hour
	modelsFetchTimeout           = 8 * time.Second
	modelsMinRetryInterval       = 5 * time.Minute
	maxRosterBytes         int64 = 4 << 20
)

type remoteModelFile struct {
	Source           string        `json:"source"`
	SchemaVersion    int           `json:"schema_version"`
	SourceCLIVersion string        `json:"source_cli_version"`
	Models           []remoteModel `json:"models"`
}
type remoteModel struct {
	ID            string   `json:"id"`
	Display       string   `json:"display"`
	Description   string   `json:"description"`
	Context       int64    `json:"context"`
	Output        int64    `json:"output"`
	OutputSource  string   `json:"output_source"`
	ContextSource string   `json:"context_source"`
	Vision        *bool    `json:"vision"`
	Efforts       []string `json:"reasoning_efforts"`
	GatewayOutput int64    `json:"gateway_output_limit"`
}

// ModelRegistry owns refresh state and immutable snapshots. One refresh per
// configuration generation; stale downloads cannot publish after reconfigure.
type ModelRegistry struct {
	mu                         sync.Mutex
	fallback                   *modelSnapshot
	current                    *modelSnapshot
	cfg                        pluginConfig
	generation                 uint64
	loading                    bool
	cancel                     context.CancelFunc
	attempted, fetched, fileMT time.Time
	failed                     bool
	client                     *http.Client
	now                        func() time.Time
}

func NewModelRegistry(defs []modelDef, client *http.Client) *ModelRegistry {
	if client == nil {
		client = &http.Client{Timeout: modelsFetchTimeout}
	}
	s := newModelSnapshot(defs)
	return &ModelRegistry{fallback: s, current: s, client: client, now: time.Now}
}

func modelsRefreshInterval(cfg pluginConfig) (time.Duration, bool) {
	if cfg.ModelsRefreshInterval == "" {
		return defaultRefreshInterval, true
	}
	d, err := time.ParseDuration(cfg.ModelsRefreshInterval)
	if err != nil {
		return defaultRefreshInterval, true
	}
	return d, d > 0
}

func (r *ModelRegistry) Configure(cfg pluginConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cfg.ModelsURL == cfg.ModelsURL && r.cfg.ModelsFile == cfg.ModelsFile && r.cfg.ModelsRefreshInterval == cfg.ModelsRefreshInterval {
		return
	}
	r.generation++
	if r.cancel != nil {
		r.cancel()
	}
	r.cancel, r.loading = nil, false
	r.cfg = cloneConfig(cfg)
	r.attempted, r.fetched, r.fileMT = time.Time{}, time.Time{}, time.Time{}
	r.failed = false
	if _, enabled := modelsRefreshInterval(cfg); !enabled {
		r.current = r.fallback
	}
}

func (r *ModelRegistry) Snapshot() *modelSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.current
}

// Refresh returns the last good snapshot on any failure. Concurrent callers
// do not wait for or start duplicate downloads. Initial failures are throttled.
func (r *ModelRegistry) Refresh(ctx context.Context) error {
	r.mu.Lock()
	cfg := r.cfg
	interval, enabled := modelsRefreshInterval(cfg)
	now := r.now()
	if !enabled || r.loading || (r.failed && now.Sub(r.attempted) < modelsMinRetryInterval) {
		r.mu.Unlock()
		return nil
	}
	var mt time.Time
	if cfg.ModelsFile != "" {
		fi, err := os.Stat(cfg.ModelsFile)
		if err != nil {
			r.attempted, r.failed = now, true
			r.mu.Unlock()
			return err
		}
		mt = fi.ModTime()
		if !r.fetched.IsZero() && mt.Equal(r.fileMT) {
			r.mu.Unlock()
			return nil
		}
	} else if !r.fetched.IsZero() && now.Sub(r.fetched) < interval {
		r.mu.Unlock()
		return nil
	}
	generation := r.generation
	requestCtx, cancel := context.WithTimeout(ctx, modelsFetchTimeout)
	r.loading, r.cancel, r.attempted = true, cancel, now
	r.mu.Unlock()
	defer cancel()
	raw, err := r.fetch(requestCtx, cfg)
	var defs []modelDef
	if err == nil {
		defs, _, err = parseModelsFile(raw)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if generation != r.generation {
		return nil
	}
	r.loading, r.cancel = false, nil
	if requestCtx.Err() != nil {
		err = requestCtx.Err()
	}
	r.failed = err != nil
	if err != nil {
		return err
	}
	r.current = newModelSnapshot(defs)
	r.fetched, r.fileMT = r.now(), mt
	return nil
}

func (r *ModelRegistry) fetch(ctx context.Context, cfg pluginConfig) ([]byte, error) {
	var reader io.ReadCloser
	if cfg.ModelsFile != "" {
		f, err := os.Open(cfg.ModelsFile)
		if err != nil {
			return nil, err
		}
		reader = f
	} else {
		url := cfg.ModelsURL
		if url == "" {
			url = defaultModelsURL
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		resp, err := r.client.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("model list HTTP %d", resp.StatusCode)
		}
		reader = resp.Body
	}
	defer reader.Close()
	return readBounded(reader, maxRosterBytes)
}

func parseModelsFile(raw []byte) ([]modelDef, string, error) {
	var doc remoteModelFile
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, "", fmt.Errorf("invalid models JSON: %w", err)
	}
	if doc.SchemaVersion != modelsSchemaVersion {
		return nil, "", fmt.Errorf("unsupported models schema %d", doc.SchemaVersion)
	}
	if len(doc.Models) == 0 || len(doc.Models) > 300 {
		return nil, "", fmt.Errorf("invalid model count %d", len(doc.Models))
	}
	defs := make([]modelDef, 0, len(doc.Models))
	seen, shorts := map[string]bool{}, map[string]bool{}
	for _, m := range doc.Models {
		id := strings.TrimSpace(m.ID)
		id = strings.TrimPrefix(id, "cpa-cmdcode-go/")
		id = strings.TrimPrefix(id, ProviderKey+"/")
		short := strings.ToLower(shortModelName(id))
		if short == "" || strings.ContainsAny(id, " \t\r\n") || seen[strings.ToLower(id)] || shorts[short] {
			return nil, "", fmt.Errorf("empty, invalid or duplicate model ID/short name: %q", id)
		}
		seen[strings.ToLower(id)], shorts[short] = true, true
		if m.Context < 0 || m.Output < 0 || m.GatewayOutput < 0 {
			return nil, "", fmt.Errorf("negative model budget: %s", id)
		}
		// Zero means unknown metadata, not a model limit or request default.
		// Validate against context only when that reference is known.
		if m.Context > 0 && m.ContextSource != "fallback" && (m.Output > m.Context || m.GatewayOutput > m.Context) {
			return nil, "", fmt.Errorf("output exceeds context: %s", id)
		}
		if m.Display == "" {
			m.Display = id
		}
		defs = append(defs, modelDef{
			id: id, display: m.Display, description: m.Description, context: m.Context, contextSource: m.ContextSource,
			output: m.Output, outputSource: m.OutputSource, vision: m.Vision,
			efforts: append([]string(nil), m.Efforts...), gatewayOutput: m.GatewayOutput,
		})
	}
	return defs, doc.SourceCLIVersion, nil
}
