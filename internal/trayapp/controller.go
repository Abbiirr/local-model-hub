// Package trayapp implements the testable lifecycle operations used by the Windows tray UI.
package trayapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/localhub/localhub/internal/startupconfig"
	"github.com/localhub/localhub/internal/winproc"
)

const (
	DefaultWebURL       = "http://127.0.0.1:39876"
	defaultHealthWindow = 3 * time.Second

	// daemonStartTimeout bounds how long StartLocalHub waits for a freshly spawned daemon to
	// answer /api/health. It is a ceiling, not a delay: the poll returns the moment the daemon
	// is up. A caller's ctx deadline still wins if it is shorter.
	daemonStartTimeout = 90 * time.Second
	daemonPollInterval = 250 * time.Millisecond
	daemonPollMax      = 2 * time.Second

	// daemonStopTimeout bounds the wait for a tray-owned daemon to exit during shutdown.
	daemonStopTimeout = 30 * time.Second
)

// commandRunner is the process-spawn seam. It exists so tests can assert what the tray would
// launch without launching it.
type commandRunner interface {
	// Start launches a fire-and-forget helper (the URL handler) and does not wait for it.
	Start(name string, args ...string) error
	// StartIn launches a long-lived detached child in dir and returns its handle so the caller
	// can stop what it started. The child gets no console window and no inherited pipes.
	StartIn(dir, name string, args ...string) (*os.Process, error)
}

type execRunner struct{}

// hiddenCommand builds a child process that never allocates a console window.
//
// The tray is linked -H=windowsgui and therefore has no console of its own, which makes this
// mandatory rather than cosmetic: when a GUI-subsystem parent spawns a console child, Windows
// allocates a brand new console window for it — visible on screen and closable by anyone.
func hiddenCommand(dir, name string, args ...string) *exec.Cmd {
	command := exec.Command(name, args...)
	command.Dir = dir
	winproc.Hide(command)
	return command
}

func (execRunner) Start(name string, args ...string) error {
	return hiddenCommand("", name, args...).Start()
}

func (execRunner) StartIn(dir, name string, args ...string) (*os.Process, error) {
	command := hiddenCommand(dir, name, args...)
	// Stdio is left nil deliberately. Go then wires the child to NUL instead of to pipes owned
	// by this process — which is what makes the child survivable and, more importantly, is why
	// the tray can no longer wedge: the previous implementation ran the daemon through
	// powershell.exe and read the result with CombinedOutput, so cmd.Wait blocked until every
	// inherited copy of the pipe handle closed. The daemon never exits, so it never returned,
	// and the tray sat on "Starting LocalHub..." with the whole menu disabled.
	winproc.Hide(command)
	if err := command.Start(); err != nil {
		return nil, err
	}
	// The child is detached: nothing waits on it, and it must outlive this call.
	return command.Process, nil
}

// Status is the live state displayed in the tray menu.
type Status struct {
	LocalHubHealthy bool
}

// AvailableModel is the small inventory projection needed by the native tray menu.
type AvailableModel struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Format      string `json:"format"`
	LocalPath   string `json:"local_path"`
	Presence    string `json:"presence"`
}

// StartupResult records one startup attempt with a reason suitable for the tray status line.
type StartupResult struct {
	ModelID string
	Started bool
	Reason  string
}

// Controller supervises the LocalHub daemon directly from Go.
//
// It used to drive scripts/keep-localhub-qwen.ps1 through powershell.exe. That script is still a
// supported standalone tool, but the tray no longer calls it: shelling out could not report
// progress, spawned visible console windows, and — because the daemon inherited the pipe the tray
// was reading — could block forever even after the daemon had started successfully.
type Controller struct {
	repoRoot string
	webURL   string
	runner   commandRunner
	client   *http.Client

	// daemon is the direct process handle when this tray started the daemon. Shutdown can also
	// stop an earlier LocalHub daemon after verifying its health-reported PID and executable path.
	daemonMu   sync.Mutex
	daemon     *os.Process
	terminator func(pid int, allowedPaths []string) error
}

func New(repoRoot, webURL string) (*Controller, error) {
	if repoRoot == "" {
		return nil, fmt.Errorf("repository root is required")
	}
	absoluteRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve repository root: %w", err)
	}
	if webURL == "" {
		webURL = DefaultWebURL
	}
	return &Controller{
		repoRoot:   absoluteRoot,
		webURL:     strings.TrimRight(webURL, "/"),
		runner:     execRunner{},
		client:     &http.Client{Timeout: defaultHealthWindow},
		terminator: winproc.TerminateVerified,
	}, nil
}

