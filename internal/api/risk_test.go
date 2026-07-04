// Copyright 2026 indeclau. Licensed under the Apache License, Version 2.0.
// See the LICENSE file in the repository root for the full license text.

package api

import (
	"testing"

	"github.com/indeclau/deitafix/internal/ai"
	"github.com/indeclau/deitafix/internal/guard"
)

// TestHeuristicRiskEdgeCases cubre ramas de heuristicRisk que el test principal
// (TestHeuristicRisk en ai_test.go) no ejercita: maxRows=0 (sin división) e
// INSERT. El resto de los casos ya están cubiertos allí.
func TestHeuristicRiskEdgeCases(t *testing.T) {
	cases := []struct {
		name     string
		op       guard.Operation
		affected int64
		maxRows  int64
		want     ai.RiskLevel
	}{
		{"INSERT pocas filas -> low", guard.OpInsert, 1, 100, ai.RiskLow},
		{"maxRows 0 no divide por cero (UPDATE -> low)", guard.OpUpdate, 5, 0, ai.RiskLow},
		{"maxRows 0 con DELETE -> medium", guard.OpDelete, 5, 0, ai.RiskMedium},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := heuristicRisk(tc.op, tc.affected, tc.maxRows); got != tc.want {
				t.Errorf("heuristicRisk(%q, %d, %d) = %q, want %q", tc.op, tc.affected, tc.maxRows, got, tc.want)
			}
		})
	}
}

// TestCombineRisk verifica que la señal más conservadora (mayor) siempre gana,
// en ambos órdenes de los argumentos.
func TestCombineRisk(t *testing.T) {
	cases := []struct {
		a, b, want ai.RiskLevel
	}{
		{ai.RiskLow, ai.RiskLow, ai.RiskLow},
		{ai.RiskLow, ai.RiskHigh, ai.RiskHigh},
		{ai.RiskHigh, ai.RiskLow, ai.RiskHigh},
		{ai.RiskMedium, ai.RiskHigh, ai.RiskHigh},
		{ai.RiskHigh, ai.RiskMedium, ai.RiskHigh},
		{ai.RiskMedium, ai.RiskLow, ai.RiskMedium},
	}
	for _, tc := range cases {
		if got := combineRisk(tc.a, tc.b); got != tc.want {
			t.Errorf("combineRisk(%q, %q) = %q, want %q", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestRiskRank fija el orden total de los niveles de riesgo (low < medium < high),
// incluido un valor desconocido que cae al piso.
func TestRiskRank(t *testing.T) {
	if riskRank(ai.RiskHigh) <= riskRank(ai.RiskMedium) {
		t.Fatal("high debe rankear por encima de medium")
	}
	if riskRank(ai.RiskMedium) <= riskRank(ai.RiskLow) {
		t.Fatal("medium debe rankear por encima de low")
	}
	// Un RiskLevel no reconocido cae al piso (0), como low.
	if riskRank(ai.RiskLevel("desconocido")) != riskRank(ai.RiskLow) {
		t.Fatal("un riesgo desconocido debía rankear como el piso (low)")
	}
}
