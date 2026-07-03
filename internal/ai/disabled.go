// Copyright 2026 indeclau. Licensed under the Apache License, Version 2.0.
// See the LICENSE file in the repository root for the full license text.

package ai

import "context"

// disabledClient es la implementación de Client cuando no hay AI_API_KEY. Todos
// los métodos devuelven ErrAIDisabled y Enabled() devuelve false. Es lo que
// hace posible la degradación limpia: el resto del servicio funciona idéntico y
// la capa de IA simplemente no se ofrece.
//
// No loguea ni reintenta: devolver el centinela y dejar que el handler decida es
// suficiente. Así no hay ruido en loop cuando la IA está apagada a propósito.
type disabledClient struct{}

// NewDisabled construye el cliente noop. Es el default cuando falta AI_API_KEY.
func NewDisabled() Client { return disabledClient{} }

func (disabledClient) Enabled() bool { return false }

func (disabledClient) SuggestSQL(context.Context, SuggestRequest) (SuggestResult, error) {
	return SuggestResult{}, ErrAIDisabled
}

func (disabledClient) ExplainImpact(context.Context, ExplainRequest) (Explanation, error) {
	return Explanation{}, ErrAIDisabled
}

func (disabledClient) ReviewStatement(context.Context, ReviewRequest) (Review, error) {
	return Review{}, ErrAIDisabled
}
