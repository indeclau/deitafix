// Copyright 2026 indeclau. Licensed under the Apache License, Version 2.0.
// See the LICENSE file in the repository root for the full license text.

package guard

import (
	"errors"
	"testing"
)

// whitelist de ejemplo usada por los casos. "CollectionBox" está permitida;
// "AuditSensitive" deliberadamente no, para replicar el seed.
var testWhitelist = []string{"CollectionBox", "orders"}

func TestCheck(t *testing.T) {
	tests := []struct {
		name    string
		stmt    Statement
		wantErr error
	}{
		{
			name:    "UPDATE con WHERE en tabla permitida ok",
			stmt:    Statement{Op: OpUpdate, Table: "CollectionBox", HasWhere: true},
			wantErr: nil,
		},
		{
			name:    "UPDATE sin WHERE se rechaza",
			stmt:    Statement{Op: OpUpdate, Table: "CollectionBox", HasWhere: false},
			wantErr: ErrMissingWhere,
		},
		{
			name:    "DELETE con WHERE en tabla permitida ok",
			stmt:    Statement{Op: OpDelete, Table: "CollectionBox", HasWhere: true},
			wantErr: nil,
		},
		{
			name:    "DELETE sin WHERE se rechaza",
			stmt:    Statement{Op: OpDelete, Table: "CollectionBox", HasWhere: false},
			wantErr: ErrMissingWhere,
		},
		{
			name:    "INSERT con VALUES en tabla permitida ok",
			stmt:    Statement{Op: OpInsert, Table: "CollectionBox", InsertFromSelect: false},
			wantErr: nil,
		},
		{
			name:    "INSERT ... SELECT se rechaza",
			stmt:    Statement{Op: OpInsert, Table: "CollectionBox", InsertFromSelect: true},
			wantErr: ErrInsertFromSelect,
		},
		{
			name:    "tabla fuera de whitelist se rechaza",
			stmt:    Statement{Op: OpUpdate, Table: "AuditSensitive", HasWhere: true},
			wantErr: ErrTableNotWhitelisted,
		},
		{
			name:    "tabla con casing distinto se rechaza (comparación exacta)",
			stmt:    Statement{Op: OpUpdate, Table: "collectionbox", HasWhere: true},
			wantErr: ErrTableNotWhitelisted,
		},
		{
			name:    "operación no soportada se rechaza",
			stmt:    Statement{Op: Operation("SELECT"), Table: "CollectionBox", HasWhere: true},
			wantErr: ErrOperationNotAllowed,
		},
		{
			name:    "WHERE ausente tiene prioridad sobre whitelist",
			stmt:    Statement{Op: OpDelete, Table: "AuditSensitive", HasWhere: false},
			wantErr: ErrMissingWhere,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Check(tt.stmt, testWhitelist)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Check() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestCheckEmptyWhitelistRejectsEverything(t *testing.T) {
	// Con whitelist vacía, ninguna tabla debe pasar (default seguro).
	err := Check(Statement{Op: OpUpdate, Table: "CollectionBox", HasWhere: true}, nil)
	if !errors.Is(err, ErrTableNotWhitelisted) {
		t.Fatalf("con whitelist vacía se esperaba ErrTableNotWhitelisted, got %v", err)
	}
}

func TestCheckAffectedRows(t *testing.T) {
	tests := []struct {
		name     string
		affected int64
		maxRows  int64
		wantErr  error
	}{
		{name: "por debajo del tope ok", affected: 5, maxRows: 50, wantErr: nil},
		{name: "exactamente en el tope ok", affected: 50, maxRows: 50, wantErr: nil},
		{name: "una fila por encima aborta", affected: 51, maxRows: 50, wantErr: ErrRowsExceeded},
		{name: "cero filas ok", affected: 0, maxRows: 50, wantErr: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckAffectedRows(tt.affected, tt.maxRows)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("CheckAffectedRows(%d, %d) error = %v, want %v",
					tt.affected, tt.maxRows, err, tt.wantErr)
			}
		})
	}
}
