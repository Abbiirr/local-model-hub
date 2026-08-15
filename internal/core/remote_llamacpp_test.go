package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/localhub/localhub/internal/config"
)

const remoteTestUpstreamModel = "/models/kat-coder-v2.5.gguf"

func remoteLlamaCPPTestServer(t *testing.T, advertisedModel string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/v1/models":
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"` + advertisedModel + `"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestResolveForProxy_RemoteLlamaCPPUsesStableAliasAndUpstreamIdentity(t *testing.T) {
	remote := remoteLlamaCPPTestServer(t, remoteTestUpstreamModel)
	defer remote.Close()
	cfg := config.Default()
	cfg.Engines.LlamaCPP.RemoteModels = []config.RemoteLlamaCPPModelConfig{{
		ID: "kat", BaseURL: remote.URL, UpstreamModel: remoteTestUpstreamModel,
	}}
	svc := New(Deps{Cfg: cfg})

	handle, block, err := svc.ResolveForProxy(context.Background(), "kat", 1)
	if err != nil {
		t.Fatalf("ResolveForProxy(remote): %v", err)
	}
	if block != nil {
		t.Fatalf("configured healthy remote must not admission-block: %s", block.Reason)
	}
	if handle == nil {
		t.Fatal("configured healthy remote must return a relay handle")
	}
	defer handle.Done()
	if handle.TargetBaseURL != remote.URL {
		t.Errorf("TargetBaseURL = %q, want %q", handle.TargetBaseURL, remote.URL)
	}
	if handle.UpstreamModel != remoteTestUpstreamModel {
		t.Errorf("UpstreamModel = %q, want exact remote identity %q", handle.UpstreamModel, remoteTestUpstreamModel)
	}
	if handle.InstanceID != "remote-llamacpp:kat" {
		t.Errorf("InstanceID = %q", handle.InstanceID)
	}
}

func TestResolveForProxy_RemoteLlamaCPPRefusesIdentityMismatch(t *testing.T) {
	remote := remoteLlamaCPPTestServer(t, "/models/not-kat.gguf")
	defer remote.Close()
	cfg := config.Default()
	cfg.Engines.LlamaCPP.RemoteModels = []config.RemoteLlamaCPPModelConfig{{
		ID: "kat", BaseURL: remote.URL, UpstreamModel: remoteTestUpstreamModel,
	}}
	svc := New(Deps{Cfg: cfg})

	handle, block, err := svc.ResolveForProxy(context.Background(), "kat", 1)
	if err == nil {
		t.Fatal("identity mismatch must fail closed")
	}
	if handle != nil || block != nil {
		t.Fatalf("identity mismatch returned handle=%v block=%v", handle, block)
	}
}

func TestResolveForEmbeddings_RemoteGenerationServerReturnsModeBlock(t *testing.T) {
	remote := remoteLlamaCPPTestServer(t, remoteTestUpstreamModel)
	defer remote.Close()
	cfg := config.Default()
	cfg.Engines.LlamaCPP.RemoteModels = []config.RemoteLlamaCPPModelConfig{{
		ID: "kat", BaseURL: remote.URL, UpstreamModel: remoteTestUpstreamModel,
	}}
	svc := New(Deps{Cfg: cfg})

	handle, block, err := svc.ResolveForEmbeddings(context.Background(), "kat", 1)
	if err != nil {
		t.Fatalf("mode incompatibility must be a deterministic block, not a transport error: %v", err)
	}
	if handle != nil {
		t.Fatal("generation-only remote must not be returned for embeddings")
	}
	if block == nil {
		t.Fatal("expected a mode-mismatch block")
	}
	if block.Kind != ProxyBlockModeMismatch {
		t.Fatalf("block kind = %q, want %q; reason=%s", block.Kind, ProxyBlockModeMismatch, block.Reason)
	}
}

func TestOpenAIModels_IncludesOnlyVerifiedRemoteLlamaCPP(t *testing.T) {
	remote := remoteLlamaCPPTestServer(t, remoteTestUpstreamModel)
	defer remote.Close()
	d := coreTestDB(t)
	cfg := config.Default()
	cfg.Engines.LlamaCPP.RemoteModels = []config.RemoteLlamaCPPModelConfig{
		{ID: "kat", BaseURL: remote.URL, UpstreamModel: remoteTestUpstreamModel},
		{ID: "wrong-remote", BaseURL: remote.URL, UpstreamModel: "/models/wrong.gguf"},
	}
	svc := New(Deps{Cfg: cfg, DB: d})
	models, err := svc.OpenAIModels(context.Background())
	if err != nil {
		t.Fatalf("OpenAIModels: %v", err)
	}
	got := map[string]bool{}
	for _, model := range models {
		got[model.ID] = true
	}
	if !got["kat"] {
		t.Error("verified remote model missing from /v1/models projection")
	}
	if got["wrong-remote"] {
		t.Error("identity-mismatched remote must not be advertised")
	}
}
