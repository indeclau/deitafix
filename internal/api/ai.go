package api

import (
	"context"
	"time"

	"github.com/indeclau/deitafix/internal/ai"
	"github.com/indeclau/deitafix/internal/guard"
)

// Este archivo contiene la lógica de aplicación de la capa de IA, separada del
// núcleo preview → confirm (service.go). Invariantes que se respetan acá:
//
//  1. La IA SOLO propone. SuggestSQL genera un candidato y NO toca la base
//     (salvo la introspección de esquema, que es de solo lectura). El candidato
//     vuelve por /preview, donde pasa por las mismas guardas.
//  2. La IA no toca la ruta segura. El enriquecimiento del preview es
//     best-effort y está aislado: un fallo o timeout de IA nunca rompe el
//     preview (que ya tiene token + affected_rows antes de llamar a la IA).
//  3. La IA no llega a confirm. Nada de este archivo participa del confirm.

// AISuggestion es el resultado de SuggestSQL para la capa HTTP.
type AISuggestion struct {
	SQL       string
	Rationale string
	Engine    string
}

// SuggestSQL propone una sentencia candidata a partir de una intención en
// lenguaje natural. NO ejecuta nada de escritura: a lo sumo introspecciona el
// esquema (solo lectura) para darle contexto al modelo. Devuelve ai.ErrAIDisabled
// si la capa de IA no está configurada, que la capa HTTP mapea a 503.
//
// El SQL devuelto es un CANDIDATO sin validar: el cliente debe mandarlo a
// /preview, donde pasa por las guardas igual que cualquier SQL humano.
func (s *Service) SuggestSQL(ctx context.Context, intent string) (AISuggestion, error) {
	if !s.ai.Enabled() {
		return AISuggestion{}, ai.ErrAIDisabled
	}

	req := ai.SuggestRequest{
		Engine: s.engine.Name(),
		Intent: intent,
		Schema: s.schemaContext(ctx),
	}

	res, err := s.ai.SuggestSQL(ctx, req)
	if err != nil {
		return AISuggestion{}, err
	}
	return AISuggestion{
		SQL:       res.SQL,
		Rationale: res.Rationale,
		Engine:    s.engine.Name(),
	}, nil
}

// schemaContext introspecciona el esquema de las tablas de la whitelist para
// pasárselo al modelo. Es best-effort: si el motor no soporta introspección, o
// si la consulta falla, se devuelve nil y el modelo trabaja solo con la
// intención. Nunca propaga el error: la sugerencia debe poder generarse igual.
func (s *Service) schemaContext(ctx context.Context) []ai.TableSchema {
	if s.schema == nil || len(s.whitelist) == 0 {
		return nil
	}

	// Acota la introspección para no colgar la sugerencia si la base tarda.
	ctx, cancel := context.WithTimeout(ctx, schemaTimeout)
	defer cancel()

	cols, err := s.schema.Columns(ctx, s.whitelist)
	if err != nil {
		return nil
	}

	out := make([]ai.TableSchema, 0, len(s.whitelist))
	// Recorremos la whitelist (no el mapa) para un orden estable.
	for _, table := range s.whitelist {
		cs := cols[table]
		if len(cs) == 0 {
			continue
		}
		tc := ai.TableSchema{Name: table, Columns: make([]ai.Column, 0, len(cs))}
		for _, c := range cs {
			tc.Columns = append(tc.Columns, ai.Column{Name: c.Name, Type: c.Type})
		}
		out = append(out, tc)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// AIInsight es el bloque de IA que enriquece un preview: explicación en lenguaje
// claro, señal de riesgo y flags del revisor. Es el "ai" de la respuesta de
// /preview. Un puntero nil (o Enabled=false) significa "sin IA".
type AIInsight struct {
	Explanation string
	RiskLevel   ai.RiskLevel
	Flags       []ai.Flag
}

// enrichPreview corre el enriquecimiento de IA sobre una sentencia YA validada y
// medida. Es best-effort y está AISLADO: se le da un context con su propio
// timeout y cualquier fallo devuelve un insight parcial (con el riesgo
// heurístico) o nil, nunca un error que suba al preview.
//
// La señal de riesgo combina una heurística determinística (proporción de filas
// y DELETE vs UPDATE) con la del modelo: aunque el modelo falle, risk_level
// sigue siendo significativo.
func (s *Service) enrichPreview(ctx context.Context, sql string, stmt guard.Statement, affected int64) *AIInsight {
	if !s.ai.Enabled() {
		return nil
	}

	// Baseline determinístico: se calcula SIEMPRE, aun si la IA falla.
	insight := &AIInsight{
		RiskLevel: heuristicRisk(stmt.Op, affected, s.maxRows),
	}

	// Timeout propio del enriquecimiento, aislado del preview y de la base.
	ctx, cancel := context.WithTimeout(ctx, aiPreviewTimeout)
	defer cancel()

	// Explicación de impacto (best-effort).
	exp, err := s.ai.ExplainImpact(ctx, ai.ExplainRequest{
		Engine:       s.engine.Name(),
		SQL:          sql,
		Op:           string(stmt.Op),
		Table:        stmt.Table,
		AffectedRows: affected,
		MaxRows:      s.maxRows,
	})
	if err == nil {
		insight.Explanation = exp.Text
		insight.RiskLevel = combineRisk(insight.RiskLevel, exp.RiskLevel)
	}

	// Revisor IA (best-effort).
	rev, err := s.ai.ReviewStatement(ctx, ai.ReviewRequest{
		Engine:       s.engine.Name(),
		SQL:          sql,
		Op:           string(stmt.Op),
		Table:        stmt.Table,
		AffectedRows: affected,
		MaxRows:      s.maxRows,
	})
	if err == nil {
		insight.Flags = rev.Flags
	}

	return insight
}

// heuristicRisk estima el riesgo sin la IA: un DELETE pesa más que un UPDATE, y
// cuanto más se acerca affected al tope, más alto el riesgo. Es el piso
// determinístico de la señal.
func heuristicRisk(op guard.Operation, affected, maxRows int64) ai.RiskLevel {
	var ratio float64
	if maxRows > 0 {
		ratio = float64(affected) / float64(maxRows)
	}

	switch {
	case op == guard.OpDelete && ratio >= 0.5:
		return ai.RiskHigh
	case ratio >= 0.75:
		return ai.RiskHigh
	case op == guard.OpDelete, ratio >= 0.25:
		return ai.RiskMedium
	default:
		return ai.RiskLow
	}
}

// combineRisk toma el mayor de dos señales de riesgo (heurística y modelo): la
// más conservadora gana, para no subestimar el impacto.
func combineRisk(a, b ai.RiskLevel) ai.RiskLevel {
	if riskRank(b) > riskRank(a) {
		return b
	}
	return a
}

func riskRank(r ai.RiskLevel) int {
	switch r {
	case ai.RiskHigh:
		return 2
	case ai.RiskMedium:
		return 1
	default:
		return 0
	}
}

// schemaTimeout acota la introspección de esquema para NL → SQL.
const schemaTimeout = 3 * time.Second

// aiPreviewTimeout acota el enriquecimiento de IA en el preview. El propio
// cliente de IA también aplica su AI_TIMEOUT; gana el más corto. Es var (no
// const) para poder acortarlo en tests y forzar la ruta de timeout sin esperar.
var aiPreviewTimeout = 15 * time.Second
