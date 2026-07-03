// Copyright 2026 indeclau. Licensed under the Apache License, Version 2.0.
// See the LICENSE file in the repository root for the full license text.

package ai

import (
	"context"
	"errors"
	"testing"
)

// TestDisabledClient verifica la degradación limpia: sin AI_API_KEY, Enabled()
// es false y todos los métodos devuelven el centinela ErrAIDisabled (que los
// handlers traducen a 503). No crashea ni hace nada raro.
func TestDisabledClient(t *testing.T) {
	c := NewDisabled()

	if c.Enabled() {
		t.Fatal("el cliente disabled debe reportar Enabled()==false")
	}

	if _, err := c.SuggestSQL(context.Background(), SuggestRequest{}); !errors.Is(err, ErrAIDisabled) {
		t.Fatalf("SuggestSQL err = %v, want ErrAIDisabled", err)
	}
	if _, err := c.ExplainImpact(context.Background(), ExplainRequest{}); !errors.Is(err, ErrAIDisabled) {
		t.Fatalf("ExplainImpact err = %v, want ErrAIDisabled", err)
	}
	if _, err := c.ReviewStatement(context.Background(), ReviewRequest{}); !errors.Is(err, ErrAIDisabled) {
		t.Fatalf("ReviewStatement err = %v, want ErrAIDisabled", err)
	}
}

// TestNormalizers cubre el mapeo defensivo de valores del modelo a los enums
// conocidos, con default conservador ante lo inesperado.
func TestNormalizers(t *testing.T) {
	if got := normalizeRisk("HIGH"); got != RiskHigh {
		t.Fatalf("normalizeRisk(HIGH) = %q", got)
	}
	if got := normalizeRisk("cualquiera"); got != RiskMedium {
		t.Fatalf("normalizeRisk(desconocido) = %q, want medium", got)
	}
	if got := normalizeSeverity("info"); got != SeverityInfo {
		t.Fatalf("normalizeSeverity(info) = %q", got)
	}
	if got := normalizeSeverity("raro"); got != SeverityWarning {
		t.Fatalf("normalizeSeverity(desconocido) = %q, want warning", got)
	}
}
