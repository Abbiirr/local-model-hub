package cluster

// aggregate.go is the P7.2 coordinator fan-out (docs/cluster-plan.md §2.5, §5b, §8 P7.2). It
// concurrently GETs each registered node's /api/models and /api/hardware (authed with the shared
// cluster token) with a PER-NODE timeout, tags every result with {node_id, node_name}, and returns
// an EXPLICIT per-node error entry for any node that times out or errors — never a hang, never a
// whole-request failure (cluster-plan §7 failure-modes: "an UNREACHABLE node yields a tagged gap").
// The coordinator's OWN local data is included as a node entry WITHOUT an HTTP round-trip (it is
// also a node, cluster-plan §2.2), via the injected LocalProvider closures — which is what keeps
// this package free of internal/core and internal/api (the same discipline as the node Client's
// injected hw-summary and the registry's DB-only surface).
//
// This layer NEVER interprets a node's hardware/inventory beyond display and id-dedupe: the owner's
// scope (verbatim, 2026-07-08) is "cluster means not necessarily k8s or anything like that simple
// over the network gpu clusters" — the coordinator aggregates and attributes, it does not compute
// fits (ADR-008, one-core-per-node).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// defaultPerNodeTimeout bounds each fan-out call so a single hung/slow node can never stall the
// aggregate: the whole request finishes within ~this bound + goroutine scheduling (cluster-plan §8
// P7.2 "an UNREACHABLE node yields a tagged gap, never a hang").
const defaultPerNodeTimeout = 4 * time.Second

// maxNodeBodyBytes caps how much of a node's response we read, so a misbehaving/hostile node cannot
// balloon coordinator memory. Inventories are small; 8 MiB is generous headroom.
const maxNodeBodyBytes = 8 << 20

// HWSummary is the compact hardware summary a node attaches to each heartbeat (JoinRequest.HWSummary,
// marshaled to JSON). SHAPE CONTRACT (cluster-plan §5b, §2.4): the coordinator stores it VERBATIM in
// nodes.hw_summary_json and returns it in listings — it NEVER parses it for a fit decision
// (ADR-008 one-core-per-node). Intentionally tiny: enough for the coordinator's display/attribution,
// not enough to tempt the coordinator into computing verdicts itself. captured_at is the NODE's clock
// and is DISPLAY-ONLY — liveness is always the coordinator-clock last_seen (§7 "clock skew is
// structurally irrelevant"), so this timestamp is never compared for health.
type HWSummary struct {
	GPUs        []HWGPU `json:"gpus"`
	RAMFreeMiB  int     `json:"ram_free_mib"`
	RAMTotalMiB int     `json:"ram_total_mib"`
	CapturedAt  string  `json:"captured_at,omitempty"`
}

// HWGPU is one GPU line in an HWSummary: name + free/total VRAM in MiB (the free/total the node's own
// probe measured). No temp/util/power — those are the node's local admission concern, not the
// coordinator's display concern.
type HWGPU struct {
	Name         string `json:"name"`
	VRAMFreeMiB  int    `json:"vram_free_mib"`
	VRAMTotalMiB int    `json:"vram_total_mib"`
}

// JSON marshals the summary to the compact string a node reports in JoinRequest.HWSummary. It never
// errors in practice (all fields are plain scalars/slices); on the impossible marshal failure it
// returns "" so a heartbeat still goes out (the summary is advisory, never load-bearing).
func (h HWSummary) JSON() string {
	b, err := json.Marshal(h)
	if err != nil {
		return ""
	}
	return string(b)
}

// LocalProvider supplies the coordinator's OWN node data without an HTTP round-trip (the coordinator
// is also a node, cluster-plan §2.2). Each func returns the SAME value the corresponding node
// endpoint returns (Models == GET /api/models, Hardware == GET /api/hardware, V1Models == the
// /v1/models list envelope), so the self entry is shaped identically to a fanned-out node entry after
// marshaling. The integrator (internal/api) supplies these as thin closures over its *core.Service,
// keeping THIS package free of core/api types. A zero LocalProvider (nil funcs / empty NodeID) simply
// omits the self entry.
type LocalProvider struct {
	NodeID   string
	NodeName string
	Models   func(ctx context.Context) (any, error) // == GET /api/models   ([]core.ModelSummary)
	Hardware func(ctx context.Context) (any, error) // == GET /api/hardware (hardware.Snapshot)
	V1Models func(ctx context.Context) (any, error) // == GET /v1/models    ({object:list,data:[...]})
}

