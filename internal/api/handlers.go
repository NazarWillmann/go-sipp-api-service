package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"sipp-service/internal/calls"
)

type Handler struct {
	Mgr *calls.Manager
	Reg *calls.Registry

	SippBinary   string
	ScenariosDir string
	LocalIP      string
	LogsDir      string

	SipRangeStart  int
	SipRangeEnd    int
	CtrlRangeStart int
	CtrlRangeEnd   int
}

func (h *Handler) Routes() http.Handler {
	r := chi.NewRouter()

	r.Get("/health", h.Health)
	r.Get("/ready", h.Ready)

	r.Post("/calls/outgoing", h.CreateOutgoing)
	r.Post("/calls/{callId}/dtmf", h.SendDTMF)
	r.Post("/calls/{callId}/hangup", h.Hangup)
	r.Post("/calls/{callId}/disconnect", h.Disconnect)
	r.Get("/calls/{callId}/status", h.Status)

	return r
}

func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

func (h *Handler) Ready(w http.ResponseWriter, r *http.Request) {
	// sipp exists and executable
	st, err := os.Stat(h.SippBinary)
	if err != nil || st.IsDir() {
		writeError(w, http.StatusServiceUnavailable, "SIPP_BINARY not found")
		return
	}
	if st.Mode()&0o111 == 0 {
		writeError(w, http.StatusServiceUnavailable, "SIPP_BINARY is not executable")
		return
	}

	// scenarios dir exists
	st, err = os.Stat(h.ScenariosDir)
	if err != nil || !st.IsDir() {
		writeError(w, http.StatusServiceUnavailable, "SCENARIOS_DIR not found")
		return
	}

	// logs dir exists and is writable (SIPp trace/log files are written next to scenario file).
	if strings.TrimSpace(h.LogsDir) != "" {
		if err := ensureWritableDir(h.LogsDir); err != nil {
			writeError(w, http.StatusServiceUnavailable, "LOGS_DIR is not writable: "+err.Error())
			return
		}
	}

	// port ranges sane + non-overlapping
	if h.SipRangeStart <= 0 || h.SipRangeEnd <= 0 || h.CtrlRangeStart <= 0 || h.CtrlRangeEnd <= 0 {
		writeError(w, http.StatusServiceUnavailable, "port ranges are invalid")
		return
	}
	if h.SipRangeStart > h.SipRangeEnd || h.CtrlRangeStart > h.CtrlRangeEnd {
		writeError(w, http.StatusServiceUnavailable, "port ranges are invalid")
		return
	}
	if rangesOverlap(h.SipRangeStart, h.SipRangeEnd, h.CtrlRangeStart, h.CtrlRangeEnd) {
		writeError(w, http.StatusServiceUnavailable, "SIP and CONTROL port ranges overlap")
		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("READY"))
}

func ensureWritableDir(dir string) error {
	st, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return errors.New("not a directory")
	}
	testPath := filepath.Join(dir, ".writable_test")
	f, err := os.OpenFile(testPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_ = f.Close()
	_ = os.Remove(testPath)
	return nil
}

func rangesOverlap(aStart, aEnd, bStart, bEnd int) bool {
	return aStart <= bEnd && bStart <= aEnd
}

// createOutgoingReq is what you send when you want to start a new call.
// The Service field is required now - it's the actual number you want to call.
// We used to incorrectly use the server IP for this, which didn't work well.
type createOutgoingReq struct {
	RemoteHost  string `json:"remoteHost"`  // Where to send the call (SIP server IP)
	RemotePort  int    `json:"remotePort"`  // SIP server port (usually 5060)
	Destination string `json:"destination"` // Optional label for your own tracking
	Service     string `json:"service"`     // Required: the number to call (like "1234567890")
	Scenario    string `json:"scenario"`    // Which SIPp scenario to use
}

// createOutgoingResp is what we send back when a call is created successfully.
type createOutgoingResp struct {
	CallID string      `json:"callId"` // Unique ID for this call
	State  calls.State `json:"state"`  // Current call state
}

// CreateOutgoing handles requests to start new outgoing calls.
// You need to include a 'service' field now - that's the number you actually want to call.
// Returns 400 if you forget the service field, 409 if we're too busy, 201 if all goes well.
func (h *Handler) CreateOutgoing(w http.ResponseWriter, r *http.Request) {
	var req createOutgoingReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}

	if req.Service == "" {
		writeError(w, http.StatusBadRequest, "service field is required")
		return
	}

	ctx, err := h.Mgr.CreateOutgoing(r.Context(), calls.CreateRequest{
		RemoteHost:  req.RemoteHost,
		RemotePort:  req.RemotePort,
		Destination: req.Destination,
		Service:     req.Service,
		Scenario:    req.Scenario,
	})
	if err != nil {
		switch {
		case errors.Is(err, calls.ErrTooManyCalls):
			writeError(w, http.StatusConflict, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	writeJSON(w, http.StatusCreated, createOutgoingResp{CallID: ctx.CallID, State: ctx.State})
}

type dtmfReq struct {
	Digits            string `json:"digits"`
	InterDigitDelayMs int    `json:"interDigitDelayMs"`
}

func (h *Handler) SendDTMF(w http.ResponseWriter, r *http.Request) {
	callID := chi.URLParam(r, "callId")
	var req dtmfReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}

	ctx, err := h.Mgr.SendDTMF(callID, req.Digits, req.InterDigitDelayMs)
	if err != nil {
		if errors.Is(err, calls.ErrNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		if errors.Is(err, calls.ErrInvalidState) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, ctx)
}

func (h *Handler) Hangup(w http.ResponseWriter, r *http.Request) {
	callID := chi.URLParam(r, "callId")
	ctx, err := h.Mgr.Hangup(callID, 3*time.Second)
	if err != nil {
		if errors.Is(err, calls.ErrNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ctx)
}

func (h *Handler) Disconnect(w http.ResponseWriter, r *http.Request) {
	callID := chi.URLParam(r, "callId")
	ctx, err := h.Mgr.Disconnect(callID)
	if err != nil {
		if errors.Is(err, calls.ErrNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ctx)
}

func (h *Handler) Status(w http.ResponseWriter, r *http.Request) {
	callID := chi.URLParam(r, "callId")
	ctx, ok := h.Reg.Get(callID)
	if !ok {
		writeError(w, http.StatusNotFound, "call not found")
		return
	}
	writeJSON(w, http.StatusOK, ctx)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
