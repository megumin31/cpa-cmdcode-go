package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRegistryDataBranchDefaultURL(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://raw.githubusercontent.com/megumin31/cpa-cmdcode-go/models-data/models.json" {
			t.Fatalf("wrong default model source: %s", req.URL)
		}
		return fixtureResponse(remoteFixture("")), nil
	})}
	if err := NewModelRegistry(modelTable, client).Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryConditionalRefreshAndTTL(t *testing.T) {
	calls := 0
	modified := "Thu, 05 Nov 2026 10:00:00 GMT"
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		response := fixtureResponse(remoteFixture(""))
		switch calls {
		case 1:
			if req.Header.Get("If-None-Match") != "" || req.Header.Get("If-Modified-Since") != "" {
				t.Fatal("initial request had validators")
			}
			response.Header.Set("ETag", `"first"`)
			response.Header.Set("Last-Modified", modified)
		case 2, 3:
			if req.Header.Get("If-None-Match") != `"first"` || req.Header.Get("If-Modified-Since") != modified {
				t.Fatalf("missing conditional headers: %v", req.Header)
			}
			if calls == 2 {
				response.StatusCode = http.StatusNotModified
				response.Body = io.NopCloser(strings.NewReader(""))
			}
		case 4:
			if req.Header.Get("If-None-Match") != "" || req.Header.Get("If-Modified-Since") != "" {
				t.Fatal("a new 200 without validators retained old validators")
			}
		}
		return response, nil
	})}
	r := NewModelRegistry(modelTable, client)
	now := time.Now()
	r.now = func() time.Time { return now }
	refresh := func() {
		t.Helper()
		if err := r.Refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	refresh()
	snapshot := r.Snapshot()
	refresh()
	if calls != 1 {
		t.Fatal("TTL did not avoid network")
	}
	now = now.Add(defaultRefreshInterval)
	refresh()
	if r.Snapshot() != snapshot || calls != 2 {
		t.Fatal("304 replaced snapshot or failed conditional request")
	}
	refresh()
	if calls != 2 {
		t.Fatal("304 did not renew TTL")
	}
	now = now.Add(defaultRefreshInterval)
	refresh()
	now = now.Add(defaultRefreshInterval)
	refresh()
}

func TestRegistryRejectsUnsolicitedNotModified(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		response := fixtureResponse("")
		response.StatusCode = http.StatusNotModified
		return response, nil
	})}
	r := NewModelRegistry(modelTable, client)
	snapshot := r.Snapshot()
	if err := r.Refresh(context.Background()); err == nil || r.Snapshot() != snapshot {
		t.Fatal("304 without a validated cache must fail and retain fallback")
	}
}

func TestRegistryInvalidResponseCannotPoisonValidators(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		response := fixtureResponse(remoteFixture(""))
		switch calls {
		case 1:
			response.Header.Set("ETag", `"good"`)
		case 2:
			response.Body = io.NopCloser(strings.NewReader("{invalid"))
			response.Header.Set("ETag", `"bad"`)
		case 3:
			if req.Header.Get("If-None-Match") != `"good"` {
				t.Fatal("invalid response poisoned cache validators")
			}
			response.StatusCode = http.StatusNotModified
		}
		return response, nil
	})}
	r := NewModelRegistry(modelTable, client)
	now := time.Now()
	r.now = func() time.Time { return now }
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot := r.Snapshot()
	now = now.Add(defaultRefreshInterval)
	if err := r.Refresh(context.Background()); err == nil || r.Snapshot() != snapshot {
		t.Fatal("invalid response discarded last good snapshot")
	}
	_ = r.Refresh(context.Background())
	if calls != 2 {
		t.Fatal("failed refresh was not throttled")
	}
	now = now.Add(modelsMinRetryInterval)
	if err := r.Refresh(context.Background()); err != nil || r.Snapshot() != snapshot {
		t.Fatal("last valid conditional response was not accepted", err)
	}
}

func TestRegistryLastModifiedAndReconfigureIsolation(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		response := fixtureResponse(remoteFixture(""))
		if calls == 2 {
			if req.Header.Get("If-Modified-Since") == "" || req.Header.Get("If-None-Match") != "" {
				t.Fatal("Last-Modified-only caching failed")
			}
			response.StatusCode = http.StatusNotModified
		} else if calls == 3 {
			if req.Header.Get("If-Modified-Since") != "" || req.Header.Get("If-None-Match") != "" {
				t.Fatal("validators leaked across reconfiguration")
			}
		}
		response.Header.Set("Last-Modified", "Thu, 05 Nov 2026 10:00:00 GMT")
		return response, nil
	})}
	r := NewModelRegistry(modelTable, client)
	now := time.Now()
	r.now = func() time.Time { return now }
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(defaultRefreshInterval)
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.Configure(pluginConfig{ModelsURL: "https://example.invalid/new-source"})
	if err := r.Refresh(context.Background()); err != nil || calls != 3 {
		t.Fatal("reconfigure failed to fetch new source", err)
	}
}