// Aggregator fans out reads across the coordinator's registered nodes + its own local data. It holds
// no mutable state; every method takes a fresh registry snapshot and returns a value, so it is safe
// for concurrent use by multiple in-flight requests.
type Aggregator struct {
	reg     *Registry
	self    LocalProvider
	token   string
	timeout time.Duration
	http    *http.Client
	logger  *slog.Logger
}

// NewAggregator builds a coordinator aggregator over reg, including self (the coordinator's own node
// entry). token is the shared cluster bearer sent on every coordinator→node call. timeout is the
// per-node bound (0 → defaultPerNodeTimeout). The http.Client has no client-wide timeout: the bound
// is applied per request via a context deadline so one slow node cannot consume another's budget.
func NewAggregator(reg *Registry, self LocalProvider, token string, timeout time.Duration, logger *slog.Logger) *Aggregator {
	if timeout <= 0 {
		timeout = defaultPerNodeTimeout
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Aggregator{
		reg:     reg,
		self:    self,
		token:   token,
		timeout: timeout,
		http:    &http.Client{},
		logger:  logger,
	}
}

// NodeModels is one entry in GET /api/cluster/models: a node's inventory, attributed. Models carries
// the node's /api/models body VERBATIM (json.RawMessage — the coordinator does not reshape it). Error
// is set (with a human-readable reason, mandatory-reason gate across the network) when the node timed
// out / errored; Models is then empty. Exactly one of Models/Error is meaningful per entry.
type NodeModels struct {
	NodeID   string          `json:"node_id"`
	NodeName string          `json:"node_name"`
	Models   json.RawMessage `json:"models,omitempty"`
	Error    string          `json:"error,omitempty"`
}

// NodeHardware is one entry in GET /api/cluster/hardware: a node's hardware snapshot, attributed.
// Same partial-result contract as NodeModels.
type NodeHardware struct {
	NodeID   string          `json:"node_id"`
	NodeName string          `json:"node_name"`
	Hardware json.RawMessage `json:"hardware,omitempty"`
	Error    string          `json:"error,omitempty"`
}

// OpenAIList is the union-deduped /v1/models envelope (GET /api/cluster/v1/models). The OpenAI shape
// stays PURE — no node_id/node_name annotation (a model on 2 nodes appears once); the per-node detail
// lives in /api/cluster/models (cluster-plan §5b, task decision). Data is sorted by id for a stable
// response.
type OpenAIList struct {
	Object string             `json:"object"` // always "list"
	Data   []OpenAIModelEntry `json:"data"`
}

// OpenAIModelEntry mirrors core.OpenAIModel structurally so a node's /v1/models data round-trips
// through the union unchanged. Kept local (not an import of core) to preserve the package's core-free
// surface.
type OpenAIModelEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// Models returns every node's inventory (self + registered nodes), attributed, with an explicit error
// entry for any node that could not be reached (partial result, never a hang — cluster-plan §7).
func (a *Aggregator) Models(ctx context.Context) []NodeModels {
	out := make([]NodeModels, 0)
	for _, r := range a.fanout(ctx, "/api/models", a.self.Models) {
		nm := NodeModels{NodeID: r.nodeID, NodeName: r.nodeName, Models: r.raw}
		if r.err != nil {
			nm.Error = r.err.Error()
		}
		out = append(out, nm)
	}
	return out
}

// Hardware returns every node's hardware snapshot (self + registered nodes), attributed, with the
// same partial-result contract as Models.
func (a *Aggregator) Hardware(ctx context.Context) []NodeHardware {
	out := make([]NodeHardware, 0)
	for _, r := range a.fanout(ctx, "/api/hardware", a.self.Hardware) {
		nh := NodeHardware{NodeID: r.nodeID, NodeName: r.nodeName, Hardware: r.raw}
		if r.err != nil {
			nh.Error = r.err.Error()
		}
		out = append(out, nh)
	}
	return out
}

// V1Models returns the union-deduped OpenAI model list across all reachable nodes + self. A node that
// errors is SKIPPED from the union (logged) rather than surfaced — the OpenAI shape has no place for a
// per-node error, and the detail is available via /api/cluster/models (cluster-plan §5b). Dedupe is by
// model id, first occurrence wins; the result is sorted by id.
func (a *Aggregator) V1Models(ctx context.Context) OpenAIList {
	seen := map[string]bool{}
	data := []OpenAIModelEntry{}
	for _, r := range a.fanout(ctx, "/v1/models", a.self.V1Models) {
		if r.err != nil {
			a.logger.Warn("cluster /v1/models: node omitted from union (per-node detail in /api/cluster/models)",
				"node", r.nodeName, "err", r.err)
			continue
		}
		var list OpenAIList
		if err := json.Unmarshal(r.raw, &list); err != nil {
			a.logger.Warn("cluster /v1/models: unparseable node response, omitted from union",
				"node", r.nodeName, "err", err)
			continue
		}
		for _, m := range list.Data {
			if m.ID == "" || seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			data = append(data, m)
		}
	}
	sort.Slice(data, func(i, j int) bool { return data[i].ID < data[j].ID })
	return OpenAIList{Object: "list", Data: data}
}

// fanoutResult is one node's raw fan-out outcome: exactly one of raw/err is set.
type fanoutResult struct {
	nodeID   string
	nodeName string
	raw      json.RawMessage
	err      error
}

// fanout concurrently collects path from the self provider (no HTTP) and from every registered node
// (authed HTTP GET, per-node timeout). It never returns an error: a registry-read failure degrades to
// the self entry only (logged), and every unreachable node becomes an error-tagged result. Results
// are sorted by (name, id) for a deterministic response the gate's group_by can rely on.
func (a *Aggregator) fanout(ctx context.Context, path string, selfFn func(context.Context) (any, error)) []fanoutResult {
	nodes, err := a.reg.List(ctx)
	if err != nil {
		// A coordinator-local DB read failure must not 500 the whole aggregate — still return self so
		// the operator sees the coordinator's own slice (graceful degradation, cluster-plan §7).
		a.logger.Warn("cluster aggregate: registry list failed; returning self only", "err", err)
		nodes = nil
	}

	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		results = make([]fanoutResult, 0, len(nodes)+1)
	)
	add := func(r fanoutResult) {
		mu.Lock()
		results = append(results, r)
		mu.Unlock()
	}

	// Self entry (no HTTP): the coordinator is also a node (cluster-plan §2.2). Omitted when no self
	// provider is wired for this path.
	if selfFn != nil && a.self.NodeID != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := fanoutResult{nodeID: a.self.NodeID, nodeName: a.self.NodeName}
			v, ferr := selfFn(ctx)
			if ferr != nil {
				r.err = fmt.Errorf("local read failed: %w", ferr)
			} else if raw, merr := json.Marshal(v); merr != nil {
				r.err = fmt.Errorf("local marshal failed: %w", merr)
			} else {
				r.raw = raw
			}
			add(r)
		}()
	}

	for _, n := range nodes {
		if n.ID == a.self.NodeID {
			continue // never double-count self if a future build has the coordinator self-register
		}
		n := n
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := fanoutResult{nodeID: n.ID, nodeName: n.Name}
			if n.Status == StatusUnreachable {
				// Already declared dead by the health state machine: emit the tagged gap without a
				// pointless timeout wait (cluster-plan §7 "new placements skip it"; here: mark it).
				r.err = fmt.Errorf("node %q is UNREACHABLE (no heartbeat since %s); skipped fan-out",
					n.Name, lastSeenLabel(n))
				add(r)
				return
			}
			if raw, ferr := a.fetch(ctx, n.BaseURL, path); ferr != nil {
				r.err = ferr
			} else {
				r.raw = raw
			}
			add(r)
		}()
	}
	wg.Wait()

	sort.Slice(results, func(i, j int) bool {
		if results[i].nodeName != results[j].nodeName {
			return results[i].nodeName < results[j].nodeName
		}
		return results[i].nodeID < results[j].nodeID
	})
	return results
}

// fetch performs one authed GET {baseURL}{path} bounded by the per-node timeout. A non-2xx status,
// a transport error, or a timeout all return an error carrying the URL + reason (mandatory-reason
// gate) so the per-node error entry is actionable.
func (a *Aggregator) fetch(ctx context.Context, baseURL, path string) (json.RawMessage, error) {
	cctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	url := strings.TrimRight(baseURL, "/") + path
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request %s: %w", url, err)
	}
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxNodeBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", url, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GET %s returned %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.RawMessage(body), nil
}

// lastSeenLabel renders a node's last_seen for an error reason, or "never" when it has not heartbeated
// since the coordinator restarted (the soft-state "known but not yet live" case, cluster-plan §7).
func lastSeenLabel(n Node) string {
	if n.LastSeen == "" {
		return "never"
	}
	return n.LastSeen
}
