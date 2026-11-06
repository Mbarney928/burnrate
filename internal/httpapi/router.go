// Package httpapi exposes burnrate's use cases as a JSON HTTP API.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/Mbarney928/burnrate/internal/model"
	"github.com/Mbarney928/burnrate/internal/ratelimit"
	"github.com/Mbarney928/burnrate/internal/service"
)

// maxBodyBytes bounds request bodies; a full 1000-event batch is well under this.
const maxBodyBytes = 1 << 20

// TokenLookup resolves a token hash to its token; *store.Store implements it.
type TokenLookup interface {
	TokenByHash(ctx context.Context, hash string) (model.Token, error)
}

// Deps are the collaborators the router needs.
type Deps struct {
	Service      *service.Service
	Tokens       TokenLookup
	Health       func(context.Context) error
	OpenAPI      []byte
	Logger       *slog.Logger
	Limiter      *ratelimit.Limiter
	AuthFailures *ratelimit.Limiter
}

type handlers struct {
	svc *service.Service
	log *slog.Logger
}

// NewRouter builds the HTTP handler: middleware, public endpoints and the authenticated API.
func NewRouter(d Deps) http.Handler {
	h := handlers{svc: d.Service, log: d.Logger}
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(securityHeaders)
	r.Use(requestLogger(d.Logger))
	r.Use(recoverer(d.Logger))
	r.Use(rateLimit(d.Limiter))

	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such route", nil)
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", nil)
	})

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := d.Health(ctx); err != nil {
			d.Logger.ErrorContext(r.Context(), "health check failed", "error", err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	r.Get("/openapi.yaml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(d.OpenAPI)
	})

	r.Route("/api/v1", func(r chi.Router) {
		r.Use(authenticate(d.Tokens, d.AuthFailures, d.Logger))
		r.Get("/status", h.overview)
		r.Get("/services", h.listServices)
		r.With(requireWriter).Post("/services", h.createService)
		r.Get("/services/{service}", h.getService)
		r.Get("/services/{service}/slos", h.listSLOs)
		r.With(requireWriter).Post("/services/{service}/slos", h.createSLO)
		r.Get("/slos/{id}", h.getSLO)
		r.Get("/slos/{id}/status", h.sloStatus)
		r.With(requireWriter).Post("/slos/{id}/events", h.recordEvents)
	})
	return r
}

func (h handlers) overview(w http.ResponseWriter, r *http.Request) {
	at, ok := parseAt(w, r)
	if !ok {
		return
	}
	all, err := h.svc.Overview(r.Context(), at)
	h.respond(w, r, http.StatusOK, map[string]any{"slos": all}, err)
}

func (h handlers) listServices(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.ListServices(r.Context())
	h.respond(w, r, http.StatusOK, map[string]any{"services": list}, err)
}

func (h handlers) createService(w http.ResponseWriter, r *http.Request) {
	var in service.CreateServiceInput
	if !decode(w, r, &in) {
		return
	}
	svc, err := h.svc.CreateService(r.Context(), in)
	h.respond(w, r, http.StatusCreated, svc, err)
}

func (h handlers) getService(w http.ResponseWriter, r *http.Request) {
	svc, err := h.svc.GetService(r.Context(), chi.URLParam(r, "service"))
	h.respond(w, r, http.StatusOK, svc, err)
}

func (h handlers) listSLOs(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.ListSLOs(r.Context(), chi.URLParam(r, "service"))
	h.respond(w, r, http.StatusOK, map[string]any{"slos": list}, err)
}

func (h handlers) createSLO(w http.ResponseWriter, r *http.Request) {
	var in service.CreateSLOInput
	if !decode(w, r, &in) {
		return
	}
	o, err := h.svc.CreateSLO(r.Context(), chi.URLParam(r, "service"), in)
	h.respond(w, r, http.StatusCreated, o, err)
}

func (h handlers) getSLO(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	o, err := h.svc.GetSLO(r.Context(), id)
	h.respond(w, r, http.StatusOK, o, err)
}

func (h handlers) sloStatus(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	at, ok := parseAt(w, r)
	if !ok {
		return
	}
	st, err := h.svc.Status(r.Context(), id, at)
	h.respond(w, r, http.StatusOK, st, err)
}

func (h handlers) recordEvents(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	var in struct {
		Events []service.EventInput `json:"events"`
	}
	if !decode(w, r, &in) {
		return
	}
	n, err := h.svc.RecordEvents(r.Context(), id, in.Events)
	h.respond(w, r, http.StatusAccepted, map[string]int{"accepted": n}, err)
}

// respond writes body with status, or maps err to a consistent error response.
func (h handlers) respond(w http.ResponseWriter, r *http.Request, status int, body any, err error) {
	var verr *model.ValidationError
	switch {
	case err == nil:
		writeJSON(w, status, body)
	case errors.As(err, &verr):
		writeError(w, http.StatusBadRequest, "validation_failed", "request failed validation", verr.Fields)
	case errors.Is(err, model.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "resource not found", nil)
	case errors.Is(err, model.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", "resource already exists", nil)
	default:
		internalError(w, r, h.log, err)
	}
}

// internalError logs the real error with the request ID and returns a generic message.
func internalError(w http.ResponseWriter, r *http.Request, log *slog.Logger, err error) {
	log.ErrorContext(r.Context(), "request failed", "request_id", middleware.GetReqID(r.Context()), "error", err)
	writeError(w, http.StatusInternalServerError, "internal_error", "internal server error", nil)
}

// decode reads a single JSON object, rejecting unknown fields, trailing data and oversized bodies.
func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	err := dec.Decode(dst)
	if err == nil && dec.Decode(&struct{}{}) != io.EOF {
		err = errors.New("request body must contain a single JSON object")
	}
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", fmt.Sprintf("request body exceeds %d bytes", maxBodyBytes), nil)
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid_json", "request body is not valid JSON for this endpoint", nil)
		return false
	}
	return true
}

func parseID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "validation_failed", "request failed validation", map[string]string{"id": "must be a positive integer"})
		return 0, false
	}
	return id, true
}

// parseAt reads the optional ?at= RFC 3339 instant for point-in-time status queries.
func parseAt(w http.ResponseWriter, r *http.Request) (time.Time, bool) {
	v := r.URL.Query().Get("at")
	if v == "" {
		return time.Time{}, true
	}
	at, err := time.Parse(time.RFC3339, v)
	if err != nil {
		writeError(w, http.StatusBadRequest, "validation_failed", "request failed validation", map[string]string{"at": "must be an RFC 3339 timestamp"})
		return time.Time{}, false
	}
	return at, true
}

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Fields  map[string]string `json:"fields,omitempty"`
}

func writeError(w http.ResponseWriter, status int, code, msg string, fields map[string]string) {
	writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Message: msg, Fields: fields}})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
