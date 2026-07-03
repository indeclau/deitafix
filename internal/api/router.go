package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/indeclau/deitafix/internal/ai"
	"github.com/indeclau/deitafix/internal/engine"
	"github.com/indeclau/deitafix/internal/guard"
	"github.com/indeclau/deitafix/internal/mcp"
	"github.com/indeclau/deitafix/internal/store"
	"github.com/indeclau/deitafix/internal/ui"
)

// readyzTimeout acota el ping de la probe de readiness para que /readyz no
// quede colgado si la base no responde.
const readyzTimeout = 3 * time.Second

// Handler agrupa las dependencias de los handlers HTTP.
type Handler struct {
	svc     *Service
	enabled bool
}

// RouterConfig son las opciones para construir el router, más allá del servicio.
type RouterConfig struct {
	// Enabled es el feature flag maestro (DATAFIX_ENABLED): si es false, las
	// rutas de escritura responden 503 sin tocar la base.
	Enabled bool

	// MCPEnabled habilita la capa MCP. Si es false, el endpoint MCP no se
	// registra y el resto del servicio queda intacto.
	MCPEnabled bool

	// MCPAuthToken es el bearer que protege el endpoint MCP. Requerido si
	// MCPEnabled es true (lo garantiza config.Load).
	MCPAuthToken string

	// MCPPath es la ruta donde se monta el endpoint MCP (default /mcp).
	MCPPath string

	// MCPApprovalBaseURL es la base pública para la approval_url que devuelve la
	// herramienta MCP confirm. Puede quedar vacía (URL relativa).
	MCPApprovalBaseURL string

	// UIAuthToken protege OPCIONALMENTE la superficie humana (confirm +
	// aprobaciones + UI). Si está vacío, no se exige bearer.
	UIAuthToken string
}

// NewRouter construye el router chi con las rutas del servicio, incluyendo la
// capa MCP y la superficie de aprobación humana.
func NewRouter(svc *Service, cfg RouterConfig) http.Handler {
	h := &Handler{svc: svc, enabled: cfg.Enabled}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)

	// Probes: no dependen del feature flag ni de ninguna auth.
	//   /healthz — liveness: el proceso está vivo, sin tocar la base.
	//   /readyz  — readiness: la base es alcanzable con el usuario restringido.
	r.Get("/healthz", h.healthz)
	r.Get("/readyz", h.readyz)

	// --- Superficie MCP (agente) ---
	// Endpoint separado, protegido por su propio bearer (MCP_AUTH_TOKEN). Solo
	// permite preview y SOLICITAR confirmación; nunca ejecuta. Va detrás del
	// feature flag también: si el servicio está apagado, el agente tampoco opera.
	if cfg.MCPEnabled {
		mcpSrv := mcp.NewServer(&mcpCore{svc: svc}, mcp.Config{
			ApprovalBaseURL: cfg.MCPApprovalBaseURL,
		})
		mcpHandler := mcp.NewStreamableHandler(mcpSrv)
		r.Group(func(r chi.Router) {
			r.Use(h.requireEnabled)
			r.Use(bearerAuth(cfg.MCPAuthToken))
			r.Handle(cfg.MCPPath, mcpHandler)
			r.Handle(cfg.MCPPath+"/*", mcpHandler)
		})
	}

	// --- Superficie humana (UI / API) ---
	// preview + confirm + aprobaciones. Detrás del feature flag y, opcionalmente,
	// de UI_AUTH_TOKEN (defensa en profundidad: la credencial MCP no la alcanza).
	// El confirm humano rechaza tokens origin=mcp con 409: esos van por aprobación.
	r.Group(func(r chi.Router) {
		r.Use(h.requireEnabled)
		if cfg.UIAuthToken != "" {
			r.Use(bearerAuth(cfg.UIAuthToken))
		}
		r.Post("/preview", h.preview)
		r.Post("/confirm", h.confirm)

		// NL → SQL: propone un SQL candidato SIN validar. No toca la base ni
		// llega a confirm; el candidato vuelve por /preview. 503 limpio si la IA
		// está deshabilitada. Va detrás del feature flag y de UI_AUTH_TOKEN, como
		// el resto de la superficie humana.
		r.Post("/ai/suggest", h.aiSuggest)

		// Aprobación humana de propuestas del agente (origin=mcp).
		r.Get("/pending", h.listPending)
		r.Post("/pending/{token}/approve", h.approve)
		r.Post("/pending/{token}/reject", h.reject)
	})

	// UI web embebida. Va fuera del feature flag a propósito: si el servicio
	// está deshabilitado, la página igual debe cargar para reflejar ese estado
	// en vez de fallar de forma opaca. La UI recibe el motor real del servidor
	// para mostrarlo como indicador read-only.
	//
	// Se monta al final como catch-all ("/*") para que las rutas de la API
	// registradas arriba (mismo prefijo raíz) tengan prioridad; la UI sirve
	// "/" (index), "/pending" (aprobaciones) y "/static/*" (Alpine.js), y 404
	// para el resto.
	r.Handle("/*", ui.Handler(svc.Engine()))

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

	// AI controla el enriquecimiento de IA best-effort en el preview. Ausente
	// (nil) significa "usar IA si está habilitada" (default on); false lo omite
	// explícitamente para no pagar latencia/costo cuando no se quiere. Es un
	// puntero para distinguir "no vino el campo" de "vino false".
	AI *bool `json:"ai,omitempty"`
}

