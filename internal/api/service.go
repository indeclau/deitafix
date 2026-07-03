// Copyright 2026 indeclau. Licensed under the Apache License, Version 2.0.
// See the LICENSE file in the repository root for the full license text.

// Package api implementa la capa HTTP del flujo preview → confirm.
//
// El transporte (router chi, JSON) se separa de la lógica de aplicación
// (Service) para poder testear la orquestación sin levantar un servidor.
package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/indeclau/deitafix/internal/ai"
	"github.com/indeclau/deitafix/internal/engine"
	"github.com/indeclau/deitafix/internal/guard"
	"github.com/indeclau/deitafix/internal/store"
)

// Service orquesta el flujo preview → confirm sobre un Engine, aplicando las
// guardas y gestionando los tokens de un solo uso.
type Service struct {
	engine    engine.Engine
	store     *store.Store
	whitelist []string
	maxRows   int64

	// ai es la capa de IA (opcional). Nunca es nil: cuando no hay AI_API_KEY es
	// un cliente disabled (noop) cuyo Enabled() devuelve false. Solo propone;
	// jamás ejecuta ni llega a confirm.
	ai ai.Client

	// schema es el cache de introspección de esquema para NL → SQL. Puede ser
	// nil (motor sin introspección o IA apagada): en ese caso el modelo trabaja
	// solo con la intención.
	schema *engine.SchemaCache
}

// NewService construye el Service con la capa de IA deshabilitada (noop). Es el
// camino histórico usado por los tests que no ejercitan IA.
func NewService(eng engine.Engine, st *store.Store, whitelist []string, maxRows int64) *Service {
	return NewServiceWithAI(eng, st, whitelist, maxRows, ai.NewDisabled(), nil)
}

// NewServiceWithAI construye el Service con una capa de IA explícita y un cache
// de esquema opcional. El entrypoint lo usa para inyectar el cliente real o el
// disabled según haya o no AI_API_KEY.
func NewServiceWithAI(eng engine.Engine, st *store.Store, whitelist []string, maxRows int64, aiClient ai.Client, schema *engine.SchemaCache) *Service {
	if aiClient == nil {
		aiClient = ai.NewDisabled()
	}
	return &Service{
		engine:    eng,
		store:     st,
		whitelist: whitelist,
		maxRows:   maxRows,
		ai:        aiClient,
		schema:    schema,
	}
}

// PreviewResult es lo que devuelve un preview exitoso.
type PreviewResult struct {
	Token        string
	AffectedRows int64
	Summary      string

	// AI es el enriquecimiento de IA (explicación + riesgo + flags). Es nil
	// cuando la IA está deshabilitada, se pidió omitir (ai:false) o falló: en
	// todos esos casos el resto del PreviewResult queda intacto.
	AI *AIInsight
}

// ConfirmResult es lo que devuelve un confirm exitoso.
type ConfirmResult struct {
	AffectedRows int64
	Summary      string
}

// Preview valida y mide el impacto de una operación desde la superficie humana
// (origin=ui), sin persistirla. Ver preview para el detalle del flujo.
//
// El SQL efectivo puede venir de dos modos, mutuamente excluyentes:
//   - rawSQL: SQL crudo provisto por el cliente (pasa por las guardas completas).
//   - op: una operación acotada que el servicio traduce a SQL parametrizado.
//
// aiEnabled pide (además) el enriquecimiento de IA best-effort. Aun cuando es
// true, la IA es opcional y aislada: si está apagada o falla, el resto del
// PreviewResult queda intacto. Pasar false lo omite del todo (para no pagar
// latencia/costo cuando no se quiere).
//
// Devuelve un token de un solo uso para el confirm posterior.
func (s *Service) Preview(ctx context.Context, rawSQL string, op *engine.BoundedOp, aiEnabled bool) (PreviewResult, error) {
	return s.preview(ctx, rawSQL, op, store.OriginUI, aiEnabled)
}

// PreviewMCP es el preview del agente (origin=mcp): mismas guardas, mismo
// preview con ROLLBACK, mismo store. La única diferencia es el origen del token,
// que hace que NUNCA se pueda ejecutar con la credencial del agente: queda a la
// espera de aprobación humana. Es el mismo camino seguro que la superficie
// humana, sin ningún atajo. No enriquece con IA (el agente no consume ese
// bloque; la aprobación la mira un humano por la superficie humana).
func (s *Service) PreviewMCP(ctx context.Context, rawSQL string, op *engine.BoundedOp) (PreviewResult, error) {
	return s.preview(ctx, rawSQL, op, store.OriginMCP, false)
}

