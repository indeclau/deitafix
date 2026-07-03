package engine

import (
	"fmt"

	pgquery "github.com/pganalyze/pg_query_go/v5"

	"github.com/indeclau/deitafix/internal/guard"
)

// parsePostgres clasifica una sentencia usando el parser real de PostgreSQL
// (libpg_query vía pg_query_go). Devuelve un guard.Statement neutral que el
// checker consume.
//
// Se usan los getters generados (GetRelation, GetWhereClause, ...) porque son
// nil-safe: sobre un puntero nil devuelven el cero del tipo sin panic.
func parsePostgres(sql string) (guard.Statement, error) {
	tree, err := pgquery.Parse(sql)
	if err != nil {
		return guard.Statement{}, fmt.Errorf("%w: %v", guard.ErrParse, err)
	}

	// Exactamente una sentencia: se rechaza multi-statement (posible inyección
	// por stacking) y la entrada vacía.
	switch len(tree.GetStmts()) {
	case 0:
		return guard.Statement{}, guard.ErrEmptySQL
	case 1:
		// ok
	default:
		return guard.Statement{}, guard.ErrMultipleStatements
	}

	node := tree.GetStmts()[0].GetStmt()
	if node == nil {
		return guard.Statement{}, guard.ErrEmptySQL
	}

	switch {
	case node.GetUpdateStmt() != nil:
		u := node.GetUpdateStmt()
		return guard.Statement{
			Op:       guard.OpUpdate,
			Table:    u.GetRelation().GetRelname(),
			HasWhere: u.GetWhereClause() != nil,
		}, nil

	case node.GetDeleteStmt() != nil:
		d := node.GetDeleteStmt()
		return guard.Statement{
			Op:       guard.OpDelete,
			Table:    d.GetRelation().GetRelname(),
			HasWhere: d.GetWhereClause() != nil,
		}, nil

	case node.GetInsertStmt() != nil:
		ins := node.GetInsertStmt()
		st := guard.Statement{
			Op:    guard.OpInsert,
			Table: ins.GetRelation().GetRelname(),
		}
		// libpg_query envuelve tanto VALUES como SELECT en un SelectStmt.
		// Si tiene ValuesLists no vacío es INSERT ... VALUES; en cualquier otro
		// caso (target/from list) es un INSERT ... SELECT.
		sel := ins.GetSelectStmt().GetSelectStmt()
		st.InsertFromSelect = sel == nil || len(sel.GetValuesLists()) == 0
		return st, nil

	default:
		// Cualquier otra cosa (SELECT, DDL, DROP, TRUNCATE, ...) no está
		// soportada. El checker la rechaza vía la operación desconocida.
		return guard.Statement{Op: unsupportedOp}, nil
	}
}

// unsupportedOp es una operación deliberadamente fuera del set permitido, para
// que el checker la rechace con ErrOperationNotAllowed sin tener que enumerar
// cada tipo de sentencia no soportada.
const unsupportedOp = guard.Operation("UNSUPPORTED")
