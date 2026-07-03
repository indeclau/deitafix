// Copyright 2026 indeclau. Licensed under the Apache License, Version 2.0.
// See the LICENSE file in the repository root for the full license text.

package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Defaults del cliente de IA. Todos overrideables por entorno (ver config).
const (
	// DefaultModel es el modelo por defecto. Se documenta acá y se puede
	// overridear con AI_MODEL. Se verificó como el ID vigente al momento de
	// escribir esto; si Anthropic lo renombra, se cambia AI_MODEL sin tocar
	// código.
	DefaultModel = "claude-opus-4-8"

	// DefaultBaseURL es el endpoint de la Messages API de Anthropic. Overrideable
	// con AI_BASE_URL (por ejemplo para un proxy o un gateway compatible).
	DefaultBaseURL = "https://api.anthropic.com"

	// DefaultTimeout es el timeout por request de IA, independiente del timeout
	// de la base. Overrideable con AI_TIMEOUT.
	DefaultTimeout = 15 * time.Second

	// anthropicVersion es la versión de la API que exige el header. Fija para el
	// contrato de la Messages API.
	anthropicVersion = "2023-06-01"

	// maxTokens acota la respuesta del modelo. Suficiente para un SQL candidato,
	// una explicación breve o un puñado de flags; evita respuestas gigantes.
	maxTokens = 1024
)

// Config son los parámetros del cliente real de Anthropic. Los provee la capa
// de configuración a partir del entorno.
type Config struct {
	// APIKey es la credencial (AI_API_KEY). Si está vacía, no se debe construir
	// este cliente: se usa NewDisabled en su lugar.
	APIKey string
	// Model es el modelo a usar (AI_MODEL). Si está vacío, DefaultModel.
	Model string
	// BaseURL es el endpoint (AI_BASE_URL). Si está vacío, DefaultBaseURL.
	BaseURL string
	// Timeout es el timeout por request (AI_TIMEOUT). Si es cero, DefaultTimeout.
	Timeout time.Duration
	// HTTPClient permite inyectar un cliente en tests (httptest). Si es nil, se
	// usa uno con el Timeout configurado.
	HTTPClient *http.Client
}

// anthropicClient es la implementación real de Client contra la Messages API.
type anthropicClient struct {
	apiKey  string
	model   string
	baseURL string
	timeout time.Duration
	http    *http.Client
}

// NewAnthropic construye el cliente real. El caller solo debe usarlo cuando hay
// APIKey; sin ella corresponde NewDisabled (ver config).
func NewAnthropic(cfg Config) Client {
	model := cfg.Model
	if strings.TrimSpace(model) == "" {
		model = DefaultModel
	}
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: timeout}
	}
	return &anthropicClient{
		apiKey:  cfg.APIKey,
		model:   model,
		baseURL: baseURL,
		timeout: timeout,
		http:    httpClient,
	}
}

func (c *anthropicClient) Enabled() bool { return true }

// --- Tipos del wire de la Messages API (solo lo que usamos) ---

type messagesRequest struct {
	Model     string        `json:"model"`
	MaxTokens int           `json:"max_tokens"`
	System    string        `json:"system,omitempty"`
	Messages  []messageWire `json:"messages"`
}