// preview es el núcleo compartido del preview para ambas superficies (humana y
// MCP). El origen es lo único que varía en la ruta segura: las guardas, la
// medición de impacto en transacción con ROLLBACK y el tope de filas son
// idénticos, para que un agente no pueda saltear ninguna salvaguarda. El
// enriquecimiento de IA es un extra best-effort al final, que NO afecta esa
// ruta.
func (s *Service) preview(ctx context.Context, rawSQL string, op *engine.BoundedOp, origin store.Origin, aiEnabled bool) (PreviewResult, error) {
	sql, args, err := s.resolveSQL(rawSQL, op)
	if err != nil {
		return PreviewResult{}, err
	}

	// 1. Parseo con el parser real del motor.
	stmt, err := s.engine.Parse(sql)
	if err != nil {
		return PreviewResult{}, err
	}

	// 2. Guardas de sentencia (operación, WHERE, INSERT..SELECT, whitelist).
	//    ESTA es la barrera: se aplica idéntica al SQL humano y al generado por
	//    IA (que llega acá como rawSQL, ya sin distinción de origen).
	if err := guard.Check(stmt, s.whitelist); err != nil {
		return PreviewResult{}, err
	}

	// 3. Ejecución en transacción con ROLLBACK: mide el impacto sin persistir.
	affected, err := s.engine.Preview(ctx, sql, args...)
	if err != nil {
		return PreviewResult{}, err
	}

	// 4. Tope de filas afectadas.
	if err := guard.CheckAffectedRows(affected, s.maxRows); err != nil {
		return PreviewResult{}, err
	}

	// 5. Guardar la sentencia validada y emitir el token, marcado con su origen.
	token, err := s.store.PutWithOrigin(store.Entry{
		SQL:          sql,
		Args:         args,
		Op:           string(stmt.Op),
		Table:        stmt.Table,
		AffectedRows: affected,
	}, origin)
	if err != nil {
		return PreviewResult{}, fmt.Errorf("api: guardando token: %w", err)
	}

	res := PreviewResult{
		Token:        token,
		AffectedRows: affected,
		Summary:      summarize(stmt.Op, stmt.Table, affected),
	}

	// 6. Enriquecimiento de IA best-effort y AISLADO. Ya tenemos token +
	//    affected_rows: pase lo que pase con la IA, el preview es válido. Un
	//    fallo o timeout de IA devuelve AI=nil, nunca rompe la respuesta.
	if aiEnabled {
		res.AI = s.enrichPreview(ctx, sql, stmt, affected)
	}

	return res, nil
}

// Ready comprueba que el servicio puede alcanzar la base con el usuario
// restringido. Lo usa la probe de readiness (/readyz); no ejecuta ninguna
// operación de negocio.
func (s *Service) Ready(ctx context.Context) error {
	return s.engine.Ping(ctx)
}

// Engine devuelve el nombre del motor real del servidor ("postgres" | "mysql").
// Lo consume la UI para mostrarlo como indicador read-only: el motor es una
// propiedad del servidor (a qué base apunta DATABASE_URL), no algo que el
// cliente elija por request.
func (s *Service) Engine() string {
	return s.engine.Name()
}

// Confirm ejecuta y persiste la operación asociada a un token de un solo uso,
// por la vía del confirm humano directo (superficie humana / flujo UI). El token
// se invalida al recuperarlo, aunque la ejecución falle.
//
// Gating: un token origin=mcp NO se ejecuta por acá. La credencial del agente no
// puede confirmar; los previews del agente deben pasar por la aprobación humana
// (Approve). Confirm devuelve ErrMCPRequiresApproval para esos tokens, que la
// capa HTTP mapea a 409. La garantía de human-in-the-loop no se confía al
// cliente: se fuerza acá, del lado del servidor.
func (s *Service) Confirm(ctx context.Context, token string) (ConfirmResult, error) {
	// Primero miramos el origen SIN consumir: si el token es del agente, esta vía
	// no puede tocarlo. Consumirlo antes de chequear permitiría destruir (griefear)
	// una propuesta pendiente enviándola a /confirm, dejándola sin aprobar.
	peeked, err := s.store.Peek(token)
	if err != nil {
		return ConfirmResult{}, err
	}
	if peeked.Origin == store.OriginMCP {
		return ConfirmResult{}, ErrMCPRequiresApproval
	}

	// Recién ahora consumimos el token (un solo uso) y ejecutamos.
	entry, err := s.store.Take(token)
	if err != nil {
		return ConfirmResult{}, err
	}
	return s.execute(ctx, entry)
}