// DaemonCandidates lists, in preference order, where the daemon executable may live relative to
// the repository root. bin/ is scripts/dev.ps1's documented output path; the repo root is what a
// plain `go build ./...` produces.
func (c *Controller) DaemonCandidates() []string {
	names := []string{"localhub.exe", "localhub"}
	paths := make([]string, 0, len(names)*2)
	for _, name := range names {
		paths = append(paths, filepath.Join(c.repoRoot, "bin", name))
	}
	for _, name := range names {
		paths = append(paths, filepath.Join(c.repoRoot, name))
	}
	return paths
}

func (c *Controller) daemonExecutable() (string, error) {
	candidates := c.DaemonCandidates()
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("LocalHub daemon executable not found; looked in %s", strings.Join(candidates, ", "))
}

// StartLocalHub makes the control plane healthy and returns.
//
// It checks health FIRST and returns immediately if the daemon is already answering, whether this
// tray started it or not. That check is the difference between a tray that reattaches to a running
// system and one that tries to start a second daemon on an occupied port and then reports failure
// (or, historically, hangs) — which is exactly what left the menu permanently greyed out.
func (c *Controller) StartLocalHub(ctx context.Context) error {
	if c.healthy(ctx, c.healthURL()) {
		return nil
	}
	executable, err := c.daemonExecutable()
	if err != nil {
		return err
	}
	process, err := c.runner.StartIn(c.repoRoot, executable, "-config", "configs/config.yaml")
	if err != nil {
		return fmt.Errorf("start LocalHub daemon %s: %w", executable, err)
	}
	c.daemonMu.Lock()
	c.daemon = process
	c.daemonMu.Unlock()

	if err := c.waitHealthy(ctx, daemonStartTimeout); err != nil {
		return fmt.Errorf("start LocalHub daemon %s: %w", executable, err)
	}
	return nil
}

// waitHealthy polls /api/health with capped exponential backoff until the daemon answers, the
// caller's context ends, or timeout elapses — whichever comes first. Every exit is bounded, so no
// caller can be parked here indefinitely.
func (c *Controller) waitHealthy(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	wait := daemonPollInterval
	for {
		if c.healthy(ctx, c.healthURL()) {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if remaining := time.Until(deadline); remaining <= 0 {
			return fmt.Errorf("daemon did not answer %s within %s", c.healthURL(), timeout)
		} else if remaining < wait {
			wait = remaining
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		if wait *= 2; wait > daemonPollMax {
			wait = daemonPollMax
		}
	}
}

// ShutdownAll stops every LocalHub-managed instance through the normal drain-aware API, then stops
// the verified LocalHub daemon answering this tray's health endpoint. Instances are stopped first
// so engine children use the normal NVML-verified teardown. The daemon PID comes from LocalHub and
// its executable path is checked against DaemonCandidates immediately before termination.
func (c *Controller) ShutdownAll(ctx context.Context) error {
	if err := c.stopActiveInstances(ctx); err != nil {
		return err
	}
	pid, pidErr := c.daemonProcessID(ctx)
	if pidErr != nil && c.healthy(ctx, c.healthURL()) {
		return pidErr
	}
	c.daemonMu.Lock()
	process := c.daemon
	c.daemon = nil
	c.daemonMu.Unlock()
	if pid == 0 && process != nil {
		pid = process.Pid
	}
	if pid == 0 {
		return nil
	}
	if err := c.terminator(pid, c.DaemonCandidates()); err != nil {
		// An owned process handle is a safe fallback if the daemon disappeared between the
		// health read and path verification. It is never used for an adopted process.
		if process == nil {
			return err
		}
		if killErr := process.Kill(); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
			return fmt.Errorf("stop LocalHub daemon (pid %d): verified stop failed: %v; owned-process stop failed: %w", pid, err, killErr)
		}
	}
	stopCtx, cancel := context.WithTimeout(ctx, daemonStopTimeout)
	defer cancel()
	for {
		if !c.healthy(stopCtx, c.healthURL()) {
			return nil
		}
		select {
		case <-stopCtx.Done():
			return fmt.Errorf("LocalHub daemon (pid %d) is still answering %s after %s",
				pid, c.healthURL(), daemonStopTimeout)
		case <-time.After(daemonPollInterval):
		}
	}
}

type daemonHealth struct {
	PID int `json:"pid"`
}

func (c *Controller) daemonProcessID(ctx context.Context) (int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.healthURL(), nil)
	if err != nil {
		return 0, err
	}
	response, err := c.client.Do(request)
	if err != nil {
		return 0, nil
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return 0, nil
	}
	var health daemonHealth
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&health); err != nil {
		return 0, fmt.Errorf("identify LocalHub daemon before shutdown: health response did not include a valid pid: %w", err)
	}
	if health.PID <= 0 {
		return 0, fmt.Errorf("identify LocalHub daemon before shutdown: health response reported invalid pid %d", health.PID)
	}
	return health.PID, nil
}

