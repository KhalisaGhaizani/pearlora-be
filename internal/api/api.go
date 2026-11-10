// Package api serves the REST API and the frontend status WebSocket.
package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"pearlora/backend/internal/app"
	"pearlora/backend/internal/control"
	"pearlora/backend/internal/settings"
)

type API struct {
	e  *app.Engine
	ui *UIHub
}

// Routes returns the HTTP handler. device serves the ESP32 at /ws/device.
func Routes(e *app.Engine, ui *UIHub, device http.Handler) http.Handler {
	a := &API{e: e, ui: ui}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", a.health)
	mux.HandleFunc("GET /api/status", a.status)
	mux.HandleFunc("POST /api/mode", a.mode)
	mux.HandleFunc("POST /api/manual", a.manual)
	mux.HandleFunc("GET /api/settings", a.getSettings)
	mux.HandleFunc("PUT /api/settings", a.putSettings)
	mux.HandleFunc("POST /api/settings/reset", a.resetSettings)
	mux.HandleFunc("POST /api/auto/target/reset", a.resetTarget)
	mux.HandleFunc("POST /api/device/zero-depth", a.zeroDepth)
	mux.HandleFunc("GET /ws", ui.ServeHTTP)
	mux.Handle("GET /ws/device", device)
	return cors(mux)
}

func (a *API) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "device_connected": a.e.Status().DeviceConnected})
}

func (a *API) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.e.Status())
}

func (a *API) mode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode control.Mode `json:"mode"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if err := a.e.SetMode(req.Mode); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a.e.Status())
}

func (a *API) manual(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action control.Dir `json:"action"`
		Hold   bool        `json:"hold"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if err := a.e.Manual(req.Action, req.Hold); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a.e.Status())
}

func (a *API) getSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.e.Settings())
}

// putSettings decodes over the current settings, so omitted fields keep
// their current values.
func (a *API) putSettings(w http.ResponseWriter, r *http.Request) {
	s := a.e.Settings()
	s.Priority = append([]string(nil), s.Priority...)
	if !readJSON(w, r, &s) {
		return
	}
	if err := a.e.UpdateSettings(s); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, s)
}

func (a *API) resetSettings(w http.ResponseWriter, r *http.Request) {
	s := settings.Defaults()
	if err := a.e.UpdateSettings(s); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s)
}

func (a *API) resetTarget(w http.ResponseWriter, r *http.Request) {
	a.e.ResetTarget()
	writeJSON(w, http.StatusOK, a.e.Status())
}

func (a *API) zeroDepth(w http.ResponseWriter, r *http.Request) {
	if !a.e.Status().DeviceConnected {
		writeErr(w, control.ErrNoDevice)
		return
	}
	a.e.ZeroDepth()
	writeJSON(w, http.StatusAccepted, map[string]string{"result": "zero_depth sent"})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad JSON: " + err.Error()})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, control.ErrBadMode), errors.Is(err, control.ErrBadAction):
		code = http.StatusBadRequest
	case errors.Is(err, control.ErrNotManual), errors.Is(err, control.ErrLimit), errors.Is(err, control.ErrEStop):
		code = http.StatusConflict
	case errors.Is(err, control.ErrNoDevice), errors.Is(err, control.ErrNoTelemetry):
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
