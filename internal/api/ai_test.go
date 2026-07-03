// Copyright 2026 indeclau. Licensed under the Apache License, Version 2.0.
// See the LICENSE file in the repository root for the full license text.

package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/indeclau/deitafix/internal/ai"
	"github.com/indeclau/deitafix/internal/guard"
	"github.com/indeclau/deitafix/internal/store"
)

// fakeAI es un ai.Client configurable para testear la capa HTTP/servicio sin
// tocar ningún proveedor real. Cada método se controla con campos, incluyendo
// forzar errores o un timeout, para ejercitar la degradación y el aislamiento.
type fakeAI struct {
	enabled bool

	suggestSQL       string
	suggestRationale string
	suggestErr       error

	explainText string
	explainRisk ai.RiskLevel
	explainErr  error
	explainWait time.Duration // simula latencia (para probar timeout/aislamiento)

	reviewFlags []ai.Flag
	reviewErr   error
}

func (f *fakeAI) Enabled() bool { return f.enabled }

func (f *fakeAI) SuggestSQL(_ context.Context, _ ai.SuggestRequest) (ai.SuggestResult, error) {
	if f.suggestErr != nil {
		return ai.SuggestResult{}, f.suggestErr
	}
	return ai.SuggestResult{SQL: f.suggestSQL, Rationale: f.suggestRationale}, nil
}

func (f *fakeAI) ExplainImpact(ctx context.Context, _ ai.ExplainRequest) (ai.Explanation, error) {
	if f.explainWait > 0 {
		// Respeta la cancelación del context (el enriquecimiento tiene su propio
		// timeout): si expira, devolvemos el error del context.
		select {
		case <-time.After(f.explainWait):
		case <-ctx.Done():
			return ai.Explanation{}, ctx.Err()
		}
	}
	if f.explainErr != nil {
		return ai.Explanation{}, f.explainErr
	}
	return ai.Explanation{Text: f.explainText, RiskLevel: f.explainRisk}, nil
}

func (f *fakeAI) ReviewStatement(_ context.Context, _ ai.ReviewRequest) (ai.Review, error) {
	if f.reviewErr != nil {
		return ai.Review{}, f.reviewErr
	}
	return ai.Review{Flags: f.reviewFlags}, nil
}

// aiPreviewTimeoutForTest acorta el timeout del enriquecimiento y devuelve una
// función para restaurarlo. Permite forzar la ruta de timeout sin esperar los
// 15s de producción.
func aiPreviewTimeoutForTest(d time.Duration) func() {
	prev := aiPreviewTimeout
	aiPreviewTimeout = d
	return func() { aiPreviewTimeout = prev }
}

// newAITestServer arma un router con el fakeEngine y un ai.Client dado.
func newAITestServer(t *testing.T, eng *fakeEngine, aiClient ai.Client) *httptest.Server {
	t.Helper()
	st := store.New(time.Minute)
	svc := NewServiceWithAI(eng, st, []string{"CollectionBox"}, 50, aiClient, nil)
	srv := httptest.NewServer(NewRouter(svc, RouterConfig{Enabled: true}))
	t.Cleanup(srv.Close)
	return srv
}

// --- Degradación limpia ---

// TestAISuggestDisabledReturns503 verifica que /ai/suggest degrada limpio (503,
// no 500) cuando la IA no está configurada.
func TestAISuggestDisabledReturns503(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{affected: 1}, true, 50) // NewService => IA disabled
	status, body := postJSON(t, srv.URL+"/ai/suggest", `{"intent":"borrá el registro 5"}`)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body = %v", status, body)
	}
}

// TestPreviewAINullWhenDisabled verifica que con IA apagada el preview devuelve
// "ai": null y el resto de la respuesta intacto (token + affected_rows).
func TestPreviewAINullWhenDisabled(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{affected: 3}, true, 50)
	status, body := postJSON(t, srv.URL+"/preview",
		`{"sql":"UPDATE CollectionBox SET status = 1 WHERE id = 42"}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %v", status, body)
	}
	if body["token"] == nil || body["token"] == "" {
		t.Fatalf("preview sin token: %v", body)
	}
	if got := body["affected_rows"]; got != float64(3) {
		t.Fatalf("affected_rows = %v, want 3", got)
	}
	// El campo ai debe estar presente y ser null.
	if v, ok := body["ai"]; !ok || v != nil {
		t.Fatalf("ai = %v (present=%v), want null", v, ok)
	}
}

// --- NL → SQL feliz ---

func TestAISuggestHappyPath(t *testing.T) {
	f := &fakeAI{
		enabled:          true,
		suggestSQL:       `UPDATE "CollectionBox" SET status = 1 WHERE id = 42`,
		suggestRationale: "actualiza el registro pedido",
	}
	srv := newAITestServer(t, &fakeEngine{affected: 1}, f)

	status, body := postJSON(t, srv.URL+"/ai/suggest", `{"intent":"marcá listo el 42"}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %v", status, body)
	}
	if sql, _ := body["sql"].(string); sql == "" {
		t.Fatalf("se esperaba un sql candidato: %v", body)
	}
	// La respuesta deja claro que el candidato NO está validado.
	if note, _ := body["note"].(string); note == "" {
		t.Fatalf("se esperaba una nota de 'sin validar': %v", body)
	}
}

