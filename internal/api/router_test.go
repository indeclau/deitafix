package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/indeclau/deitafix/internal/engine"
	"github.com/indeclau/deitafix/internal/guard"
	"github.com/indeclau/deitafix/internal/store"
)

// fakeEngine es un Engine en memoria para testear la capa HTTP sin base de
// datos. Clasifica el SQL con heurística mínima suficiente para los tests y
// devuelve un conteo de filas configurable.
type fakeEngine struct {
	affected   int64
	confirmErr error
	pingErr    error

	lastPreviewSQL string
	lastConfirmSQL string
}

func (f *fakeEngine) Name() string { return "fake" }

func (f *fakeEngine) Parse(sql string) (guard.Statement, error) {
	up := strings.ToUpper(strings.TrimSpace(sql))
	st := guard.Statement{Table: "CollectionBox"}
	switch {
	case strings.HasPrefix(up, "UPDATE"):
		st.Op = guard.OpUpdate
		st.HasWhere = strings.Contains(up, "WHERE")
	case strings.HasPrefix(up, "DELETE"):
		st.Op = guard.OpDelete
		st.HasWhere = strings.Contains(up, "WHERE")
	case strings.HasPrefix(up, "INSERT"):
		st.Op = guard.OpInsert
		st.InsertFromSelect = strings.Contains(up, "SELECT")
	default:
		st.Op = guard.Operation("OTHER")
	}
	return st, nil
}

func (f *fakeEngine) BuildSQL(op engine.BoundedOp) (string, []any, error) {
	// Delegar en el builder real vía un Postgres-like: reconstruimos algo
	// simple y determinista para el test.
	if op.Op == guard.OpUpdate {
		return `UPDATE "CollectionBox" SET "status" = $1 WHERE "id" = $2`,
			[]any{op.Set["status"], op.Where["id"]}, nil
	}
	return `DELETE FROM "CollectionBox" WHERE "id" = $1`, []any{op.Where["id"]}, nil
}

func (f *fakeEngine) Preview(_ context.Context, sql string, _ ...any) (int64, error) {
	f.lastPreviewSQL = sql
	return f.affected, nil
}

func (f *fakeEngine) Confirm(_ context.Context, sql string, _ ...any) (int64, error) {
	f.lastConfirmSQL = sql
	if f.confirmErr != nil {
		return 0, f.confirmErr
	}
	return f.affected, nil
}

func (f *fakeEngine) Ping(_ context.Context) error { return f.pingErr }

func (f *fakeEngine) Close() error { return nil }

// newTestServer arma un router con el fakeEngine y devuelve el server de test.
func newTestServer(t *testing.T, eng engine.Engine, enabled bool, maxRows int64) *httptest.Server {
	t.Helper()
	st := store.New(time.Minute)
	svc := NewService(eng, st, []string{"CollectionBox"}, maxRows)
	srv := httptest.NewServer(NewRouter(svc, RouterConfig{Enabled: enabled}))
	t.Cleanup(srv.Close)
	return srv
}

// postJSON hace un POST con cuerpo JSON y devuelve status + cuerpo decodificado.
func postJSON(t *testing.T, url, body string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(url, "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestPreviewConfirmRawSQLHappyPath(t *testing.T) {
	eng := &fakeEngine{affected: 3}
	srv := newTestServer(t, eng, true, 50)

	// Preview.
	status, body := postJSON(t, srv.URL+"/preview",
		`{"sql":"UPDATE CollectionBox SET status = 1 WHERE id = 42"}`)
	if status != http.StatusOK {
		t.Fatalf("preview status = %d, body = %v", status, body)
	}
	token, _ := body["token"].(string)
	if token == "" {
		t.Fatalf("preview no devolvió token: %v", body)
	}
	if got := body["affected_rows"]; got != float64(3) {
		t.Fatalf("affected_rows = %v, want 3", got)
	}

	// Confirm con el token.
	status, body = postJSON(t, srv.URL+"/confirm", `{"token":"`+token+`"}`)
	if status != http.StatusOK {
		t.Fatalf("confirm status = %d, body = %v", status, body)
	}
	if got := body["affected_rows"]; got != float64(3) {
		t.Fatalf("confirm affected_rows = %v, want 3", got)
	}

	// El token es de un solo uso: reconfirmar debe fallar con 404.
	status, _ = postJSON(t, srv.URL+"/confirm", `{"token":"`+token+`"}`)
	if status != http.StatusNotFound {
		t.Fatalf("segundo confirm status = %d, want 404", status)
	}
}

func TestPreviewBoundedOp(t *testing.T) {
	eng := &fakeEngine{affected: 1}
	srv := newTestServer(t, eng, true, 50)

	status, body := postJSON(t, srv.URL+"/preview",
		`{"operation":{"op":"UPDATE","table":"CollectionBox","set":{"status":1},"where":{"id":42}}}`)
	if status != http.StatusOK {
		t.Fatalf("preview acotado status = %d, body = %v", status, body)
	}
	if _, ok := body["token"].(string); !ok {
		t.Fatalf("preview acotado no devolvió token: %v", body)
	}
}

func TestPreviewRejectsUpdateWithoutWhere(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{affected: 1000}, true, 50)
	status, body := postJSON(t, srv.URL+"/preview", `{"sql":"UPDATE CollectionBox SET status = 1"}`)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422, body = %v", status, body)
	}
}