type messageWire struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type messagesResponse struct {
	Content []contentBlock `json:"content"`
	// StopReason ayuda a distinguir un refusal de una respuesta normal.
	StopReason string `json:"stop_reason"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// --- Payloads estructurados que le pedimos al modelo ---
//
// El system prompt le pide devolver EXACTAMENTE un objeto JSON con estas formas.
// El parseo es defensivo: si no matchea, se degrada con un mensaje neutro.

type suggestPayload struct {
	SQL       string `json:"sql"`
	Rationale string `json:"rationale"`
}

type explainPayload struct {
	Explanation string `json:"explanation"`
	RiskLevel   string `json:"risk_level"`
}

type reviewPayload struct {
	Flags []flagPayload `json:"flags"`
}

type flagPayload struct {
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

// SuggestSQL pide al modelo una sentencia candidata. No toca la base. El SQL
// resultante es input NO confiable: el caller debe pasarlo por /preview.
func (c *anthropicClient) SuggestSQL(ctx context.Context, req SuggestRequest) (SuggestResult, error) {
	system := suggestSystemPrompt(req.Engine)
	user := suggestUserPrompt(req)

	raw, err := c.complete(ctx, system, user)
	if err != nil {
		return SuggestResult{}, err
	}

	var p suggestPayload
	if err := decodeModelJSON(raw, &p); err != nil {
		return SuggestResult{}, fmt.Errorf("ai: respuesta del modelo no interpretable: %w", err)
	}
	if strings.TrimSpace(p.SQL) == "" {
		return SuggestResult{}, fmt.Errorf("ai: el modelo no propuso ninguna sentencia")
	}
	return SuggestResult{
		SQL:       strings.TrimSpace(p.SQL),
		Rationale: strings.TrimSpace(p.Rationale),
	}, nil
}

// ExplainImpact traduce el impacto a lenguaje claro. Best-effort: si el modelo
// devuelve algo inválido, se degrada a un mensaje neutro con riesgo medium en
// vez de fallar (el caller además aísla esto del preview).
func (c *anthropicClient) ExplainImpact(ctx context.Context, req ExplainRequest) (Explanation, error) {
	system := explainSystemPrompt()
	user := explainUserPrompt(req)

	raw, err := c.complete(ctx, system, user)
	if err != nil {
		return Explanation{}, err
	}

	var p explainPayload
	if err := decodeModelJSON(raw, &p); err != nil {
		// Degradación neutra: el impacto ya lo tiene el caller; devolvemos un
		// texto genérico en lugar de romper.
		return Explanation{
			Text:      "No se pudo generar una explicación legible del impacto.",
			RiskLevel: RiskMedium,
		}, nil
	}
	return Explanation{
		Text:      strings.TrimSpace(p.Explanation),
		RiskLevel: normalizeRisk(p.RiskLevel),
	}, nil
}

// ReviewStatement marca patrones sospechosos. Best-effort: si no parsea,
// devuelve una review vacía (sin flags) en lugar de fallar.
func (c *anthropicClient) ReviewStatement(ctx context.Context, req ReviewRequest) (Review, error) {
	system := reviewSystemPrompt()
	user := reviewUserPrompt(req)

	raw, err := c.complete(ctx, system, user)
	if err != nil {
		return Review{}, err
	}

	var p reviewPayload
	if err := decodeModelJSON(raw, &p); err != nil {
		// Sin flags interpretables: mejor no marcar nada que inventar.
		return Review{}, nil
	}
	out := make([]Flag, 0, len(p.Flags))
	for _, f := range p.Flags {
		msg := strings.TrimSpace(f.Message)
		if msg == "" {
			continue
		}
		out = append(out, Flag{
			Severity: normalizeSeverity(f.Severity),
			Message:  msg,
		})
	}
	return Review{Flags: out}, nil
}

// complete hace un POST a /v1/messages con su propio timeout y devuelve el texto
// concatenado de los bloques de tipo "text" de la respuesta.
func (c *anthropicClient) complete(ctx context.Context, system, user string) (string, error) {
	// Timeout propio de la IA, independiente del de la base. Si el context ya
	// trae un deadline más corto, gana el más corto.
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	body, err := json.Marshal(messagesRequest{
		Model:     c.model,
		MaxTokens: maxTokens,
		System:    system,
		Messages:  []messageWire{{Role: "user", Content: user}},
	})
	if err != nil {
		return "", fmt.Errorf("ai: serializando request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("ai: construyendo request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Api-Key", c.apiKey)
	httpReq.Header.Set("Anthropic-Version", anthropicVersion)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("ai: llamando al proveedor: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Acotamos la lectura para no dejar que una respuesta enorme del proveedor
	// consuma memoria sin límite.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("ai: leyendo respuesta: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ai: proveedor devolvió %d", resp.StatusCode)
	}

	var mr messagesResponse
	if err := json.Unmarshal(raw, &mr); err != nil {
		return "", fmt.Errorf("ai: respuesta del proveedor ilegible: %w", err)
	}

	var sb strings.Builder
	for _, b := range mr.Content {
		if b.Type == "text" {
			sb.WriteString(b.Text)
		}
	}
	text := strings.TrimSpace(sb.String())
	if text == "" {
		return "", fmt.Errorf("ai: el proveedor no devolvió texto (stop_reason=%q)", mr.StopReason)
	}
	return text, nil
}

// decodeModelJSON parsea de forma defensiva un objeto JSON emitido por el
// modelo. El modelo a veces envuelve el JSON en prosa o en un bloque ```json:
// extraemos el primer objeto {...} balanceado antes de deserializar.
func decodeModelJSON(text string, dst any) error {
	obj, ok := extractJSONObject(text)
	if !ok {
		return fmt.Errorf("ai: no se encontró un objeto JSON en la respuesta")
	}
	return json.Unmarshal([]byte(obj), dst)
}

// extractJSONObject devuelve el primer objeto JSON balanceado ({...}) dentro de
// s, respetando strings y escapes. Si no hay ninguno, ok=false. Es la defensa
// contra que el modelo agregue texto alrededor del JSON pedido.
func extractJSONObject(s string) (string, bool) {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return "", false
	}
	depth := 0
	inStr := false
	escaped := false
	for i := start; i < len(s); i++ {
		ch := s[i]
		if inStr {
			switch {
			case escaped:
				escaped = false
			case ch == '\\':
				escaped = true
			case ch == '"':
				inStr = false
			}
			continue
		}
		switch ch {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1], true
			}
		}
	}
	return "", false
}

// normalizeRisk mapea el string del modelo a un RiskLevel conocido; ante algo
// inesperado, medium (default conservador).
func normalizeRisk(s string) RiskLevel {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "low":
		return RiskLow
	case "high":
		return RiskHigh
	default:
		return RiskMedium
	}
}

// normalizeSeverity mapea el string del modelo a una Severity conocida; ante
// algo inesperado, warning (default conservador).
func normalizeSeverity(s string) Severity {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "info":
		return SeverityInfo
	case "danger":
		return SeverityDanger
	default:
		return SeverityWarning
	}
}
