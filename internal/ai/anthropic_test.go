package ai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeMessagesServer arma un httptest.Server que imita la Messages API de
// Anthropic devolviendo un bloque de texto con el body dado. NUNCA se llama al
// proveedor real: todos los tests van contra este server local.
func fakeMessagesServer(t *testing.T, status int, textBlock string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verificamos que el cliente arma bien la request (headers de auth/versión).
		if r.Header.Get("X-Api-Key") == "" {
			t.Errorf("falta header X-Api-Key")
		}
		if r.Header.Get("Anthropic-Version") == "" {
			t.Errorf("falta header Anthropic-Version")
		}
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path inesperado: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		resp := map[string]any{
			"stop_reason": "end_turn",
			"content": []map[string]any{
				{"type": "text", "text": textBlock},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newTestClient construye un cliente real apuntado al server de test.
func newTestClient(t *testing.T, srv *httptest.Server) Client {
	t.Helper()
	return NewAnthropic(Config{
		APIKey:     "test-key",
		Model:      "test-model",
		BaseURL:    srv.URL,
		Timeout:    2 * time.Second,
		HTTPClient: srv.Client(),
	})
}

func TestSuggestSQLParsesResponse(t *testing.T) {
	// Respuesta bien formada: un objeto JSON con sql + rationale.
	srv := fakeMessagesServer(t, http.StatusOK,
		`{"sql":"UPDATE \"CollectionBox\" SET status = 1 WHERE id = 42","rationale":"actualiza el registro pedido"}`)
	c := newTestClient(t, srv)

	res, err := c.SuggestSQL(context.Background(), SuggestRequest{
		Engine: "postgres",
		Intent: "marcá como listo el registro 42",
	})
	if err != nil {
		t.Fatalf("SuggestSQL: %v", err)
	}
	if !strings.HasPrefix(res.SQL, "UPDATE") {
		t.Fatalf("SQL inesperado: %q", res.SQL)
	}
	if res.Rationale == "" {
		t.Fatal("se esperaba un rationale no vacío")
	}
}

func TestSuggestSQLExtractsJSONFromProse(t *testing.T) {
	// El modelo envuelve el JSON en prosa y un bloque markdown: igual debe
	// extraerse el objeto de forma defensiva.
	body := "Claro, acá va la sentencia:\n```json\n" +
		`{"sql":"DELETE FROM \"CollectionBox\" WHERE id = 7","rationale":"borra el 7"}` +
		"\n```\nEspero que sirva."
	srv := fakeMessagesServer(t, http.StatusOK, body)
	c := newTestClient(t, srv)

	res, err := c.SuggestSQL(context.Background(), SuggestRequest{Engine: "postgres", Intent: "borrá el 7"})
	if err != nil {
		t.Fatalf("SuggestSQL: %v", err)
	}
	if !strings.HasPrefix(res.SQL, "DELETE") {
		t.Fatalf("SQL inesperado: %q", res.SQL)
	}
}

func TestSuggestSQLMalformedResponse(t *testing.T) {
	// El modelo devuelve algo sin ningún objeto JSON: debe degradar con error
	// controlado, no crashear.
	srv := fakeMessagesServer(t, http.StatusOK, "no tengo idea, no hay JSON acá")
	c := newTestClient(t, srv)

	_, err := c.SuggestSQL(context.Background(), SuggestRequest{Engine: "postgres", Intent: "algo"})
	if err == nil {
		t.Fatal("se esperaba error por respuesta malformada")
	}
}

func TestSuggestSQLEmptySQL(t *testing.T) {
	// JSON válido pero con sql vacío: es un error controlado (el modelo no
	// propuso sentencia).
	srv := fakeMessagesServer(t, http.StatusOK, `{"sql":"","rationale":"no supe"}`)
	c := newTestClient(t, srv)

	_, err := c.SuggestSQL(context.Background(), SuggestRequest{Engine: "postgres", Intent: "algo"})
	if err == nil {
		t.Fatal("se esperaba error por sql vacío")
	}
}

func TestSuggestSQLProviderError(t *testing.T) {
	// El proveedor devuelve 500: error controlado.
	srv := fakeMessagesServer(t, http.StatusInternalServerError, "boom")
	c := newTestClient(t, srv)

	_, err := c.SuggestSQL(context.Background(), SuggestRequest{Engine: "postgres", Intent: "algo"})
	if err == nil {
		t.Fatal("se esperaba error por 500 del proveedor")
	}
}

func TestExplainImpactDegradesOnMalformed(t *testing.T) {
	// ExplainImpact es best-effort: ante respuesta malformada, degrada a un
	// texto neutro con riesgo medium, sin error.
	srv := fakeMessagesServer(t, http.StatusOK, "sin json")
	c := newTestClient(t, srv)

	exp, err := c.ExplainImpact(context.Background(), ExplainRequest{
		Engine: "postgres", SQL: "DELETE FROM t WHERE id=1", Op: "DELETE", Table: "t", AffectedRows: 1, MaxRows: 50,
	})
	if err != nil {
		t.Fatalf("ExplainImpact no debía fallar (best-effort): %v", err)
	}
	if exp.RiskLevel != RiskMedium {
		t.Fatalf("riesgo degradado = %q, want medium", exp.RiskLevel)
	}
	if exp.Text == "" {
		t.Fatal("se esperaba un texto neutro de degradación")
	}
}

func TestReviewStatementParsesFlags(t *testing.T) {
	srv := fakeMessagesServer(t, http.StatusOK,
		`{"flags":[{"severity":"danger","message":"WHERE demasiado amplio"},{"severity":"info","message":"ok"}]}`)
	c := newTestClient(t, srv)

	rev, err := c.ReviewStatement(context.Background(), ReviewRequest{
		Engine: "postgres", SQL: "DELETE FROM t WHERE 1=1", Op: "DELETE", Table: "t", AffectedRows: 100, MaxRows: 50,
	})
	if err != nil {
		t.Fatalf("ReviewStatement: %v", err)
	}
	if len(rev.Flags) != 2 {
		t.Fatalf("flags = %d, want 2", len(rev.Flags))
	}
	if rev.Flags[0].Severity != SeverityDanger {
		t.Fatalf("severidad = %q, want danger", rev.Flags[0].Severity)
	}
}

func TestReviewStatementDegradesToEmpty(t *testing.T) {
	// Sin JSON interpretable, el revisor devuelve una review vacía (mejor no
	// marcar nada que inventar), sin error.
	srv := fakeMessagesServer(t, http.StatusOK, "no hay flags acá")
	c := newTestClient(t, srv)

	rev, err := c.ReviewStatement(context.Background(), ReviewRequest{Engine: "postgres", SQL: "x", Op: "UPDATE", Table: "t"})
	if err != nil {
		t.Fatalf("ReviewStatement no debía fallar: %v", err)
	}
	if len(rev.Flags) != 0 {
		t.Fatalf("flags = %d, want 0", len(rev.Flags))
	}
}

func TestClientRespectsOwnTimeout(t *testing.T) {
	// El server tarda más que el timeout del cliente: la llamada debe cortar por
	// timeout, sin colgarse.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(slow.Close)

	c := NewAnthropic(Config{
		APIKey:     "k",
		BaseURL:    slow.URL,
		Timeout:    20 * time.Millisecond,
		HTTPClient: slow.Client(),
	})

	_, err := c.SuggestSQL(context.Background(), SuggestRequest{Engine: "postgres", Intent: "x"})
	if err == nil {
		t.Fatal("se esperaba error por timeout propio de la IA")
	}
}

func TestExtractJSONObject(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"plano", `{"a":1}`, `{"a":1}`, true},
		{"con prosa", "texto {\"a\":1} más texto", `{"a":1}`, true},
		{"anidado", `pre {"a":{"b":2}} post`, `{"a":{"b":2}}`, true},
		{"llave en string", `{"a":"}"}`, `{"a":"}"}`, true},
		{"llave escapada en string", `{"a":"\"}"}`, `{"a":"\"}"}`, true},
		{"sin objeto", "no hay nada", "", false},
		{"vacío", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := extractJSONObject(tt.in)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNewAnthropicDefaults(t *testing.T) {
	// Sin overrides, se aplican los defaults documentados.
	c := NewAnthropic(Config{APIKey: "k"}).(*anthropicClient)
	if c.model != DefaultModel {
		t.Fatalf("model default = %q, want %q", c.model, DefaultModel)
	}
	if c.baseURL != DefaultBaseURL {
		t.Fatalf("baseURL default = %q, want %q", c.baseURL, DefaultBaseURL)
	}
	if c.timeout != DefaultTimeout {
		t.Fatalf("timeout default = %v, want %v", c.timeout, DefaultTimeout)
	}
	if !c.Enabled() {
		t.Fatal("el cliente real debe estar Enabled")
	}
}
