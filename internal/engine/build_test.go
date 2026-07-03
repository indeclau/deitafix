package engine

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/indeclau/deitafix/internal/guard"
)

func TestBuildBoundedSQL(t *testing.T) {
	tests := []struct {
		name     string
		op       BoundedOp
		quote    func(string) string
		ph       placeholderStyle
		wantSQL  string
		wantArgs []any
		wantErr  error
	}{
		{
			name: "UPDATE postgres con set y where",
			op: BoundedOp{
				Op:    guard.OpUpdate,
				Table: "CollectionBox",
				Set:   map[string]any{"status": 1},
				Where: map[string]any{"id": 42},
			},
			quote:    quotePostgres,
			ph:       placeholderDollar,
			wantSQL:  `UPDATE "CollectionBox" SET "status" = $1 WHERE "id" = $2`,
			wantArgs: []any{1, 42},
		},
		{
			name: "UPDATE mysql con set y where",
			op: BoundedOp{
				Op:    guard.OpUpdate,
				Table: "CollectionBox",
				Set:   map[string]any{"status": 1},
				Where: map[string]any{"id": 42},
			},
			quote:    quoteMySQL,
			ph:       placeholderQuestion,
			wantSQL:  "UPDATE `CollectionBox` SET `status` = ? WHERE `id` = ?",
			wantArgs: []any{1, 42},
		},
		{
			name: "DELETE postgres con where",
			op: BoundedOp{
				Op:    guard.OpDelete,
				Table: "CollectionBox",
				Where: map[string]any{"id": 7},
			},
			quote:    quotePostgres,
			ph:       placeholderDollar,
			wantSQL:  `DELETE FROM "CollectionBox" WHERE "id" = $1`,
			wantArgs: []any{7},
		},
		{
			name: "columnas ordenadas alfabéticamente (determinismo)",
			op: BoundedOp{
				Op:    guard.OpUpdate,
				Table: "t",
				Set:   map[string]any{"b": 2, "a": 1},
				Where: map[string]any{"z": 9, "y": 8},
			},
			quote:    quotePostgres,
			ph:       placeholderDollar,
			wantSQL:  `UPDATE "t" SET "a" = $1, "b" = $2 WHERE "y" = $3 AND "z" = $4`,
			wantArgs: []any{1, 2, 8, 9},
		},
		{
			name: "identificador con comilla se escapa (postgres)",
			op: BoundedOp{
				Op:    guard.OpDelete,
				Table: `we"ird`,
				Where: map[string]any{"id": 1},
			},
			quote:    quotePostgres,
			ph:       placeholderDollar,
			wantSQL:  `DELETE FROM "we""ird" WHERE "id" = $1`,
			wantArgs: []any{1},
		},
		{
			name: "DELETE sin where se rechaza",
			op: BoundedOp{
				Op:    guard.OpDelete,
				Table: "t",
			},
			quote:   quotePostgres,
			ph:      placeholderDollar,
			wantErr: guard.ErrMissingWhere,
		},
		{
			name: "UPDATE sin set se rechaza",
			op: BoundedOp{
				Op:    guard.OpUpdate,
				Table: "t",
				Where: map[string]any{"id": 1},
			},
			quote:   quotePostgres,
			ph:      placeholderDollar,
			wantErr: errSentinelSet,
		},
		{
			name: "operación no soportada en modo acotado",
			op: BoundedOp{
				Op:    guard.OpInsert,
				Table: "t",
				Where: map[string]any{"id": 1},
			},
			quote:   quotePostgres,
			ph:      placeholderDollar,
			wantErr: guard.ErrOperationNotAllowed,
		},
		{
			name:    "tabla vacía se rechaza",
			op:      BoundedOp{Op: guard.OpDelete, Where: map[string]any{"id": 1}},
			quote:   quotePostgres,
			ph:      placeholderDollar,
			wantErr: errSentinelTable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotSQL, gotArgs, err := buildBoundedSQL(tt.op, tt.quote, tt.ph)

			if tt.wantErr != nil {
				if err == nil {
					t.Fatalf("esperaba error %v, got nil (sql=%q)", tt.wantErr, gotSQL)
				}
				// Los errores de tabla/set no son centinela; se chequean por marcador.
				switch tt.wantErr {
				case errSentinelTable:
					if !strings.Contains(err.Error(), "sin tabla") {
						t.Fatalf("esperaba error de tabla, got %v", err)
					}
				case errSentinelSet:
					if !strings.Contains(err.Error(), "sin campos SET") {
						t.Fatalf("esperaba error de SET vacío, got %v", err)
					}
				default:
					if !errors.Is(err, tt.wantErr) {
						t.Fatalf("error = %v, want %v", err, tt.wantErr)
					}
				}
				return
			}

			if err != nil {
				t.Fatalf("error inesperado: %v", err)
			}
			if gotSQL != tt.wantSQL {
				t.Fatalf("SQL:\n got  %q\n want %q", gotSQL, tt.wantSQL)
			}
			if !reflect.DeepEqual(gotArgs, tt.wantArgs) {
				t.Fatalf("args:\n got  %v\n want %v", gotArgs, tt.wantArgs)
			}
		})
	}
}

// Marcadores locales para distinguir errores no-centinela en la tabla.
var (
	errSentinelTable = errors.New("marker: table")
	errSentinelSet   = errors.New("marker: set")
)
