package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Client is the node-side join/heartbeat loop (cluster-plan §2.4). It pushes ONE idempotent
// upsert (POST {coordinator}/api/cluster/nodes/join) on start and then every heartbeat interval.
// It is best-effort by design: the coordinator being down must NEVER affect local serving, so
// every failure is logged (once, until recovery) and retried with a short backoff — the loop
// never returns an error to the daemon and never blocks the local API.
type Client struct {
	coordinatorURL string
	token          string
	req            JoinRequest
	interval       time.Duration
	http           *http.Client
	logger         *slog.Logger

	// hwSummary, when set, is invoked once per beat to refresh the compact hardware summary the node
	// reports (JoinRequest.HWSummary). Injected as a closure so this package stays free of the
	// hardware package (the integrator maps its own probe → cluster.HWSummary.JSON() in main.go). Nil
	// leaves HWSummary empty — the summary is advisory (coordinator display), never load-bearing, so a
	// node that cannot probe its hardware still heartbeats and stays live (cluster-plan §2.4, §5b).
	hwSummary func(context.Context) string

	// down tracks whether we are currently in a failing state, for log-once semantics: log the
	// first failure loudly and the first recovery, but stay quiet on repeated identical failures.
	down bool
}

// ClientConfig carries the immutable node identity + coordinator coordinates for the heartbeat loop.
type ClientConfig struct {
	CoordinatorURL string        // e.g. "http://127.0.0.1:39921"
	Token          string        // shared cluster bearer (cluster.cluster_token)
	NodeID         string        // this daemon's stable UUID (LoadOrCreateNodeID)
	Name           string        // human label (cluster.node_name)
	BaseURL        string        // how the coordinator reaches THIS node (cluster.advertise_url)
	Version        string        // localhub version for skew visibility
	Interval       time.Duration // heartbeat cadence
	Logger         *slog.Logger
	// HWSummary, when set, is called once per beat to produce the node's compact hardware summary
	// (free/total VRAM, GPU name, free RAM) as the JSON string reported in JoinRequest.HWSummary
	// (P7.2, cluster-plan §2.4/§5b). The integrator builds it from the node's own hardware probe.
	HWSummary func(context.Context) string
}

// NewClient builds a node heartbeat client. It does not perform any I/O until Run is called.
func NewClient(cfg ClientConfig) *Client {
	interval := cfg.Interval
	if interval <= 0 {
		interval = 10 * time.Second
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{
		coordinatorURL: strings.TrimRight(cfg.CoordinatorURL, "/"),
		token:          cfg.Token,
		req: JoinRequest{
			NodeID:  cfg.NodeID,
			Name:    cfg.Name,
			BaseURL: cfg.BaseURL,
			Version: cfg.Version,
		},
		interval: interval,
		// Per-request timeout well under the heartbeat interval so a hung coordinator never stacks
		// goroutines; the loop just records a miss and retries.
		http:      &http.Client{Timeout: 5 * time.Second},
		logger:    logger,
		hwSummary: cfg.HWSummary,
	}
}

// Run drives the join+heartbeat loop until ctx is cancelled. It sends an immediate first beat
// (so a node appears in the registry promptly, not after one interval), then ticks. All errors are
// swallowed after logging — this goroutine's contract is "never let coordinator health touch local
// serving" (cluster-plan §1.3, §7). On a transient failure it retries at a shortened backoff so the
// registry rebuilds quickly after a coordinator restart.
func (c *Client) Run(ctx context.Context) {
	c.logger.Info("cluster node heartbeat starting",
		"coordinator", c.coordinatorURL, "node_id", c.req.NodeID, "name", c.req.Name,
		"base_url", c.req.BaseURL, "interval", c.interval.String())
	c.beat(ctx)
	timer := time.NewTimer(c.nextDelay())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			c.logger.Info("cluster node heartbeat stopped")
			return
		case <-timer.C:
			c.beat(ctx)
			timer.Reset(c.nextDelay())
		}
	}
}

// nextDelay returns the wait before the next beat: the full interval when healthy, a shortened
// backoff (min(interval, 3s)) while the coordinator is unreachable so the registry rebuilds fast.
func (c *Client) nextDelay() time.Duration {
	if c.down {
		if c.interval < 3*time.Second {
			return c.interval
		}
		return 3 * time.Second
	}
	return c.interval
}

// beat performs one join/heartbeat POST and updates the log-once down/up state. It first refreshes the
// hardware summary (P7.2) so every heartbeat carries the node's CURRENT free VRAM / free RAM — the
// coordinator's aggregated view is only as fresh as the last beat (cluster-plan §2.5).
func (c *Client) beat(ctx context.Context) {
	if c.hwSummary != nil {
		c.req.HWSummary = c.hwSummary(ctx)
	}
	if err := c.postOnce(ctx); err != nil {
		if !c.down {
			// First failure (or first after a recovery): log loudly with the readable reason. The
			// token-typo case (401) is the #1 expected setup failure — the reason names it.
			c.logger.Warn("cluster heartbeat failed; coordinator unreachable or rejecting (local serving unaffected, retrying)",
				"coordinator", c.coordinatorURL, "err", err)
			c.down = true
		}
		return
	}
	if c.down {
		c.logger.Info("cluster heartbeat recovered", "coordinator", c.coordinatorURL)
		c.down = false
	}
}

// postOnce sends a single join/heartbeat upsert and returns an error unless the coordinator
// answers 2xx. A non-2xx (e.g. 401 token mismatch) carries the coordinator's reason string.
func (c *Client) postOnce(ctx context.Context) error {
	body, err := json.Marshal(c.req)
	if err != nil {
		return fmt.Errorf("marshal join: %w", err)
	}
	url := c.coordinatorURL + "/api/cluster/nodes/join"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build join request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Drain a little of the body so the connection can be reused, and surface it in the error.
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("coordinator returned %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	return nil
}
