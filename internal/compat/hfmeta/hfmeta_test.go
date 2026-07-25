package hfmeta

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetch_ConfigAndIndex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/config.json"):
			w.Write([]byte(`{"num_hidden_layers":32,"hidden_size":4096}`))
		case strings.HasSuffix(r.URL.Path, "/model.safetensors.index.json"):
			w.Write([]byte(`{"metadata":{"total_size":16000000000}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	old := hfBase
	hfBase = srv.URL
	defer func() { hfBase = old }()

	c := &Client{}
	res, err := c.Fetch(context.Background(), "meta-llama/Llama-3.1-8B-Instruct", "main")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !strings.Contains(string(res.ConfigJSON), "num_hidden_layers") {
		t.Errorf("config missing content: %s", res.ConfigJSON)
	}
	if !strings.Contains(string(res.IndexJSON), "total_size") {
		t.Errorf("index missing content: %s", res.IndexJSON)
	}
}

func TestFetch_NoIndex_NotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/config.json") {
			w.Write([]byte(`{"num_hidden_layers":24}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	old := hfBase
	hfBase = srv.URL
	defer func() { hfBase = old }()

	res, err := (&Client{}).Fetch(context.Background(), "owner/model", "")
	if err != nil {
		t.Fatalf("Fetch with no index should succeed: %v", err)
	}
	if res.IndexJSON != nil {
		t.Errorf("IndexJSON should be nil when index is absent, got %s", res.IndexJSON)
	}
}

func TestFetch_MissingConfig_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	old := hfBase
	hfBase = srv.URL
	defer func() { hfBase = old }()

	_, err := (&Client{}).Fetch(context.Background(), "owner/model", "")
	if err == nil {
		t.Fatal("missing config.json must error")
	}
	if !strings.Contains(err.Error(), "config.json not found") {
		t.Errorf("error should explain missing config: %v", err)
	}
}

func TestFetch_SizeCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/config.json") {
			big := strings.Repeat("a", MaxBodyBytes+10)
			w.Write([]byte(`{"x":"` + big + `"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	old := hfBase
	hfBase = srv.URL
	defer func() { hfBase = old }()

	_, err := (&Client{}).Fetch(context.Background(), "owner/model", "")
	if err == nil || !strings.Contains(err.Error(), "cap") {
		t.Fatalf("oversized body must be refused by the cap, got: %v", err)
	}
}

func TestFetch_GatedRepo_Message(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	old := hfBase
	hfBase = srv.URL
	defer func() { hfBase = old }()

	_, err := (&Client{}).Fetch(context.Background(), "meta-llama/Llama-3.1-8B-Instruct", "")
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "gated") {
		t.Fatalf("403 should surface a gated-repo message, got: %v", err)
	}
}

func TestFetch_InvalidRepo(t *testing.T) {
	_, err := (&Client{}).Fetch(context.Background(), "not-a-valid-repo", "")
	if err == nil {
		t.Fatal("repo without owner/name must be rejected")
	}
}