// DaemonOwned reports whether this tray started the running daemon, so the UI can be honest about
// what Exit will and will not stop.
func (c *Controller) DaemonOwned() bool {
	c.daemonMu.Lock()
	defer c.daemonMu.Unlock()
	return c.daemon != nil
}

func (c *Controller) healthURL() string { return c.webURL + "/api/health" }

func (c *Controller) OpenWebUI() error {
	// rundll32 delegates URL handling to the user's configured default browser without shell
	// command parsing. It returns immediately and keeps the tray responsive.
	if err := c.runner.Start("rundll32.exe", "url.dll,FileProtocolHandler", c.webURL); err != nil {
		return fmt.Errorf("open LocalHub web UI: %w", err)
	}
	return nil
}

func (c *Controller) Status(ctx context.Context) Status {
	return Status{LocalHubHealthy: c.healthy(ctx, c.healthURL())}
}

func (c *Controller) LoadStartupConfig() (startupconfig.StartupConfig, error) {
	return startupconfig.LoadStartupConfig(c.repoRoot)
}

func (c *Controller) SaveStartupConfig(cfg startupconfig.StartupConfig) error {
	return startupconfig.SaveStartupConfig(c.repoRoot, cfg)
}

// Models returns the local inventory used to populate per-model startup controls.
func (c *Controller) Models(ctx context.Context) ([]AvailableModel, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.webURL+"/api/models", nil)
	if err != nil {
		return nil, err
	}
	response, err := c.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, apiResponseError("list models", response)
	}
	var models []AvailableModel
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&models); err != nil {
		return nil, fmt.Errorf("decode model inventory: %w", err)
	}
	present := models[:0]
	for _, model := range models {
		if model.Presence == "missing" {
			continue
		}
		present = append(present, model)
	}
	sort.Slice(present, func(i, j int) bool { return present[i].ID < present[j].ID })
	return present, nil
}

