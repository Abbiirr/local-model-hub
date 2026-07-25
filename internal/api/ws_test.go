package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/localhub/localhub/internal/config"
	"github.com/localhub/localhub/internal/core"
	"github.com/localhub/localhub/internal/hardware"
	"github.com/localhub/localhub/internal/inventory"
)

// wsTestServer spins up the full routed handler over a real httptest server and returns its
// http:// URL (the ws client rewrites the scheme). Events bus is wired so rescan/detect
// signals reach the WS. The returned service lets the caller trigger a rescan.
func wsTestServer(t *testing.T, mutate func(*core.Deps)) (*httptest.Server, *core.Service) {
	t.Helper()
	cfg := config.Default()
	cfg.Hardware.PollIntervalSeconds = 1 // 1s floor; fine for the snapshot cadence
	deps := core.Deps{
		Cfg:      cfg,
		DB:       migratedDB(t),
		Hardware: &hardware.FakeProbe{},
		Events:   core.NewEventBus(),
	}
	if mutate != nil {
		mutate(&deps)
	}
	svc := core.New(deps)
	srv := &Server{svc: svc, logger: quietLogger()}
	mux := http.NewServeMux()
	srv.routes(mux)
	chained := recoverMiddleware(quietLogger(), logMiddleware(quietLogger(), mux))
	ts := httptest.NewServer(chained)
	t.Cleanup(ts.Close)
	return ts, svc
}

func wsURL(httpURL string) string {
	return "ws" + strings.TrimPrefix(httpURL, "http") + "/api/ws"
}

// readFrame reads one JSON message from the WS and unmarshals into a generic map.
func readFrame(t *testing.T, ctx context.Context, c *websocket.Conn) map[string]any {
	t.Helper()
	typ, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("ws read: %v", err)
	}
	if typ != websocket.MessageText {
		t.Fatalf("ws message type = %v, want text", typ)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("ws frame not JSON object: %v (%s)", err, data)
	}
	return m
}

// TestWSSendsHardwareSnapshotThenHeartbeat asserts the connect contract: the FIRST frame is
// a hardware.snapshot (immediate paint), and a heartbeat arrives within the (test-shortened)
// heartbeat interval.
func TestWSSendsHardwareSnapshotThenHeartbeat(t *testing.T) {
	// Shorten the heartbeat so the test doesn't wait 10s. Restore after.
	orig := heartbeatInterval
	heartbeatInterval = 100 * time.Millisecond
	t.Cleanup(func() { heartbeatInterval = orig })

	ts, _ := wsTestServer(t, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "test done")

	// First frame MUST be hardware.snapshot with a data payload carrying the GPU array.
	first := readFrame(t, ctx, c)
	if first["type"] != "hardware.snapshot" {
		t.Fatalf("first frame type = %v, want hardware.snapshot", first["type"])
	}
	data, ok := first["data"].(map[string]any)
	if !ok {
		t.Fatalf("hardware.snapshot missing data object; got %v", first)
	}
	if _, ok := data["gpus"]; !ok {
		t.Errorf("hardware.snapshot data missing gpus; got keys %v", keysOf(data))
	}
	if _, ok := data["captured_at"]; !ok {
		t.Errorf("hardware.snapshot data missing captured_at")
	}

	// Within a few heartbeat intervals we must see at least one heartbeat frame (the poll
	// ticker is 1s, the heartbeat 100ms, so heartbeats dominate). Read a handful of frames.
	sawHeartbeat := false
	for i := 0; i < 10 && !sawHeartbeat; i++ {
		f := readFrame(t, ctx, c)
		if f["type"] == "heartbeat" {
			sawHeartbeat = true
			if _, ok := f["ts"]; !ok {
				t.Errorf("heartbeat frame missing ts")
			}
		}
	}
	if !sawHeartbeat {
		t.Errorf("no heartbeat frame within 10 frames")
	}
}

// TestWSModelsUpdatedAfterRescan asserts a rescan publishes models.updated to a connected
// WS client. It wires a real scanner over a fixture tree, connects, triggers a rescan, and
// expects a models.updated frame among the pushed frames.
func TestWSModelsUpdatedAfterRescan(t *testing.T) {
	orig := heartbeatInterval
	heartbeatInterval = 5 * time.Second // keep heartbeats sparse so models.updated stands out
	t.Cleanup(func() { heartbeatInterval = orig })

	root := buildFixtureModels(t)
	ts, svc := wsTestServer(t, func(d *core.Deps) {
		d.Inventory = inventory.New([]string{root})
		d.InvRepo = inventory.NewRepository(d.DB)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "test done")

	// Drain the immediate hardware.snapshot.
	_ = readFrame(t, ctx, c)

	// Trigger a rescan on the same core → publishes models.updated on the bus.
	if _, err := svc.RescanModels(ctx); err != nil {
		t.Fatalf("rescan: %v", err)
	}

	// The models.updated frame should arrive; allow a few interleaved snapshot/heartbeat
	// frames before it.
	sawModelsUpdated := false
	for i := 0; i < 15 && !sawModelsUpdated; i++ {
		f := readFrame(t, ctx, c)
		if f["type"] == "models.updated" {
			sawModelsUpdated = true
		}
	}
	if !sawModelsUpdated {
		t.Errorf("did not receive models.updated frame after rescan")
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
