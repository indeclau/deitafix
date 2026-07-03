// Package ai es la capa de IA opcional de Deitafix: NL → SQL, explicación de
// impacto y revisor de sentencias. Es estrictamente "la IA solo propone": todo
// lo que produce este paquete vuelve a pasar por las mismas guardas y el mismo
// flujo preview → confirm que el SQL crudo de un humano. La IA nunca ejecuta ni
// tiene un camino a confirm.
//
// El paquete se diseña alrededor de una interfaz (Client) para poder:
//
//   - degradar de forma limpia cuando no hay AI_API_KEY (Disabled, un noop que
//     devuelve ErrAIDisabled y que los handlers traducen a una respuesta clara);
//   - mockear en los tests sin tocar ningún proveedor real (un fake que satisface
//     la interfaz, o un httptest.Server que imita la Messages API).
//
// La implementación real (anthropicClient) habla la Messages API de Anthropic
// por HTTP crudo, con su propio timeout, y parsea la salida del modelo de forma
// defensiva: si el modelo devuelve algo que no matchea el JSON esperado, se
// degrada con un mensaje neutro en vez de crashear. La salida del modelo se
// trata como input NO confiable (posible prompt injection): la seguridad la
// garantizan el parser + el usuario restringido de la base, jamás el modelo.
package ai

import (
	"context"
	"errors"
)

// ErrAIDisabled es el error centinela que devuelven todos los métodos del
// cliente Disabled (sin AI_API_KEY). Los handlers HTTP lo detectan con
// errors.Is y lo traducen a una respuesta limpia ("IA no configurada"), en vez
// de a un 500 opaco.
var ErrAIDisabled = errors.New("ai: capa de IA no configurada (falta AI_API_KEY)")

// RiskLevel es la señal de riesgo cualitativa de una operación, combinando la
// heurística determinística (proporción de filas, DELETE vs UPDATE) con la señal
// del modelo.
type RiskLevel string

const (
	RiskLow    RiskLevel = "low"
	RiskMedium RiskLevel = "medium"
	RiskHigh   RiskLevel = "high"
)

// Severity es la severidad de un flag del revisor.
type Severity string

const (
	SeverityInfo    Severity = "info"
	SeverityWarning Severity = "warning"
	SeverityDanger  Severity = "danger"
)

// TableSchema es el esquema compacto de una tabla de la whitelist, que se le
// pasa al modelo como contexto para que genere columnas reales en NL → SQL. Se
// introspecciona de information_schema, acotado a lo que el usuario restringido
// puede ver.
type TableSchema struct {
	// Name es el nombre de la tabla, con su casing original.
	Name string
	// Columns son las columnas de la tabla, en un formato compacto "col tipo".
	Columns []Column
}

// Column es una columna dentro de un TableSchema.
type Column struct {
	Name string
	Type string
}

// SuggestRequest es la entrada de SuggestSQL: la intención en lenguaje natural
// más el motor y, opcionalmente, el esquema de las tablas de la whitelist.
type SuggestRequest struct {
	// Engine es el motor de la base ("postgres" | "mysql"), para que el modelo
	// use la sintaxis correcta.
	Engine string
	// Intent es la intención en lenguaje natural del usuario.
	Intent string
	// Schema es el contexto de esquema opcional (tablas de la whitelist). Puede
	// estar vacío: el modelo funciona con la intención y los nombres de tabla.
	Schema []TableSchema
}

// SuggestResult es la salida de SuggestSQL: el SQL candidato más el porqué.
//
// El SQL es un CANDIDATO: todavía NO pasó por las guardas ni por el preview. El
// cliente debe mandarlo a POST /preview, donde se valida igual que cualquier SQL
// humano.
type SuggestResult struct {
	// SQL es la sentencia candidata propuesta por el modelo.
	SQL string
	// Rationale explica en lenguaje claro por qué el modelo eligió esa sentencia.
	Rationale string
}

// ExplainRequest es la entrada de ExplainImpact: la sentencia validada y su
// impacto medido en el preview, para traducirlo a lenguaje claro.
type ExplainRequest struct {
	Engine       string
	SQL          string
	Op           string
	Table        string
	AffectedRows int64
	// MaxRows es el tope de filas configurado, para que el modelo dimensione el
	// impacto relativo.
	MaxRows int64
}

// Explanation es la salida de ExplainImpact: una traducción del impacto a
// lenguaje claro más una señal de riesgo.
type Explanation struct {
	Text      string
	RiskLevel RiskLevel
}

// ReviewRequest es la entrada de ReviewStatement: la sentencia y su impacto,
// para que el revisor marque patrones sospechosos.
type ReviewRequest struct {
	Engine       string
	SQL          string
	Op           string
	Table        string
	AffectedRows int64
	MaxRows      int64
}

// Flag es un patrón sospechoso marcado por el revisor IA. Es un par de ojos
// PARCIAL: informa, no bloquea. El gate real siguen siendo las guardas.
type Flag struct {
	Severity Severity
	Message  string
}

// Review es la salida de ReviewStatement: la lista de flags.
type Review struct {
	Flags []Flag
}

// Client es la abstracción de la capa de IA. La satisfacen el cliente real
// (Anthropic) y el cliente Disabled (noop), además de cualquier fake en tests.
//
// Ningún método toca la base ni ejecuta nada: solo proponen texto/SQL que luego
// pasa por las guardas. Cada método respeta su propio timeout vía el context.
type Client interface {
	// Enabled indica si la capa de IA está configurada y operativa. Cuando es
	// false, los handlers ocultan/deshabilitan lo de IA con gracia.
	Enabled() bool

	// SuggestSQL propone una sentencia candidata a partir de una intención en
	// lenguaje natural. NO toca la base. Devuelve ErrAIDisabled si la IA no está
	// configurada.
	SuggestSQL(ctx context.Context, req SuggestRequest) (SuggestResult, error)

	// ExplainImpact traduce el impacto medido en el preview a lenguaje claro más
	// una señal de riesgo. Es best-effort: el caller debe tolerar un error o
	// timeout sin romper el preview.
	ExplainImpact(ctx context.Context, req ExplainRequest) (Explanation, error)

	// ReviewStatement marca patrones sospechosos en la sentencia (segundo par de
	// ojos parcial). Best-effort, igual que ExplainImpact.
	ReviewStatement(ctx context.Context, req ReviewRequest) (Review, error)
}