// StartConfiguredModel starts one model through LocalHub's normal admission-controlled
// POST /api/instances path — the only launch mechanism there is now that canonical_qwen is gone.
// Admission, VRAM estimation, and the human-readable refusal reason all stay in the daemon.
func (c *Controller) StartConfiguredModel(ctx context.Context, model startupconfig.StartupModelConfig) error {
	if active, err := c.modelActive(ctx, model.ModelID); err == nil && active {
		return nil
	}
	requestBody := make(map[string]any)
	requestBody["model_id"] = model.ModelID
	if model.UseSavedProfile {
		profile, err := c.savedLaunchProfile(ctx, model.ModelID)
		if err != nil {
			return err
		}
		for key, value := range profile {
			requestBody[key] = value
		}
		requestBody["model_id"] = model.ModelID
	}
	mergeConfiguredLaunch(requestBody, model)
	body, err := json.Marshal(requestBody)
	if err != nil {
		return fmt.Errorf("encode startup request for %s: %w", model.ModelID, err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.webURL+"/api/instances", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	// Model admission and cold loading can legitimately take minutes. The shared client has a
	// three-second timeout for health/status probes, so using it here cancels every large startup.
	// The caller's startup/recovery context is the authoritative bounded deadline.
	response, err := (&http.Client{}).Do(request)
	if err != nil {
		return fmt.Errorf("start %s through LocalHub: %w", model.ModelID, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return apiResponseError("start "+model.ModelID, response)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	return nil
}

// RunStartup executes enabled models in normalized order. It returns every result rather than
// stopping after one failure, so the UI can explain partial startup without hiding later models.
func (c *Controller) RunStartup(ctx context.Context, cfg startupconfig.StartupConfig) []StartupResult {
	results := make([]StartupResult, 0, len(cfg.Models))
	for _, model := range cfg.Models {
		if !model.Enabled {
			continue
		}
		if model.StartDelaySeconds > 0 {
			select {
			case <-ctx.Done():
				results = append(results, StartupResult{ModelID: model.ModelID, Reason: ctx.Err().Error()})
				return results
			case <-time.After(time.Duration(model.StartDelaySeconds) * time.Second):
			}
		}
		err := c.StartConfiguredModel(ctx, model)
		if err != nil {
			results = append(results, StartupResult{ModelID: model.ModelID, Reason: err.Error()})
			continue
		}
		results = append(results, StartupResult{ModelID: model.ModelID, Started: true, Reason: "running or accepted by LocalHub"})
	}
	return results
}

// RestoreLastSession adds models that were active in the tray's most recent snapshot without
// overriding explicitly configured entries. Restored models use safe LocalHub API defaults. It is
// off by default (see startupconfig.DefaultStartupConfig) and does nothing unless enabled.
func (c *Controller) RestoreLastSession(cfg startupconfig.StartupConfig) startupconfig.StartupConfig {
	if !cfg.RestoreLastSession {
		return cfg
	}
	data, err := os.ReadFile(filepath.Join(c.repoRoot, "logs", "tray-last-session.json"))
	if err != nil {
		return cfg
	}
	var ids []string
	if json.Unmarshal(data, &ids) != nil {
		return cfg
	}
	seen := make(map[string]bool, len(cfg.Models))
	for _, model := range cfg.Models {
		seen[model.ModelID] = true
	}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		cfg.Models = append(cfg.Models, startupconfig.StartupModelConfig{
			ModelID: id, Enabled: true, Order: len(cfg.Models),
			Mode:        startupconfig.StartupModeLocalHubAPI,
			RetryPolicy: startupconfig.RetryNever,
		})
	}
	_ = cfg.ValidateAndNormalize()
	return cfg
}

// SaveLastSession snapshots active model IDs atomically. It records observed state only; it never
// changes startup intent, and failures are returned so the tray can log rather than claim success.
func (c *Controller) SaveLastSession(ctx context.Context) error {
	instances, err := c.instanceList(ctx)
	if err != nil {
		return err
	}
	set := map[string]bool{}
	for _, instance := range instances {
		switch instance.State {
		case "STARTING", "HEALTHY", "DRAINING":
			if instance.ModelID != "" {
				set[instance.ModelID] = true
			}
		}
	}
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	data, err := json.MarshalIndent(ids, "", "  ")
	if err != nil {
		return err
	}
	directory := filepath.Join(c.repoRoot, "logs")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".tray-session-*.tmp")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(name, filepath.Join(directory, "tray-last-session.json"))
}

func (c *Controller) modelActive(ctx context.Context, modelID string) (bool, error) {
	instances, err := c.instanceList(ctx)
	if err != nil {
		return false, err
	}
	for _, instance := range instances {
		if instance.ModelID != modelID {
			continue
		}
		switch instance.State {
		case "STARTING", "HEALTHY", "DRAINING":
			return true, nil
		}
	}
	return false, nil
}

func (c *Controller) savedLaunchProfile(ctx context.Context, modelID string) (map[string]any, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.webURL+"/api/models/"+url.PathEscape(modelID)+"/launch-default", nil)
	if err != nil {
		return nil, err
	}
	response, err := c.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("read saved launch profile for %s: %w", modelID, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, apiResponseError("read saved launch profile for "+modelID, response)
	}
	var profile map[string]any
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&profile); err != nil {
		return nil, fmt.Errorf("decode saved launch profile for %s: %w", modelID, err)
	}
	delete(profile, "created_at")
	delete(profile, "updated_at")
	delete(profile, "stale_reason")
	delete(profile, "validated_note")
	delete(profile, "engine_version")
	delete(profile, "engine_commit")
	delete(profile, "binary_path")
	delete(profile, "artifact_sha256")
	if spec, ok := profile["spec_mode"]; ok {
		profile["speculative"] = spec
		delete(profile, "spec_mode")
	}
	return profile, nil
}

func mergeConfiguredLaunch(body map[string]any, model startupconfig.StartupModelConfig) {
	setString := func(key, value string) {
		if value != "" {
			body[key] = value
		}
	}
	setInt := func(key string, value int) {
		if value != 0 {
			body[key] = value
		}
	}
	setString("engine", model.Engine)
	setInt("ctx", model.Ctx)
	setInt("batch", model.Batch)
	setInt("priority", model.Priority)
	setString("kv_type_k", model.KVTypeK)
	setString("kv_type_v", model.KVTypeV)
	setInt("slots", model.Slots)
	setString("speculative", model.Speculative)
	setInt("spec_draft_max", model.SpecDraftMax)
	if model.Pin {
		body["pin"] = true
	}
	if model.AllowCPUOffload {
		body["allow_cpu_offload"] = true
	}
}

