// Copyright 2026 indeclau. Licensed under the Apache License, Version 2.0.
// See the LICENSE file in the repository root for the full license text.

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/indeclau/deitafix/internal/store"
)

// newMCPService arma un Service con un fakeEngine y whitelist CollectionBox,
// devolviendo también el store y el engine para inspeccionar el estado real.
func newMCPService(t *testing.T, eng *fakeEngine, maxRows int64) (*Service, *store.Store) {
	t.Helper()
	st := store.New(time.Minute)
	return NewService(eng, st, []string{"CollectionBox"}, maxRows), st
}

// TestConfirmGatingMCPToken verifica el gating central: un token creado con
// origin=mcp NO se ejecuta por el confirm humano directo. Confirm debe devolver
// ErrMCPRequiresApproval (que la capa HTTP mapea a 409) y la base no se toca.
func TestConfirmGatingMCPToken(t *testing.T) {
	eng := &fakeEngine{affected: 1}
	svc, _ := newMCPService(t, eng, 50)

	res, err := svc.PreviewMCP(context.Background(),
		`UPDATE CollectionBox SET status = 1 WHERE id = 42`, nil)
	if err != nil {
		t.Fatalf("PreviewMCP: %v", err)
	}

	_, err = svc.Confirm(context.Background(), res.Token)
	if err == nil {
		t.Fatal("Confirm de token mcp debía fallar, got nil")
	}
	if err != ErrMCPRequiresApproval {
		t.Fatalf("Confirm error = %v, want ErrMCPRequiresApproval", err)
	}
	// La base no se ejecutó.
	if eng.lastConfirmSQL != "" {
		t.Fatalf("Confirm ejecutó SQL para token mcp: %q", eng.lastConfirmSQL)
	}

	// El confirm rechazado NO debe haber consumido el token: la propuesta sigue
	// viva y aprobable por un humano (no se puede griefear con /confirm).
	if err := svc.RequestApproval(context.Background(), res.Token); err != nil {
		t.Fatalf("el token mcp fue consumido por un Confirm rechazado: %v", err)
	}
	if _, err := svc.Approve(context.Background(), res.Token); err != nil {
		t.Fatalf("Approve tras Confirm rechazado: %v", err)
	}
}

// TestRequestApprovalNeverExecutes comprueba que solicitar la confirmación (lo
// que hace la herramienta MCP confirm) NO ejecuta nada contra la base: solo
// mueve el token a pending_approval.
func TestRequestApprovalNeverExecutes(t *testing.T) {
	eng := &fakeEngine{affected: 3}
	svc, _ := newMCPService(t, eng, 50)

	res, err := svc.PreviewMCP(context.Background(),
		`DELETE FROM CollectionBox WHERE status = 0`, nil)
	if err != nil {
		t.Fatalf("PreviewMCP: %v", err)
	}

	if err := svc.RequestApproval(context.Background(), res.Token); err != nil {
		t.Fatalf("RequestApproval: %v", err)
	}
	if eng.lastConfirmSQL != "" {
		t.Fatalf("RequestApproval ejecutó SQL: %q", eng.lastConfirmSQL)
	}

	// El token queda listado como pendiente.
	pend := svc.ListPending(context.Background())
	if len(pend) != 1 {
		t.Fatalf("ListPending = %d items, want 1", len(pend))
	}
	if pend[0].Token != res.Token {
		t.Fatalf("token pendiente = %q, want %q", pend[0].Token, res.Token)
	}
}

// TestApproveExecutesAndConsumes verifica el flujo humano completo: preview MCP
// → RequestApproval → Approve ejecuta (COMMIT) y consume el token (un solo uso).
func TestApproveExecutesAndConsumes(t *testing.T) {
	eng := &fakeEngine{affected: 2}
	svc, _ := newMCPService(t, eng, 50)

	res, _ := svc.PreviewMCP(context.Background(),
		`UPDATE CollectionBox SET status = 1 WHERE id = 42`, nil)
	if err := svc.RequestApproval(context.Background(), res.Token); err != nil {
		t.Fatalf("RequestApproval: %v", err)
	}

	out, err := svc.Approve(context.Background(), res.Token)
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if out.AffectedRows != 2 {
		t.Fatalf("Approve affected = %d, want 2", out.AffectedRows)
	}
	if eng.lastConfirmSQL == "" {
		t.Fatal("Approve no ejecutó SQL")
	}

	// Segundo Approve: token consumido → ErrNotFound.
	if _, err := svc.Approve(context.Background(), res.Token); err != store.ErrNotFound {
		t.Fatalf("segundo Approve error = %v, want ErrNotFound", err)
	}
}

