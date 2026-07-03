package engine

import (
	"fmt"

	"github.com/pingcap/tidb/pkg/parser"
	"github.com/pingcap/tidb/pkg/parser/ast"
	// test_driver registra los constructores de value-expr (NewValueExpr, ...).
	// Sin este blank import, cualquier SQL con un literal (WHERE id = 5) rompe
	// el parseo. Es el driver self-contained recomendado para uso standalone.
	_ "github.com/pingcap/tidb/pkg/parser/test_driver"

	"github.com/indeclau/deitafix/internal/guard"
)

// parseMySQL clasifica una sentencia usando el parser real de MySQL/MariaDB
// (el parser de TiDB, Go puro). Devuelve un guard.Statement neutral.
func parseMySQL(sql string) (guard.Statement, error) {
	p := parser.New()

	stmts, _, err := p.Parse(sql, "", "")
	if err != nil {
		return guard.Statement{}, fmt.Errorf("%w: %v", guard.ErrParse, err)
	}

	switch len(stmts) {
	case 0:
		return guard.Statement{}, guard.ErrEmptySQL
	case 1:
		// ok
	default:
		return guard.Statement{}, guard.ErrMultipleStatements
	}

	switch st := stmts[0].(type) {
	case *ast.UpdateStmt:
		return guard.Statement{
			Op:       guard.OpUpdate,
			Table:    firstTableName(st.TableRefs),
			HasWhere: st.Where != nil,
		}, nil

	case *ast.DeleteStmt:
		return guard.Statement{
			Op:       guard.OpDelete,
			Table:    firstTableName(st.TableRefs),
			HasWhere: st.Where != nil,
		}, nil

	case *ast.InsertStmt:
		// En InsertStmt el campo de tabla es Table (no TableRefs).
		// Select != nil ⇒ INSERT ... SELECT; Lists no vacío ⇒ VALUES.
		return guard.Statement{
			Op:               guard.OpInsert,
			Table:            firstTableName(st.Table),
			InsertFromSelect: st.Select != nil || len(st.Lists) == 0,
		}, nil

	default:
		// SELECT, DDL, DROP, TRUNCATE, etc.: no soportado.
		return guard.Statement{Op: unsupportedOp}, nil
	}
}

// firstTableName navega el join tree y devuelve el nombre (original) de la
// primera tabla. Devuelve "" si la fuente no es una tabla simple (subconsulta,
// etc.), lo que hará que el checker rechace la sentencia por whitelist.
func firstTableName(clause *ast.TableRefsClause) string {
	if clause == nil || clause.TableRefs == nil {
		return ""
	}
	if tn := resolveTableName(clause.TableRefs); tn != nil {
		return tn.Name.O
	}
	return ""
}

// resolveTableName desciende recursivamente por el árbol de joins hasta el
// primer *ast.TableName. La tabla de más a la izquierda es la objetivo en un
// UPDATE/DELETE/INSERT simple de una sola tabla.
func resolveTableName(rs ast.ResultSetNode) *ast.TableName {
	switch n := rs.(type) {
	case *ast.Join:
		if tn := resolveTableName(n.Left); tn != nil {
			return tn
		}
		return resolveTableName(n.Right)
	case *ast.TableSource:
		return resolveTableName(n.Source)
	case *ast.TableName:
		return n
	default:
		return nil
	}
}
