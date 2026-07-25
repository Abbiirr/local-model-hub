package jsonrpc

import "encoding/json"

// This file holds the tiny param-decode helpers the method bindings share. They are the ONLY
// place params bytes are interpreted, and they interpret them mechanically (unmarshal into a
// shape, pull a field) — no business logic. Keeping them here lets each registry binding stay a
// single call line (thin-transport rule, phase-6-design.md §3.6).

// decodeParams unmarshals params into a typed shape. Absent/empty params are treated as an
// empty object so a method whose fields are all optional works when called with no params.
// A malformed params blob yields an -32602 Invalid params error carrying the decode reason.
func decodeParams(raw json.RawMessage, dst any) *RPCError {
	if len(raw) == 0 || string(raw) == "null" {
		return nil // no params → leave dst at its zero value (all-optional method)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return newError(CodeInvalidParams, "invalid params", err.Error())
	}
	return nil
}

// idParams is the common {"id": "..."} param shape used by get/stop/cancel-style methods.
type idParams struct {
	ID string `json:"id"`
}

// compatByIDParams is the models.compat param shape: an inventory model id plus optional context
// and batch overrides, mirroring the REST GET /api/models/{id}/compat?ctx=&batch= query (design
// §3.2). ctx/batch default to 0, which core.CompatPreview reads as "use the model's default".
type compatByIDParams struct {
	ID    string `json:"id"`
	Ctx   int    `json:"ctx,omitempty"`
	Batch int    `json:"batch,omitempty"`
}

// decodeID pulls a required string id from params, returning an -32602 error when it is missing
// or the params are malformed. Used by instances.get/stop, queue.cancel, jobs.get/cancel,
// models.manifest/reproduce.
func decodeID(raw json.RawMessage) (string, *RPCError) {
	var p idParams
	if rpcErr := decodeParams(raw, &p); rpcErr != nil {
		return "", rpcErr
	}
	if p.ID == "" {
		return "", newError(CodeInvalidParams, "invalid params", `"id" is required`)
	}
	return p.ID, nil
}