// RequestApproval marca un token origin=mcp como pendiente de aprobación humana
// SIN ejecutar nada contra la base. Es lo que hace la herramienta MCP confirm:
// el agente propone y solicita, pero jamás ejecuta.
//
// Devuelve ErrConfirmNotMCP si el token no es de origen mcp (un token humano no
// se propone para aprobación por esta vía), y propaga store.ErrNotFound /
// store.ErrWrongState (token inexistente, expirado o en estado inesperado).
func (s *Service) RequestApproval(_ context.Context, token string) error {
	entry, err := s.store.Peek(token)
	if err != nil {
		return err
	}
	if entry.Origin != store.OriginMCP {
		return ErrConfirmNotMCP
	}
	if _, err := s.store.RequestApproval(token); err != nil {
		return err
	}
	return nil
}

// PendingItem es un token a la espera de aprobación humana, tal como lo ve la
// superficie de aprobación (lista y detalle).
type PendingItem struct {
	Token        string
	SQL          string
	Op           string
	Table        string
	AffectedRows int64
	ExpiresIn    time.Duration
}

// ListPending devuelve los previews de agente (origin=mcp) a la espera de
// aprobación humana. Es la fuente de la pantalla "Aprobaciones pendientes".
func (s *Service) ListPending(_ context.Context) []PendingItem {
	pend := s.store.ListPending()
	out := make([]PendingItem, 0, len(pend))
	for _, p := range pend {
		out = append(out, PendingItem(p))
	}
	return out
}

// Approve es la ejecución que SOLO un humano puede disparar: toma un token
// pendiente de aprobación, lo consume y ejecuta la operación con COMMIT. Es el
// equivalente humano del confirm para los previews del agente.
//
// El token debe estar en pending_approval; en cualquier otro caso propaga
// store.ErrNotFound / store.ErrWrongState.
func (s *Service) Approve(ctx context.Context, token string) (ConfirmResult, error) {
	entry, err := s.store.Approve(token)
	if err != nil {
		return ConfirmResult{}, err
	}
	return s.execute(ctx, entry)
}

// Reject descarta un token pendiente de aprobación sin ejecutar nada. Consume el
// token. Mismos errores que Approve.
func (s *Service) Reject(_ context.Context, token string) error {
	if _, err := s.store.Reject(token); err != nil {
		return err
	}
	return nil
}

// execute corre la sentencia validada dentro de una transacción con COMMIT y
// arma el resultado. Es el único lugar que toca la base para ejecutar de verdad;
// lo comparten el confirm humano directo (Confirm) y la aprobación humana
// (Approve).
func (s *Service) execute(ctx context.Context, entry store.Entry) (ConfirmResult, error) {
	affected, err := s.engine.Confirm(ctx, entry.SQL, entry.Args...)
	if err != nil {
		return ConfirmResult{}, err
	}
	return ConfirmResult{
		AffectedRows: affected,
		Summary:      fmt.Sprintf("Confirmado: %d fila(s) afectada(s) en %q.", affected, entry.Table),
	}, nil
}

// resolveSQL determina el SQL efectivo según el modo de entrada. Exactamente
// uno de rawSQL / op debe estar presente.
func (s *Service) resolveSQL(rawSQL string, op *engine.BoundedOp) (string, []any, error) {
	hasRaw := rawSQL != ""
	hasOp := op != nil

	switch {
	case hasRaw && hasOp:
		return "", nil, ErrAmbiguousInput
	case hasRaw:
		return rawSQL, nil, nil
	case hasOp:
		return s.engine.BuildSQL(*op)
	default:
		return "", nil, ErrEmptyInput
	}
}

// Errores de entrada de la capa de aplicación.
var (
	// ErrAmbiguousInput indica que se enviaron a la vez SQL crudo y operación.
	ErrAmbiguousInput = errors.New("api: enviar SQL crudo u operación acotada, no ambos")
	// ErrEmptyInput indica que no se envió ni SQL crudo ni operación.
	ErrEmptyInput = errors.New("api: falta SQL crudo u operación acotada")

	// ErrMCPRequiresApproval indica que se intentó ejecutar por el confirm humano
	// directo un token creado por el agente (origin=mcp). Esos tokens solo se
	// ejecutan vía aprobación humana explícita. La capa HTTP lo mapea a 409.
	ErrMCPRequiresApproval = errors.New("api: un token de origen mcp requiere aprobación humana, no se ejecuta por confirm")

	// ErrConfirmNotMCP indica que se pidió aprobación (herramienta MCP confirm)
	// sobre un token que no es de origen mcp.
	ErrConfirmNotMCP = errors.New("api: confirm solo aplica a tokens de origen mcp")
)

// summarize arma un resumen legible del impacto medido.
func summarize(op guard.Operation, table string, affected int64) string {
	return fmt.Sprintf("%s sobre %q afectaría %d fila(s). Revisá antes de confirmar.",
		op, table, affected)
}
