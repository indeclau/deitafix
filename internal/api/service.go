// Package api implementa la capa HTTP del flujo preview → confirm.
//
// El transporte (router chi, JSON) se separa de la lógica de aplicación
// (Service) para poder testear la orquestación sin levantar un servidor.
package api

import (
	"context"
	"errors"
	"fmt"

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
}

// NewService construye el Service con sus dependencias.
func NewService(eng engine.Engine, st *store.Store, whitelist []string, maxRows int64) *Service {
	return &Service{
		engine:    eng,
		store:     st,
		whitelist: whitelist,
		maxRows:   maxRows,
	}
}

// PreviewResult es lo que devuelve un preview exitoso.
type PreviewResult struct {
	Token        string
	AffectedRows int64
	Summary      string
}

// ConfirmResult es lo que devuelve un confirm exitoso.
type ConfirmResult struct {
	AffectedRows int64
	Summary      string
}

// Preview valida y mide el impacto de una operación, sin persistirla.
//
// El SQL efectivo puede venir de dos modos, mutuamente excluyentes:
//   - rawSQL: SQL crudo provisto por el cliente (pasa por las guardas completas).
//   - op: una operación acotada que el servicio traduce a SQL parametrizado.
//
// Devuelve un token de un solo uso para el confirm posterior.
func (s *Service) Preview(ctx context.Context, rawSQL string, op *engine.BoundedOp) (PreviewResult, error) {
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

	// 5. Guardar la sentencia validada y emitir el token.
	token, err := s.store.Put(store.Entry{
		SQL:          sql,
		Args:         args,
		Table:        stmt.Table,
		AffectedRows: affected,
	})
	if err != nil {
		return PreviewResult{}, fmt.Errorf("api: guardando token: %w", err)
	}

	return PreviewResult{
		Token:        token,
		AffectedRows: affected,
		Summary:      summarize(stmt.Op, stmt.Table, affected),
	}, nil
}

// Ready comprueba que el servicio puede alcanzar la base con el usuario
// restringido. Lo usa la probe de readiness (/readyz); no ejecuta ninguna
// operación de negocio.
func (s *Service) Ready(ctx context.Context) error {
	return s.engine.Ping(ctx)
}

// Confirm ejecuta y persiste la operación asociada a un token de un solo uso.
// El token se invalida al recuperarlo, aunque la ejecución falle.
func (s *Service) Confirm(ctx context.Context, token string) (ConfirmResult, error) {
	entry, err := s.store.Take(token)
	if err != nil {
		return ConfirmResult{}, err
	}

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
)

// summarize arma un resumen legible del impacto medido.
func summarize(op guard.Operation, table string, affected int64) string {
	return fmt.Sprintf("%s sobre %q afectaría %d fila(s). Revisá antes de confirmar.",
		op, table, affected)
}