type apiErrorEnvelope struct {
	Error struct {
		Message string `json:"message"`
		Reason  string `json:"reason"`
	} `json:"error"`
}

func apiResponseError(action string, response *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	var envelope apiErrorEnvelope
	reason := strings.TrimSpace(string(body))
	if json.Unmarshal(body, &envelope) == nil {
		if envelope.Error.Reason != "" {
			reason = envelope.Error.Reason
		} else if envelope.Error.Message != "" {
			reason = envelope.Error.Message
		}
	}
	if reason == "" {
		reason = response.Status
	}
	return fmt.Errorf("%s: HTTP %d: %s", action, response.StatusCode, reason)
}

func (c *Controller) healthy(ctx context.Context, url string) bool {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	response, err := c.client.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	return response.StatusCode >= 200 && response.StatusCode < 300
}

type trayInstance struct {
	ID      string `json:"id"`
	State   string `json:"state"`
	ModelID string `json:"model_id"`
	Port    int    `json:"port"`
}

func (c *Controller) instanceList(ctx context.Context) ([]trayInstance, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.webURL+"/api/instances", nil)
	if err != nil {
		return nil, err
	}
	response, err := c.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("list LocalHub instances: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, apiResponseError("list LocalHub instances", response)
	}
	var instances []trayInstance
	if err := json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&instances); err != nil {
		return nil, fmt.Errorf("decode LocalHub instances: %w", err)
	}
	return instances, nil
}

// stopActiveInstances asks the daemon to stop every live instance. The DELETE path is the normal
// drain-aware stop (DRAINING first, then an NVML-verified teardown), so the tray never has to
// reason about VRAM itself. If the daemon is already unreachable there is nothing to stop, which
// is success rather than an error the operator would have to dismiss on the way out.
func (c *Controller) stopActiveInstances(ctx context.Context) error {
	instances, err := c.instanceList(ctx)
	if err != nil {
		if !c.healthy(ctx, c.healthURL()) {
			return nil
		}
		return fmt.Errorf("list LocalHub instances before shutdown: %w", err)
	}
	stopClient := &http.Client{Timeout: 2 * time.Minute}
	for _, instance := range instances {
		switch instance.State {
		case "STARTING", "HEALTHY", "DRAINING":
		default:
			continue
		}
		stopURL := c.webURL + "/api/instances/" + url.PathEscape(instance.ID)
		stopRequest, err := http.NewRequestWithContext(ctx, http.MethodDelete, stopURL, nil)
		if err != nil {
			return err
		}
		stopResponse, err := stopClient.Do(stopRequest)
		if err != nil {
			return fmt.Errorf("stop LocalHub instance %s: %w", instance.ID, err)
		}
		body, _ := io.ReadAll(io.LimitReader(stopResponse.Body, 4096))
		stopResponse.Body.Close()
		if stopResponse.StatusCode < 200 || stopResponse.StatusCode >= 300 {
			return fmt.Errorf("stop LocalHub instance %s: HTTP %d: %s",
				instance.ID, stopResponse.StatusCode, strings.TrimSpace(string(body)))
		}
	}
	// DELETE is intentionally asynchronous (normally 202): the daemon transitions through
	// DRAINING, stops the process tree, measures the NVML delta, then writes STOPPED. Killing the
	// daemon immediately after submitting DELETE orphans llama-server and turns a queued stop into
	// a false success. Wait until every active row is terminal before daemon termination.
	for {
		current, err := c.instanceList(ctx)
		if err != nil {
			return fmt.Errorf("verify LocalHub instances stopped before daemon shutdown: %w", err)
		}
		activeIDs := make([]string, 0)
		for _, instance := range current {
			switch instance.State {
			case "STARTING", "HEALTHY", "DRAINING":
				activeIDs = append(activeIDs, instance.ID)
			}
		}
		if len(activeIDs) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("LocalHub instances still active after shutdown deadline (%s): %w",
				strings.Join(activeIDs, ", "), ctx.Err())
		case <-time.After(daemonPollInterval):
		}
	}
}