// TestApproveRequiresPendingState comprueba que un token en previewed (aún no
// propuesto para aprobación) no se puede aprobar: ErrWrongState (→ 409).
func TestApproveRequiresPendingState(t *testing.T) {
	eng := &fakeEngine{affected: 1}
	svc, _ := newMCPService(t, eng, 50)

	res, _ := svc.PreviewMCP(context.Background(),
		`UPDATE CollectionBox SET status = 1 WHERE id = 42`, nil)

	// Sin RequestApproval previo.
	if _, err := svc.Approve(context.Background(), res.Token); err != store.ErrWrongState {
		t.Fatalf("Approve sin pending error = %v, want ErrWrongState", err)
	}
	if eng.lastConfirmSQL != "" {
		t.Fatalf("Approve ejecutó SQL sobre token no pendiente: %q", eng.lastConfirmSQL)
	}
}

// TestRejectDiscardsWithoutExecuting verifica que rechazar consume el token y no
// ejecuta nada.
func TestRejectDiscardsWithoutExecuting(t *testing.T) {
	eng := &fakeEngine{affected: 1}
	svc, _ := newMCPService(t, eng, 50)

	res, _ := svc.PreviewMCP(context.Background(),
		`DELETE FROM CollectionBox WHERE id = 1`, nil)
	_ = svc.RequestApproval(context.Background(), res.Token)

	if err := svc.Reject(context.Background(), res.Token); err != nil {
		t.Fatalf("Reject: %v", err)
	}
	if eng.lastConfirmSQL != "" {
		t.Fatalf("Reject ejecutó SQL: %q", eng.lastConfirmSQL)
	}
	if len(svc.ListPending(context.Background())) != 0 {
		t.Fatal("el token rechazado sigue en la lista de pendientes")
	}
	// Aprobar algo ya rechazado → ErrNotFound (consumido).
	if _, err := svc.Approve(context.Background(), res.Token); err != store.ErrNotFound {
		t.Fatalf("Approve tras Reject = %v, want ErrNotFound", err)
	}
}

// TestMCPGuardsRejectUnsafe verifica que las guardas aplican idénticas vía el
// preview del agente: UPDATE sin WHERE, tope de filas y whitelist se rechazan.
func TestMCPGuardsRejectUnsafe(t *testing.T) {
	t.Run("UPDATE sin WHERE", func(t *testing.T) {
		svc, _ := newMCPService(t, &fakeEngine{affected: 1000}, 50)
		if _, err := svc.PreviewMCP(context.Background(),
			`UPDATE CollectionBox SET status = 1`, nil); err == nil {
			t.Fatal("preview mcp de UPDATE sin WHERE debía fallar")
		}
	})

	t.Run("tope de filas", func(t *testing.T) {
		svc, _ := newMCPService(t, &fakeEngine{affected: 100}, 50)
		if _, err := svc.PreviewMCP(context.Background(),
			`DELETE FROM CollectionBox WHERE status = 0`, nil); err == nil {
			t.Fatal("preview mcp que supera el tope debía fallar")
		}
	})

	t.Run("tabla fuera de whitelist", func(t *testing.T) {
		eng := &tableEngine{fakeEngine: fakeEngine{affected: 1}, table: "AuditSensitive"}
		st := store.New(time.Minute)
		svc := NewService(eng, st, []string{"CollectionBox"}, 50)
		if _, err := svc.PreviewMCP(context.Background(),
			`UPDATE AuditSensitive SET note = 'x' WHERE id = 1`, nil); err == nil {
			t.Fatal("preview mcp fuera de whitelist debía fallar")
		}
	})
}

// --- Tests HTTP: superficie de aprobación + gating + auth ---

// newApprovalServer arma un router con MCP habilitado (y opcionalmente
// UI_AUTH_TOKEN) y devuelve el server y el service para preparar estado.
func newApprovalServer(t *testing.T, eng *fakeEngine, uiToken string) (*httptest.Server, *Service) {
	t.Helper()
	st := store.New(time.Minute)
	svc := NewService(eng, st, []string{"CollectionBox"}, 50)
	srv := httptest.NewServer(NewRouter(svc, RouterConfig{
		Enabled:      true,
		MCPEnabled:   true,
		MCPAuthToken: "mcp-secret",
		MCPPath:      "/mcp",
		UIAuthToken:  uiToken,
	}))
	t.Cleanup(srv.Close)
	return srv, svc
}

// TestConfirmHTTPGatingReturns409 verifica el gating end-to-end sobre HTTP: un
// token origin=mcp enviado a POST /confirm devuelve 409.
func TestConfirmHTTPGatingReturns409(t *testing.T) {
	eng := &fakeEngine{affected: 1}
	srv, svc := newApprovalServer(t, eng, "")

	res, _ := svc.PreviewMCP(context.Background(),
		`UPDATE CollectionBox SET status = 1 WHERE id = 42`, nil)

	status, _ := postJSON(t, srv.URL+"/confirm", `{"token":"`+res.Token+`"}`)
	if status != http.StatusConflict {
		t.Fatalf("POST /confirm con token mcp status = %d, want 409", status)
	}
	if eng.lastConfirmSQL != "" {
		t.Fatalf("el confirm HTTP ejecutó SQL de un token mcp: %q", eng.lastConfirmSQL)
	}
}

