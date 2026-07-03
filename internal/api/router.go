package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/indeclau/deitafix/internal/engine"
	"github.com/indeclau/deitafix/internal/guard"
	"github.com/indeclau/deitafix/internal/store"
)

// Handler agrupa las dependencias de los handlers HTTP.
type Handler struct {
	svc     *Service
	enabled bool
}

// NewRouter construye el router chi con las rutas del servicio.
//
// enabled es el feature flag (DATAFIX_ENABLED): si es false, las rutas de
// escritura responden 503 sin tocar la base.
func NewRouter(svc *Service, enabled bool) http.Handler {
	h := &Handler{svc: svc, enabled: enabled}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)

	// Healthcheck: no depende del feature flag.
	r.Get("/healthz", h.health)

	// Rutas de escritura, detrás del feature flag.
	r.Group(func(r chi.Router) {
		r.Use(h.requireEnabled)
		r.Post("/preview", h.preview)
		r.Post("/confirm", h.confirm)
	})

	return r
}

// --- Tipos de request/response (contrato JSON) ---

// boundedOpRequest es la representación JSON de una operación acotada.
type boundedOpRequest struct {
	Op    string         `json:"op"`
	Table string         `json:"table"`
	Set   map[string]any `json:"set,omitempty"`
	Where map[string]any `json:"where"`
}

// previewRequest es el cuerpo de POST /preview. Trae SQL crudo u operación
// acotada (exactamente uno).
//
// Engine se acepta por compatibilidad con el ejemplo del README pero se ignora:
// el motor es una propiedad del servidor (a qué base apunta DATABASE_URL), no
// algo que el cliente pueda elegir por request.
type previewRequest struct {
	Engine    string            `json:"engine,omitempty"`
	SQL       string            `json:"sql,omitempty"`
	Operation *boundedOpRequest `json:"operation,omitempty"`
}

// previewResponse es el cuerpo de la respuesta de /preview.
type previewResponse struct {
	Token        string `json:"token"`
	AffectedRows int64  `json:"affected_rows"`
	Summary      string `json:"summary"`
}

// confirmRequest es el cuerpo de POST /confirm. Solo el token, nunca SQL.
type confirmRequest struct {
	Token string `json:"token"`
}

// confirmResponse es el cuerpo de la respuesta de /confirm.
type confirmResponse struct {
	AffectedRows int64  `json:"affected_rows"`
	Summary      string `json:"summary"`
}

// errorResponse es el cuerpo de error uniforme.
type errorResponse struct {
	Error string `json:"error"`
}

// --- Handlers ---

func (h *Handler) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) preview(w http.ResponseWriter, r *http.Request) {
	var req previewRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	var op *engine.BoundedOp
	if req.Operation != nil {
		parsed, err := toBoundedOp(req.Operation)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		op = &parsed
	}

	res, err := h.svc.Preview(r.Context(), req.SQL, op)
	if err != nil {
		writeError(w, statusForError(err), err)
		return
	}

	writeJSON(w, http.StatusOK, previewResponse(res))
}

func (h *Handler) confirm(w http.ResponseWriter, r *http.Request) {
	var req confirmRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Token == "" {
		writeError(w, http.StatusBadRequest, errors.New("api: falta el token"))
		return
	}

	res, err := h.svc.Confirm(r.Context(), req.Token)
	if err != nil {
		writeError(w, statusForError(err), err)
		return
	}

	writeJSON(w, http.StatusOK, confirmResponse(res))
}

// requireEnabled es el middleware del feature flag.
func (h *Handler) requireEnabled(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !h.enabled {
			writeError(w, http.StatusServiceUnavailable,
				errors.New("api: servicio deshabilitado (DATAFIX_ENABLED=false)"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- Helpers ---

// toBoundedOp convierte el request JSON en el BoundedOp del dominio, validando
// la operación.
func toBoundedOp(req *boundedOpRequest) (engine.BoundedOp, error) {
	op := engine.BoundedOp{
		Table: req.Table,
		Set:   req.Set,
		Where: req.Where,
	}
	switch guard.Operation(req.Op) {
	case guard.OpUpdate:
		op.Op = guard.OpUpdate
	case guard.OpDelete:
		op.Op = guard.OpDelete
	default:
		return engine.BoundedOp{}, errors.New("api: operación acotada inválida (esperado UPDATE|DELETE)")
	}
	return op, nil
}

// statusForError mapea los errores conocidos a códigos HTTP. Los rechazos por
// guardas son 422 (entrada válida sintácticamente pero no permitida); el token
// inexistente/expirado es 404; el resto, 500.
func statusForError(err error) int {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrEmptyInput),
		errors.Is(err, ErrAmbiguousInput),
		errors.Is(err, guard.ErrEmptySQL),
		errors.Is(err, guard.ErrParse):
		return http.StatusBadRequest
	case errors.Is(err, guard.ErrMultipleStatements),
		errors.Is(err, guard.ErrOperationNotAllowed),
		errors.Is(err, guard.ErrMissingWhere),
		errors.Is(err, guard.ErrInsertFromSelect),
		errors.Is(err, guard.ErrTableNotWhitelisted),
		errors.Is(err, guard.ErrRowsExceeded):
		return http.StatusUnprocessableEntity
	default:
		return http.StatusInternalServerError
	}
}

// decodeJSON decodifica el cuerpo con campos desconocidos prohibidos, para
// rechazar payloads mal formados en lugar de ignorarlos silenciosamente.
func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return errors.New("api: JSON inválido en el cuerpo de la petición")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, errorResponse{Error: err.Error()})
}