func TestAISuggestRequiresIntent(t *testing.T) {
	f := &fakeAI{enabled: true, suggestSQL: "x"}
	srv := newAITestServer(t, &fakeEngine{affected: 1}, f)
	status, _ := postJSON(t, srv.URL+"/ai/suggest", `{"intent":"   "}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (intent vacío)", status)
	}
}

// --- INVARIANTE DE SEGURIDAD (el test más importante) ---

// TestAIGeneratedSQLPassesGuards prueba que el SQL generado por IA pasa por LAS
// MISMAS guardas que el SQL humano: un DELETE sin WHERE propuesto por el fake es
// rechazado por /preview con 422, exactamente igual que si lo hubiera escrito
// una persona. La IA solo propone; el gate lo aplica el preview.
func TestAIGeneratedSQLPassesGuards(t *testing.T) {
	// El fake "propone" un DELETE sin WHERE (viola la guarda ErrMissingWhere).
	f := &fakeAI{
		enabled:    true,
		suggestSQL: "DELETE FROM CollectionBox",
	}
	srv := newAITestServer(t, &fakeEngine{affected: 1000}, f)

	// 1. La sugerencia se genera igual (la IA no valida; solo propone).
	status, body := postJSON(t, srv.URL+"/ai/suggest", `{"intent":"borrá todo"}`)
	if status != http.StatusOK {
		t.Fatalf("/ai/suggest status = %d, body = %v", status, body)
	}
	candidate, _ := body["sql"].(string)
	if candidate == "" {
		t.Fatalf("no se obtuvo el SQL candidato: %v", body)
	}

	// 2. Ese candidato, enviado a /preview como cualquier SQL, es RECHAZADO por
	//    las guardas con 422 (DELETE sin WHERE). No hay atajo para la IA.
	status, body = postJSON(t, srv.URL+"/preview", `{"sql":"`+candidate+`"}`)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("preview del candidato IA status = %d, want 422 (guarda), body = %v", status, body)
	}
}

// --- AISLAMIENTO: un fallo/timeout de IA NO rompe el preview ---

// TestPreviewIAErrorDoesNotBreakPreview verifica que un error de la IA en el
// enriquecimiento no rompe el preview: sigue devolviendo token + affected_rows,
// con la señal de riesgo heurística (aunque el modelo haya fallado).
func TestPreviewIAErrorDoesNotBreakPreview(t *testing.T) {
	f := &fakeAI{
		enabled:    true,
		explainErr: errors.New("proveedor caído"),
		reviewErr:  errors.New("proveedor caído"),
	}
	srv := newAITestServer(t, &fakeEngine{affected: 40}, f) // 40/50 = 0.8 => high

	status, body := postJSON(t, srv.URL+"/preview",
		`{"sql":"UPDATE CollectionBox SET status = 1 WHERE id = 42"}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %v", status, body)
	}
	if body["token"] == nil || body["token"] == "" {
		t.Fatalf("preview sin token pese a IA caída: %v", body)
	}
	if got := body["affected_rows"]; got != float64(40) {
		t.Fatalf("affected_rows = %v, want 40", got)
	}
	// El bloque ai existe (IA habilitada) con el riesgo heurístico; la
	// explicación queda vacía porque el modelo falló, pero no rompe nada.
	aiBlock, ok := body["ai"].(map[string]any)
	if !ok {
		t.Fatalf("se esperaba un bloque ai con el riesgo heurístico, got %v", body["ai"])
	}
	if aiBlock["risk_level"] != string(ai.RiskHigh) {
		t.Fatalf("risk_level = %v, want high (heurística 40/50)", aiBlock["risk_level"])
	}
}

