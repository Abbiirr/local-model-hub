package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/localhub/localhub/internal/config"
)

const remoteLlamaCPPProbeTimeout = 3 * time.Second

var remoteLlamaCPPProbeClient = &http.Client{
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return fmt.Errorf("redirects are not allowed for configured remote llama.cpp identity probes")
	},
}

// remoteLlamaCPPModel returns the fixed admin-configured remote whose stable alias matches id.
// Client input never selects a URL, so this does not create an open proxy or SSRF surface.
func (s *Service) remoteLlamaCPPModel(id string) (config.RemoteLlamaCPPModelConfig, bool) {
	for _, model := range s.deps.Cfg.Engines.LlamaCPP.RemoteModels {
		if model.ID == id {
			return model, true
		}
	}
	return config.RemoteLlamaCPPModelConfig{}, false
}

// availableRemoteLlamaCPPModels returns only remotes whose health AND served-model identity are
// currently proven. A stale config must not be advertised as runnable by GET /v1/models.
func (s *Service) availableRemoteLlamaCPPModels(ctx context.Context) []config.RemoteLlamaCPPModelConfig {
	var out []config.RemoteLlamaCPPModelConfig
	for _, model := range s.deps.Cfg.Engines.LlamaCPP.RemoteModels {
		if err := s.verifyRemoteLlamaCPPModel(ctx, model); err == nil {
			out = append(out, model)
		}
	}
	return out
}

// verifyRemoteLlamaCPPModel proves health and exact served-model identity before routing.
func (s *Service) verifyRemoteLlamaCPPModel(ctx context.Context, model config.RemoteLlamaCPPModelConfig) error {
	probeCtx, cancel := context.WithTimeout(ctx, remoteLlamaCPPProbeTimeout)
	defer cancel()

	var health struct {
		Status string `json:"status"`
	}
	if err := getRemoteLlamaCPPJSON(probeCtx, model.BaseURL+"/health", &health); err != nil {
		return fmt.Errorf("health probe %s/health failed: %w", model.BaseURL, err)
	}
	if health.Status != "ok" {
		return fmt.Errorf("health probe returned status %q, want %q", health.Status, "ok")
	}

	var listed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Models []struct {
			Name  string `json:"name"`
			Model string `json:"model"`
		} `json:"models"`
	}
	if err := getRemoteLlamaCPPJSON(probeCtx, model.BaseURL+"/v1/models", &listed); err != nil {
		return fmt.Errorf("model identity probe %s/v1/models failed: %w", model.BaseURL, err)
	}
	advertised := make(map[string]bool)
	for _, item := range listed.Data {
		advertised[item.ID] = true
	}
	for _, item := range listed.Models {
		advertised[item.Name] = true
		advertised[item.Model] = true
	}
	if advertised[model.UpstreamModel] {
		return nil
	}
	names := make([]string, 0, len(advertised))
	for name := range advertised {
		if strings.TrimSpace(name) != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return fmt.Errorf("served-model identity mismatch: expected %q, advertised %v", model.UpstreamModel, names)
}

func getRemoteLlamaCPPJSON(ctx context.Context, target string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	resp, err := remoteLlamaCPPProbeClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	return nil
}
