package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/localhub/localhub/internal/startupconfig"
)

// handleStartupGet returns the durable Windows tray startup intent. On first use the store creates
// a documented safe default, so the GET response and configs/startup.json never disagree.
func (s *Server) handleStartupGet(w http.ResponseWriter, _ *http.Request) {
	cfg, err := startupconfig.LoadStartupConfig(s.startupRoot)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "startup_config_read_failed",
			"could not read startup behavior", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

// handleStartupPut validates and atomically saves the complete startup policy. It changes intent
// for the next tray launch/health cycle; it never starts or stops a model inside this request.
func (s *Server) handleStartupPut(w http.ResponseWriter, r *http.Request) {
	if !requireJSONBody(w, r) {
		return
	}
	decoder := json.NewDecoder(io.LimitReader(r.Body, 2<<20))
	decoder.DisallowUnknownFields()
	var cfg startupconfig.StartupConfig
	if err := decoder.Decode(&cfg); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "could not parse startup behavior", err.Error())
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		reason := "the request must contain exactly one JSON object"
		if err != nil {
			reason = err.Error()
		}
		writeError(w, http.StatusBadRequest, "bad_request", "could not parse startup behavior", reason)
		return
	}
	// Upgrade before validating so a dashboard or tray that has not been reloaded since the v1
	// schema still saves successfully instead of being rejected for its version alone.
	if _, err := cfg.UpgradeToCurrent(); err != nil {
		writeError(w, http.StatusBadRequest, "startup_config_invalid",
			"startup behavior is invalid", err.Error())
		return
	}
	if err := cfg.ValidateAndNormalize(); err != nil {
		writeError(w, http.StatusBadRequest, "startup_config_invalid",
			"startup behavior is invalid", err.Error())
		return
	}
	if err := startupconfig.SaveStartupConfig(s.startupRoot, cfg); err != nil {
		writeError(w, http.StatusInternalServerError, "startup_config_write_failed",
			"could not save startup behavior", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}
