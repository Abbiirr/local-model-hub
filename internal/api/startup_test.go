package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/localhub/localhub/internal/startupconfig"
)

func TestStartupConfigGetCreatesDefault(t *testing.T) {
	s := &Server{startupRoot: t.TempDir()}
	recorder := httptest.NewRecorder()
	s.handleStartupGet(recorder, httptest.NewRequest(http.MethodGet, "/api/startup", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var got startupconfig.StartupConfig
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	// The shipped default starts the control plane and no models at all.
	if !got.StartLocalHub || len(got.Models) != 0 || got.RestoreLastSession {
		t.Fatalf("unexpected default: %+v", got)
	}
	if got.Version != startupconfig.StartupConfigVersion {
		t.Fatalf("version = %d, want %d", got.Version, startupconfig.StartupConfigVersion)
	}
}

func TestStartupConfigPutValidatesAndPersists(t *testing.T) {
	root := t.TempDir()
	s := &Server{startupRoot: root}
	body := []byte(`{"version":2,"start_localhub":true,"open_web_ui":true,"restore_last_session":false,"startup_delay_seconds":15,"health_check_seconds":30,"models":[]}`)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/api/startup", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	s.handleStartupPut(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	got, err := startupconfig.LoadStartupConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	if !got.OpenWebUI || got.StartupDelaySeconds != 15 || len(got.Models) != 0 {
		t.Fatalf("persisted config = %+v", got)
	}
}

// A dashboard left open across the upgrade still submits v1. Migrate it rather than answering 400
// for the version alone, and drop the canonical_qwen entry it may carry.
func TestStartupConfigPutUpgradesLegacyBody(t *testing.T) {
	root := t.TempDir()
	s := &Server{startupRoot: root}
	body := []byte(`{"version":1,"start_localhub":true,"open_web_ui":false,"restore_last_session":false,` +
		`"startup_delay_seconds":0,"health_check_seconds":60,"models":[` +
		`{"model_id":"gguf-qwen3-6-27b-16gb-vram-mtp-mini-iq4-xs","enabled":true,"order":0,` +
		`"start_delay_seconds":0,"retry_policy":"keep_available","max_attempts":0,` +
		`"use_saved_profile":false,"mode":"canonical_qwen"}]}`)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/api/startup", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	s.handleStartupPut(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	got, err := startupconfig.LoadStartupConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != startupconfig.StartupConfigVersion || len(got.Models) != 0 {
		t.Fatalf("persisted config = %+v, want v%d with no models", got, startupconfig.StartupConfigVersion)
	}
}

func TestStartupConfigPutRejectsInvalidPolicyWithReason(t *testing.T) {
	s := &Server{startupRoot: t.TempDir()}
	body := []byte(`{"version":1,"health_check_seconds":0,"models":[]}`)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/api/startup", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	s.handleStartupPut(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte(`"reason"`)) {
		t.Fatalf("error omitted reason: %s", recorder.Body.String())
	}
}