func TestPreviewRejectsTableNotWhitelisted(t *testing.T) {
	eng := &fakeEngine{affected: 1}
	// Parseamos como tabla no permitida: sobreescribimos el fake para eso.
	srv := newTestServer(t, &tableEngine{fakeEngine: *eng, table: "AuditSensitive"}, true, 50)
	status, _ := postJSON(t, srv.URL+"/preview",
		`{"sql":"UPDATE AuditSensitive SET note = 'x' WHERE id = 1"}`)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", status)
	}
}

func TestPreviewRejectsRowsExceeded(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{affected: 100}, true, 50)
	status, body := postJSON(t, srv.URL+"/preview",
		`{"sql":"DELETE FROM CollectionBox WHERE status = 0"}`)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422, body = %v", status, body)
	}
}

func TestPreviewRejectsInsertFromSelect(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{affected: 1}, true, 50)
	status, _ := postJSON(t, srv.URL+"/preview",
		`{"sql":"INSERT INTO CollectionBox SELECT * FROM other"}`)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", status)
	}
}

func TestPreviewAmbiguousInput(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{affected: 1}, true, 50)
	status, _ := postJSON(t, srv.URL+"/preview",
		`{"sql":"DELETE FROM CollectionBox WHERE id=1","operation":{"op":"DELETE","table":"CollectionBox","where":{"id":1}}}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
}

func TestFeatureFlagDisabled(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{affected: 1}, false, 50)
	status, _ := postJSON(t, srv.URL+"/preview", `{"sql":"DELETE FROM CollectionBox WHERE id=1"}`)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", status)
	}
}

func TestConfirmRejectsUnknownToken(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{affected: 1}, true, 50)
	status, _ := postJSON(t, srv.URL+"/confirm", `{"token":"deadbeef"}`)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
}

func TestConfirmRejectsMissingToken(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{affected: 1}, true, 50)
	status, _ := postJSON(t, srv.URL+"/confirm", `{}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
}

func TestHealthzAlwaysOK(t *testing.T) {
	// Liveness no toca la base: responde 200 aunque el ping falle y aunque el
	// feature flag esté apagado.
	srv := newTestServer(t, &fakeEngine{pingErr: errors.New("db caída")}, false, 50)
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", resp.StatusCode)
	}
}

func TestReadyzReflectsDB(t *testing.T) {
	t.Run("base alcanzable -> 200", func(t *testing.T) {
		srv := newTestServer(t, &fakeEngine{}, true, 50)
		resp, err := http.Get(srv.URL + "/readyz")
		if err != nil {
			t.Fatalf("GET /readyz: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("readyz status = %d, want 200", resp.StatusCode)
		}
	})

	t.Run("base caída -> 503", func(t *testing.T) {
		srv := newTestServer(t, &fakeEngine{pingErr: errors.New("db caída")}, true, 50)
		resp, err := http.Get(srv.URL + "/readyz")
		if err != nil {
			t.Fatalf("GET /readyz: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("readyz status = %d, want 503", resp.StatusCode)
		}
	})
}

// TestServesUIOnRoot verifica que la UI embebida se sirve montada en el mismo
// router que la API: GET / devuelve el HTML y los estáticos (Alpine) se sirven.
// La UI convive con /preview y /confirm sin alterar su contrato.
func TestServesUIOnRoot(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{affected: 1}, true, 50)

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html…", ct)
	}
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	if !strings.Contains(buf.String(), "<title>Deitafix</title>") {
		t.Fatalf("GET / no devolvió el HTML embebido")
	}
	// El motor real del servidor se inyecta como indicador read-only.
	if !strings.Contains(buf.String(), `data-engine="fake"`) {
		t.Fatalf("GET / no inyectó el motor del servidor en el HTML")
	}
}

func TestServesUIStatic(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{affected: 1}, true, 50)

	resp, err := http.Get(srv.URL + "/static/alpine.min.js")
	if err != nil {
		t.Fatalf("GET /static/alpine.min.js: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /static/alpine.min.js status = %d, want 200", resp.StatusCode)
	}
}

// TestUIServedEvenWhenDisabled comprueba que la UI carga aunque el feature flag
// esté apagado: la página debe reflejar el estado deshabilitado, no fallar de
// forma opaca. Las rutas de escritura sí devuelven 503.
func TestUIServedEvenWhenDisabled(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{affected: 1}, false, 50)

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("con flag off, GET / status = %d, want 200", resp.StatusCode)
	}
}

// tableEngine es un fakeEngine que fuerza un nombre de tabla concreto al
// parsear, para testear el rechazo por whitelist.
type tableEngine struct {
	fakeEngine
	table string
}

func (t *tableEngine) Parse(sql string) (guard.Statement, error) {
	st, err := t.fakeEngine.Parse(sql)
	st.Table = t.table
	return st, err
}