// aiInsightResponse es el bloque "ai" de la respuesta de /preview cuando la IA
// está habilitada y respondió. Es nil (JSON null) si la IA está apagada, se
// pidió omitir o falló.
type aiInsightResponse struct {
	Explanation string           `json:"explanation"`
	RiskLevel   string           `json:"risk_level"`
	Flags       []aiFlagResponse `json:"flags"`
}

// aiFlagResponse es un flag del revisor IA en la respuesta.
type aiFlagResponse struct {
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

// previewResponse es el cuerpo de la respuesta de /preview. El campo ai es un
// puntero para poder emitir null cuando no hay IA, dejando el resto intacto.
type previewResponse struct {
	Token        string             `json:"token"`
	AffectedRows int64              `json:"affected_rows"`
	Summary      string             `json:"summary"`
	AI           *aiInsightResponse `json:"ai"`
}

// aiSuggestRequest es el cuerpo de POST /ai/suggest.
type aiSuggestRequest struct {
	Engine string `json:"engine,omitempty"`
	Intent string `json:"intent"`
	Schema string `json:"schema,omitempty"`
}

// aiSuggestResponse es la respuesta de /ai/suggest: el SQL candidato SIN validar.
type aiSuggestResponse struct {
	SQL       string `json:"sql"`
	Rationale string `json:"rationale"`
	Engine    string `json:"engine"`
	// Note recuerda que el candidato todavía no pasó por las guardas.
	Note string `json:"note"`
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

// healthz es la probe de liveness: responde 200 sin tocar la base. Solo indica
// que el proceso está vivo y sirviendo HTTP.
func (h *Handler) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readyz es la probe de readiness: hace ping a la base con el usuario
// restringido. 200 si conecta, 503 si no, para que el orquestador no enrute
// tráfico hasta que la base sea alcanzable.
func (h *Handler) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readyzTimeout)
	defer cancel()

	if err := h.svc.Ready(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
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

	// Default on: la IA se usa salvo que el cliente mande "ai": false.
	aiEnabled := req.AI == nil || *req.AI

	res, err := h.svc.Preview(r.Context(), req.SQL, op, aiEnabled)
	if err != nil {
		writeError(w, statusForError(err), err)
		return
	}

	writeJSON(w, http.StatusOK, toPreviewResponse(res))
}

// toPreviewResponse mapea el PreviewResult del servicio al contrato JSON,
// traduciendo el bloque de IA (o dejándolo en null si no hay).
func toPreviewResponse(res PreviewResult) previewResponse {
	out := previewResponse{
		Token:        res.Token,
		AffectedRows: res.AffectedRows,
		Summary:      res.Summary,
	}
	if res.AI != nil {
		flags := make([]aiFlagResponse, 0, len(res.AI.Flags))
		for _, f := range res.AI.Flags {
			flags = append(flags, aiFlagResponse{
				Severity: string(f.Severity),
				Message:  f.Message,
			})
		}
		out.AI = &aiInsightResponse{
			Explanation: res.AI.Explanation,
			RiskLevel:   string(res.AI.RiskLevel),
			Flags:       flags,
		}
	}
	return out
}

// aiSuggest maneja POST /ai/suggest: genera un SQL candidato a partir de una
// intención en lenguaje natural. NO toca la base (salvo introspección de solo
// lectura). Si la IA está deshabilitada, responde 503 limpio. El candidato NO
// está validado: el cliente debe mandarlo a /preview.
func (h *Handler) aiSuggest(w http.ResponseWriter, r *http.Request) {
	var req aiSuggestRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(req.Intent) == "" {
		writeError(w, http.StatusBadRequest, errors.New("api: falta la intención (intent)"))
		return
	}

	res, err := h.svc.SuggestSQL(r.Context(), req.Intent)
	if err != nil {
		writeError(w, statusForError(err), err)
		return
	}

	writeJSON(w, http.StatusOK, aiSuggestResponse{
		SQL:       res.SQL,
		Rationale: res.Rationale,
		Engine:    res.Engine,
		Note:      "SQL candidato SIN validar: envialo a POST /preview para pasar por las guardas antes de confirmar.",
	})
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
// inexistente/expirado es 404; un token en el estado equivocado (p. ej. mcp por
// el confirm humano, o aprobar algo no pendiente) es 409; el resto, 500.
func statusForError(err error) int {
	switch {
	case errors.Is(err, ai.ErrAIDisabled):
		// IA no configurada: 503 (degradación limpia), no 500.
		return http.StatusServiceUnavailable
	case errors.Is(err, store.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrMCPRequiresApproval),
		errors.Is(err, ErrConfirmNotMCP),
		errors.Is(err, store.ErrWrongState):
		return http.StatusConflict
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