// TestPendingApproveRejectHTTP recorre la superficie humana sobre HTTP:
// listar pendientes → aprobar (ejecuta) y rechazar (descarta).
func TestPendingApproveRejectHTTP(t *testing.T) {
	eng := &fakeEngine{affected: 2}
	srv, svc := newApprovalServer(t, eng, "")

	// Dos propuestas del agente, ambas pendientes de aprobación.
	a, _ := svc.PreviewMCP(context.Background(), `UPDATE CollectionBox SET status = 1 WHERE id = 1`, nil)
	b, _ := svc.PreviewMCP(context.Background(), `DELETE FROM CollectionBox WHERE id = 2`, nil)
	_ = svc.RequestApproval(context.Background(), a.Token)
	_ = svc.RequestApproval(context.Background(), b.Token)

	// GET /pending lista ambas.
	resp, err := http.Get(srv.URL + "/pending")
	if err != nil {
		t.Fatalf("GET /pending: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /pending status = %d, want 200", resp.StatusCode)
	}

	// Aprobar la primera → ejecuta.
	status, _ := postJSON(t, srv.URL+"/pending/"+a.Token+"/approve", `{}`)
	if status != http.StatusOK {
		t.Fatalf("approve status = %d, want 200", status)
	}
	if eng.lastConfirmSQL == "" {
		t.Fatal("approve no ejecutó SQL")
	}

	// Rechazar la segunda → no ejecuta (limpiamos el rastro y verificamos).
	eng.lastConfirmSQL = ""
	status, _ = postJSON(t, srv.URL+"/pending/"+b.Token+"/reject", `{}`)
	if status != http.StatusOK {
		t.Fatalf("reject status = %d, want 200", status)
	}
	if eng.lastConfirmSQL != "" {
		t.Fatalf("reject ejecutó SQL: %q", eng.lastConfirmSQL)
	}

	// Ambos tokens consumidos: la lista queda vacía.
	if n := len(svc.ListPending(context.Background())); n != 0 {
		t.Fatalf("quedan %d pendientes tras aprobar+rechazar, want 0", n)
	}
}

// TestMCPEndpointRequiresBearer comprueba que el endpoint MCP exige el bearer:
// sin credencial responde 401, y no se registra si MCP está deshabilitado.
func TestMCPEndpointRequiresBearer(t *testing.T) {
	srv, _ := newApprovalServer(t, &fakeEngine{affected: 1}, "")

	// POST /mcp sin Authorization → 401.
	resp, err := http.Post(srv.URL+"/mcp", "application/json",
		bytesReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	if err != nil {
		t.Fatalf("POST /mcp: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("POST /mcp sin bearer status = %d, want 401", resp.StatusCode)
	}
}

// TestUIAuthTokenProtectsHumanSurface verifica que, con UI_AUTH_TOKEN seteado,
// la superficie humana (confirm/pending) exige el bearer y la credencial MCP no
// alcanza para operarla.
func TestUIAuthTokenProtectsHumanSurface(t *testing.T) {
	srv, _ := newApprovalServer(t, &fakeEngine{affected: 1}, "ui-secret")

	// Sin bearer → 401.
	resp, err := http.Get(srv.URL + "/pending")
	if err != nil {
		t.Fatalf("GET /pending: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /pending sin bearer status = %d, want 401", resp.StatusCode)
	}

	// La credencial MCP NO abre la superficie humana.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/pending", nil)
	req.Header.Set("Authorization", "Bearer mcp-secret")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /pending con bearer mcp: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /pending con credencial MCP status = %d, want 401", resp.StatusCode)
	}

	// Con el bearer humano correcto → 200.
	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/pending", nil)
	req.Header.Set("Authorization", "Bearer ui-secret")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /pending con bearer ui: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /pending con bearer ui status = %d, want 200", resp.StatusCode)
	}
}

// TestMCPDisabledNoEndpoint comprueba que con MCP deshabilitado el endpoint /mcp
// no se registra (cae al catch-all de la UI → 404), y el resto del servicio
// funciona igual.
func TestMCPDisabledNoEndpoint(t *testing.T) {
	st := store.New(time.Minute)
	svc := NewService(&fakeEngine{affected: 1}, st, []string{"CollectionBox"}, 50)
	srv := httptest.NewServer(NewRouter(svc, RouterConfig{Enabled: true, MCPEnabled: false, MCPPath: "/mcp"}))
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/mcp", "application/json", bytesReader(`{}`))
	if err != nil {
		t.Fatalf("POST /mcp: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("con MCP off, POST /mcp status = %d, want 404 (catch-all UI)", resp.StatusCode)
	}

	// El flujo humano sigue vivo.
	status, _ := postJSON(t, srv.URL+"/preview", `{"sql":"UPDATE CollectionBox SET status = 1 WHERE id = 1"}`)
	if status != http.StatusOK {
		t.Fatalf("preview con MCP off status = %d, want 200", status)
	}
}

// bytesReader es un helper mínimo para cuerpos de POST en los tests HTTP.
func bytesReader(s string) *strings.Reader { return strings.NewReader(s) }
