package core

import "sync"

// This file is the minimal in-process pub/sub the WebSocket transport (and any future
// live transport) subscribes to for change signals. It is deliberately tiny and
// dependency-free (PLAN.md decision 7: no external deps for a core seam this small):
//
//   - The core PUBLISHES an Event when something the dashboard cares about changes
//     (a rescan completed → EventModelsUpdated; an engine re-detect ran →
//     EventEnginesUpdated). Hardware snapshots are NOT published here — the WS handler
//     polls the probe on its own cadence, mirroring the SSE handler.
//   - Transports SUBSCRIBE to receive those events and fan them out to connected clients.
//
// Delivery is best-effort and non-blocking: a slow/absent subscriber never blocks a
// publisher (a rescan must not stall because a WS client's buffer is full). Each
// subscriber gets a small buffered channel; on overflow the event is dropped for that
// subscriber (the client re-syncs over REST anyway — these events are invalidation
// signals, not the source of truth, per wiring-p15.md §2).

// EventType is the kind of change being announced. Values match the WS frame `type` the
// client expects (wiring-p15.md §2): "models.updated", "engines.updated".
type EventType string

const (
	// EventModelsUpdated fires after a rescan completes and the catalog may have changed.
	EventModelsUpdated EventType = "models.updated"
	// EventEnginesUpdated fires after an engine re-detection sweep (POST /api/engines/detect).
	EventEnginesUpdated EventType = "engines.updated"
	// EventDownloadProgress fires repeatedly during a download/import with per-file progress
	// (Phase 3). Data is a DownloadEvent. The dashboard's Downloads card renders a live bar.
	EventDownloadProgress EventType = "download.progress"
	// EventDownloadDone fires once a download/import completes (success or failure). Data is a
	// DownloadEvent with Status set. On success the catalog changed → clients also refetch
	// /api/models (EventModelsUpdated is published alongside).
	EventDownloadDone EventType = "download.done"
	// EventInstancesUpdated fires when an instance's state changes (start/stop/reconcile) so the
	// dashboard's instances card refetches /api/instances (P4.1/P4.2).
	EventInstancesUpdated EventType = "instances.updated"
	// EventQueueUpdated fires when the residency queue changes (a request enqueued, admitted,
	// cancelled, or drained) so the dashboard's queue card refetches /api/queue (P5.1). The
	// event vocabulary matches lmwrapper's "queue.update" (research/lmwrapper-reuse-notes.md).
	EventQueueUpdated EventType = "queue.update"
	// EventJobUpdated fires on ANY async-job state change (queued→running→succeeded/failed/
	// cancelled) so a WS subscriber refetches GET /api/jobs/{id} for the terminal result_json
	// (P6.2, design §2.4). It carries a minimal JobEvent Data payload (id/state/model/reason) so a
	// subscriber can react without a refetch round-trip; the result body itself stays off the WS
	// frame (fetched over REST on the terminal event). Named `jobs.updated` to match the existing
	// dotted EventType convention (models.updated / queue.update / instances.updated), not the
	// lmwrapper literal names (request.accepted / response.completed) — one naming scheme, not two.
	EventJobUpdated EventType = "jobs.updated"
)

// JobEvent is the minimal Data payload carried on an EventJobUpdated event (design §2.4). A WS
// subscriber can react to the state transition without a refetch; Reason is populated only on a
// failure/cancel terminal state (the mandatory-reason gate carried onto the event bus).
type JobEvent struct {
	ID      string `json:"id"`
	State   string `json:"state"`
	ModelID string `json:"model_id"`
	Reason  string `json:"reason,omitempty"`
}

// Event is one change announcement. Data is optional — the Phase-1 client uses these purely
// as a signal to refetch over REST, so publishers may leave Data nil and still be correct.
type Event struct {
	Type EventType
	Data any
}

// EventBus is a minimal fan-out pub/sub. The zero value is NOT usable; construct with
// NewEventBus. It is safe for concurrent Publish/Subscribe/unsubscribe.
type EventBus struct {
	mu   sync.Mutex
	subs map[int]chan Event
	next int
}

// NewEventBus constructs an empty bus.
func NewEventBus() *EventBus {
	return &EventBus{subs: make(map[int]chan Event)}
}

// Subscribe registers a new subscriber and returns its receive channel plus an unsubscribe
// func. The channel is buffered; if a subscriber falls behind, Publish drops events for it
// rather than blocking. Always call the returned unsubscribe func (e.g. on WS close) to
// avoid leaking the channel.
func (b *EventBus) Subscribe() (<-chan Event, func()) {
	if b == nil {
		// A nil bus yields a closed, never-firing channel so callers need no nil checks.
		ch := make(chan Event)
		close(ch)
		return ch, func() {}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.next
	b.next++
	ch := make(chan Event, 16)
	b.subs[id] = ch
	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if c, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(c)
		}
	}
}

// Publish delivers ev to every current subscriber without blocking. A subscriber whose
// buffer is full misses this event (it will re-sync over REST). Safe to call on a nil bus
// (no-op), so publishers never need to guard.
func (b *EventBus) Publish(ev Event) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs {
		select {
		case ch <- ev:
		default:
			// Subscriber is behind; drop for it. These are invalidation signals, not the
			// source of truth, so a dropped event only delays a refetch, never corrupts state.
		}
	}
}
