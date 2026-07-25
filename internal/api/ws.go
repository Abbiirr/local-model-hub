package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/coder/websocket"
)

// heartbeatInterval is the max gap between server→client frames. The client marks the link
// stalled after 15s of silence (wiring-p15.md §2), so a heartbeat at 10s keeps it healthy
// even when nothing else is being pushed. It is a var (not a const) only so tests can lower
// it; production never changes it.
var heartbeatInterval = 10 * time.Second

// wsFrame is one WebSocket message: a single JSON object {"type": ..., "data": ...}. The
// browser client parses it with the SAME parseFrame() it uses for SSE (web/src/live/
// connection.ts). `data` is omitted for content-free frames (heartbeat, or an updated
// signal the client only uses to trigger a REST refetch).
type wsFrame struct {
	Type string `json:"type"`
	Data any    `json:"data,omitempty"`
	TS   string `json:"ts,omitempty"` // set on heartbeat frames only
}

// handleWS serves GET /api/ws: a server-push live channel. On connect it sends one
// hardware.snapshot immediately (so the UI paints without waiting), then a snapshot every
// Hardware.PollIntervalSeconds, a heartbeat every <=10s, and models.updated / engines.updated
// frames as the core publishes them. It never reads application frames from the client
// (Phase 1 is push-only); it does drain control frames so ping/pong and close work.
//
// The endpoint is same-origin and loopback-bound (the server listens on 127.0.0.1); we set
// InsecureSkipVerify on the origin check because a loopback daemon has no meaningful origin
// allowlist and the browser same-origin policy plus the loopback bind already gate access.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // loopback-only daemon; same-origin + bind gate access
	})
	if err != nil {
		// Accept already wrote an HTTP error response (e.g. 426 if not an upgrade); nothing
		// more to do. Log server-side for diagnosis.
		s.logger.Warn("ws accept failed", "err", err, "remote", r.RemoteAddr)
		return
	}
	// Use the request context so a client disconnect / server shutdown cancels the loop.
	ctx := r.Context()
	defer conn.Close(websocket.StatusNormalClosure, "server closing")

	// Subscribe to core change events (models.updated / engines.updated). nil-safe.
	events, unsubscribe := s.svc.Events().Subscribe()
	defer unsubscribe()

	// Drain incoming control/data frames in the background so pings are answered and a
	// client close is observed promptly (it cancels ctx via conn.CloseRead).
	ctx = conn.CloseRead(ctx)

	// Immediate first hardware snapshot so the UI paints on connect.
	if !s.wsSendHardware(ctx, conn) {
		return
	}

	pollInterval := s.svc.HardwarePollInterval()
	pollTicker := time.NewTicker(pollInterval)
	defer pollTicker.Stop()
	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-pollTicker.C:
			if !s.wsSendHardware(ctx, conn) {
				return
			}
		case <-heartbeat.C:
			frame := wsFrame{Type: "heartbeat", TS: time.Now().UTC().Format(time.RFC3339)}
			if !s.wsSend(ctx, conn, frame) {
				return
			}
		case ev, ok := <-events:
			if !ok {
				continue // bus subscriber closed (nil bus); keep heartbeating
			}
			if !s.wsSend(ctx, conn, wsFrame{Type: string(ev.Type), Data: ev.Data}) {
				return
			}
		}
	}
}

// wsSendHardware probes hardware and pushes a hardware.snapshot frame. A transient probe
// error skips the frame (mirrors the SSE handler) rather than closing the connection.
// Returns false if the write failed (caller should stop the loop).
func (s *Server) wsSendHardware(ctx context.Context, conn *websocket.Conn) bool {
	snap, err := s.svc.Hardware(ctx)
	if err != nil {
		return true // skip this frame; keep the connection alive
	}
	return s.wsSend(ctx, conn, wsFrame{Type: "hardware.snapshot", Data: snap})
}

// wsSend marshals and writes one JSON text frame under a short per-write deadline so a dead
// client can't wedge the goroutine. Returns false on any error (the caller stops the loop);
// the error is logged at debug-ish level since a client closing is routine, not exceptional.
func (s *Server) wsSend(ctx context.Context, conn *websocket.Conn, frame wsFrame) bool {
	b, err := json.Marshal(frame)
	if err != nil {
		s.logger.Warn("ws frame marshal failed", "type", frame.Type, "err", err)
		return true // a marshal bug on one frame type shouldn't kill the whole stream
	}
	writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := conn.Write(writeCtx, websocket.MessageText, b); err != nil {
		s.logger.Info("ws write ended", "type", frame.Type, "err", err)
		return false
	}
	return true
}