// TestPreviewIATimeoutDoesNotBreakPreview: si ExplainImpact tarda y su context
// se cancela por el timeout del enriquecimiento, el preview igual devuelve token
// + affected_rows. Simulamos el timeout con un explainWait mayor que el propio
// context del enriquecimiento: el fake respeta ctx.Done() y devuelve un error de
// deadline, que el servicio absorbe (no lo propaga al preview).
func TestPreviewIATimeoutDoesNotBreakPreview(t *testing.T) {
	f := &fakeAI{
		enabled:     true,
		explainWait: 30 * time.Second, // no llega a cumplirse: ctx se cancela antes
		explainText: "no debería llegar",
		explainRisk: ai.RiskLow,
	}
	// aiPreviewTimeout (15s) es demasiado largo para un test; lo acortamos en
	// caliente para forzar la cancelación del enriquecimiento rápido, sin esperar.
	restore := aiPreviewTimeoutForTest(50 * time.Millisecond)
	defer restore()

	srv := newAITestServer(t, &fakeEngine{affected: 1}, f)

	start := time.Now()
	status, body := postJSON(t, srv.URL+"/preview",
		`{"sql":"UPDATE CollectionBox SET status = 1 WHERE id = 42"}`)
	elapsed := time.Since(start)

	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %v", status, body)
	}
	if body["token"] == nil || body["token"] == "" {
		t.Fatalf("preview sin token pese a timeout de IA: %v", body)
	}
	if got := body["affected_rows"]; got != float64(1) {
		t.Fatalf("affected_rows = %v, want 1", got)
	}
	// El preview no debió bloquearse los 30s del fake: cortó por el timeout del
	// enriquecimiento (con amplio margen para la lentitud del CI).
	if elapsed > 5*time.Second {
		t.Fatalf("el preview tardó %v: la IA lo bloqueó (aislamiento roto)", elapsed)
	}
}

// --- ai:false omite el enriquecimiento ---

// TestPreviewAIFalseSkipsEnrichment: con "ai": false en el body, el preview no
// llama a la IA (aunque esté habilitada) y devuelve ai: null.
func TestPreviewAIFalseSkipsEnrichment(t *testing.T) {
	// Si el enriquecimiento se llamara, este fake devolvería una explicación;
	// como ai:false lo omite, el bloque ai debe ser null.
	f := &fakeAI{
		enabled:     true,
		explainText: "no debería aparecer",
		explainRisk: ai.RiskHigh,
	}
	srv := newAITestServer(t, &fakeEngine{affected: 1}, f)

	status, body := postJSON(t, srv.URL+"/preview",
		`{"sql":"UPDATE CollectionBox SET status = 1 WHERE id = 42","ai":false}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %v", status, body)
	}
	if v, ok := body["ai"]; !ok || v != nil {
		t.Fatalf("ai = %v, want null con ai:false", v)
	}
}

// TestPreviewAIEnrichesWhenEnabled: con IA habilitada y default on, el preview
// devuelve explanation + risk_level + flags.
func TestPreviewAIEnrichesWhenEnabled(t *testing.T) {
	f := &fakeAI{
		enabled:     true,
		explainText: "Vas a marcar 1 registro como listo.",
		explainRisk: ai.RiskLow,
		reviewFlags: []ai.Flag{{Severity: ai.SeverityInfo, Message: "operación acotada"}},
	}
	srv := newAITestServer(t, &fakeEngine{affected: 1}, f)

	status, body := postJSON(t, srv.URL+"/preview",
		`{"sql":"UPDATE CollectionBox SET status = 1 WHERE id = 42"}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %v", status, body)
	}
	aiBlock, ok := body["ai"].(map[string]any)
	if !ok {
		t.Fatalf("se esperaba un bloque ai, got %v", body["ai"])
	}
	if aiBlock["explanation"] == "" {
		t.Fatalf("se esperaba una explicación: %v", aiBlock)
	}
	if aiBlock["risk_level"] == "" {
		t.Fatalf("se esperaba un risk_level: %v", aiBlock)
	}
	flags, ok := aiBlock["flags"].([]any)
	if !ok || len(flags) != 1 {
		t.Fatalf("se esperaba 1 flag, got %v", aiBlock["flags"])
	}
}

// --- Heurística de riesgo (unidad) ---

func TestHeuristicRisk(t *testing.T) {
	tests := []struct {
		name     string
		op       guard.Operation
		affected int64
		max      int64
		want     ai.RiskLevel
	}{
		{"update chico", guard.OpUpdate, 1, 50, ai.RiskLow},
		{"update medio", guard.OpUpdate, 20, 50, ai.RiskMedium}, // 0.4 => medium
		{"update alto", guard.OpUpdate, 40, 50, ai.RiskHigh},    // 0.8 => high
		{"delete chico", guard.OpDelete, 1, 50, ai.RiskMedium},  // DELETE piso medium
		{"delete grande", guard.OpDelete, 30, 50, ai.RiskHigh},  // DELETE + 0.6 => high
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := heuristicRisk(tt.op, tt.affected, tt.max)
			if got != tt.want {
				t.Fatalf("heuristicRisk(%s, %d/%d) = %q, want %q", tt.op, tt.affected, tt.max, got, tt.want)
			}
		})
	}
}
