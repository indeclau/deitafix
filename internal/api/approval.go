package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/indeclau/deitafix/internal/engine"
	"github.com/indeclau/deitafix/internal/mcp"
)

// mcpCore adapta *Service a la interfaz mcp.Core. Vive en la capa api (que ya
// importa mcp para montar el handler), de modo que el paquete mcp no necesita
// importar api y no hay ciclo de importación.
//
// Solo reexpone preview (origin=mcp) y la solicitud de aprobación: la ejecución
// real no está acá, porque la capa MCP jamás debe poder ejecutar.
type mcpCore struct {
	svc *Service
}

func (c *mcpCore) Engine() string { return c.svc.Engine() }

func (c *mcpCore) PreviewMCP(ctx context.Context, rawSQL string, op *mcp.BoundedOp) (mcp.PreviewResult, error) {
	var engOp *engine.BoundedOp
	if op != nil {
		// Reutiliza la misma validación/traducción de la operación acotada que
		// la API HTTP, en lugar de duplicar la lógica.
		parsed, err := toBoundedOp(&boundedOpRequest{
			Op:    op.Op,
			Table: op.Table,
			Set:   op.Set,
			Where: op.Where,
		})
		if err != nil {
			return mcp.PreviewResult{}, err
		}
		engOp = &parsed
	}

	res, err := c.svc.PreviewMCP(ctx, rawSQL, engOp)
	if err != nil {
		return mcp.PreviewResult{}, err
	}
	// api.PreviewResult y mcp.PreviewResult tienen los mismos campos: conversión directa.
	return mcp.PreviewResult(res), nil
}

func (c *mcpCore) RequestApproval(ctx context.Context, token string) error {
	return c.svc.RequestApproval(ctx, token)
}

// --- Handlers de aprobación humana ---

// pendingItemResponse es un token pendiente de aprobación, tal como lo ve la UI.
type pendingItemResponse struct {
	Token        string `json:"token"`
	Op           string `json:"op"`
	Table        string `json:"table"`
	AffectedRows int64  `json:"affected_rows"`
	SQL          string `json:"sql"`
	ExpiresInSec int64  `json:"expires_in_sec"`
}

// pendingListResponse es la lista de pendientes.
type pendingListResponse struct {
	Pending []pendingItemResponse `json:"pending"`
}

// listPending devuelve los previews del agente (origin=mcp) a la espera de
// aprobación humana.
func (h *Handler) listPending(w http.ResponseWriter, r *http.Request) {
	items := h.svc.ListPending(r.Context())
	out := pendingListResponse{Pending: make([]pendingItemResponse, 0, len(items))}
	for _, it := range items {
		out.Pending = append(out.Pending, pendingItemResponse{
			Token:        it.Token,
			Op:           it.Op,
			Table:        it.Table,
			AffectedRows: it.AffectedRows,
			SQL:          it.SQL,
			ExpiresInSec: int64(it.ExpiresIn.Seconds()),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// approve ejecuta la propuesta del agente: es el equivalente humano del confirm.
// Solo acá ocurre el COMMIT de un token origin=mcp.
func (h *Handler) approve(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	if token == "" {
		writeError(w, http.StatusBadRequest, errors.New("api: falta el token"))
		return
	}

	res, err := h.svc.Approve(r.Context(), token)
	if err != nil {
		writeError(w, statusForError(err), err)
		return
	}
	writeJSON(w, http.StatusOK, confirmResponse(res))
}

// reject descarta la propuesta del agente sin ejecutar nada.
func (h *Handler) reject(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	if token == "" {
		writeError(w, http.StatusBadRequest, errors.New("api: falta el token"))
		return
	}

	if err := h.svc.Reject(r.Context(), token); err != nil {
		writeError(w, statusForError(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "rejected"})
}

// --- Middleware de auth bearer ---

// bearerAuth exige un header "Authorization: Bearer <token>" que coincida con el
// token esperado. La comparación es en tiempo constante para no filtrar el token
// por timing. Se usa tanto para la superficie MCP (MCP_AUTH_TOKEN) como para la
// superficie humana opcional (UI_AUTH_TOKEN).
func bearerAuth(expected string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got := bearerToken(r)
			if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(expected)) != 1 {
				w.Header().Set("WWW-Authenticate", `Bearer`)
				writeError(w, http.StatusUnauthorized, errors.New("api: credencial inválida o ausente"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// bearerToken extrae el token del header Authorization (esquema Bearer).
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}
