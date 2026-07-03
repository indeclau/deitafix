// Copyright 2026 indeclau. Licensed under the Apache License, Version 2.0.
// See the LICENSE file in the repository root for the full license text.

package ai

import (
	"fmt"
	"strings"
)

// Este archivo concentra los prompts. Se mantienen aparte del transporte para
// poder iterarlos sin tocar el cliente HTTP.
//
// Principio transversal: al modelo se le pide SIEMPRE devolver un único objeto
// JSON con una forma fija, y se le recuerda que su salida es solo una PROPUESTA
// que pasará por las guardas del servidor. El modelo no es una barrera de
// seguridad; el parser real y el usuario restringido de la base sí lo son.

// suggestSystemPrompt arma el system prompt de NL → SQL para el motor dado.
func suggestSystemPrompt(engine string) string {
	dialect := dialectName(engine)
	return fmt.Sprintf(`Sos un asistente que traduce una intención en lenguaje natural a UNA sentencia SQL de escritura para %s.

Reglas estrictas:
- Devolvé EXACTAMENTE una sola sentencia: UPDATE, DELETE o INSERT (con VALUES explícitos). Nunca SELECT, DDL, DROP, TRUNCATE ni múltiples sentencias.
- UPDATE y DELETE DEBEN incluir una cláusula WHERE acotada; nunca generes un UPDATE o DELETE sin WHERE.
- Usá solo tablas y columnas que aparezcan en el esquema provisto. Si el esquema no alcanza, hacé tu mejor esfuerzo con los nombres que da la intención.
- Tu salida es una PROPUESTA. El servidor la re-valida con un parser real, una whitelist de tablas y un tope de filas; no intentes evadir esas guardas.

Respondé ÚNICAMENTE con un objeto JSON, sin texto alrededor, con esta forma:
{"sql": "<la sentencia>", "rationale": "<por qué esta sentencia, en una o dos frases>"}`, dialect)
}

// suggestUserPrompt arma el mensaje de usuario con la intención y el esquema.
func suggestUserPrompt(req SuggestRequest) string {
	var sb strings.Builder
	sb.WriteString("Intención:\n")
	sb.WriteString(strings.TrimSpace(req.Intent))
	sb.WriteString("\n\n")

	if len(req.Schema) > 0 {
		sb.WriteString("Esquema disponible (solo estas tablas y columnas):\n")
		sb.WriteString(formatSchema(req.Schema))
	} else {
		sb.WriteString("No hay esquema disponible; usá los nombres de tabla/columna que aparezcan en la intención.")
	}
	return sb.String()
}

// explainSystemPrompt arma el system prompt de explicación de impacto.
func explainSystemPrompt() string {
	return `Sos un asistente que le explica a una persona, en lenguaje claro y breve, el impacto de una escritura sobre una base de datos de producción, antes de que confirme.

- Traducí "N filas afectadas" a algo comprensible y accionable.
- Estimá el riesgo: "low", "medium" o "high", combinando el tipo de operación (un DELETE pesa más que un UPDATE) y la proporción de filas afectadas respecto del tope.
- No inventes datos que no estén en la sentencia; describí lo que la sentencia haría.

Respondé ÚNICAMENTE con un objeto JSON, sin texto alrededor, con esta forma:
{"explanation": "<explicación en lenguaje claro>", "risk_level": "low|medium|high"}`
}

// explainUserPrompt arma el mensaje de usuario para la explicación.
func explainUserPrompt(req ExplainRequest) string {
	return fmt.Sprintf(`Motor: %s
Operación: %s sobre la tabla %q
Filas afectadas (medidas en un preview con ROLLBACK): %d
Tope de filas permitido: %d
Sentencia:
%s`, dialectName(req.Engine), req.Op, req.Table, req.AffectedRows, req.MaxRows, strings.TrimSpace(req.SQL))
}

// reviewSystemPrompt arma el system prompt del revisor IA.
func reviewSystemPrompt() string {
	return `Sos un segundo par de ojos que revisa una sentencia SQL de escritura antes de que una persona la confirme sobre producción. Marcás patrones sospechosos; NO bloqueás nada (el gate real son las guardas del servidor).

Marcá cosas como:
- un WHERE que parece demasiado amplio para la intención,
- un DELETE que afecta muchas filas,
- modificación de columnas sensibles (por ejemplo password, saldo, rol, estado de pago),
- valores que no matchean el tipo esperado de la columna.

Si no ves nada digno de mención, devolvé una lista vacía. No inventes problemas.

Respondé ÚNICAMENTE con un objeto JSON, sin texto alrededor, con esta forma:
{"flags": [{"severity": "info|warning|danger", "message": "<qué notaste>"}]}`
}

// reviewUserPrompt arma el mensaje de usuario para el revisor.
func reviewUserPrompt(req ReviewRequest) string {
	return fmt.Sprintf(`Motor: %s
Operación: %s sobre la tabla %q
Filas afectadas (medidas en un preview con ROLLBACK): %d
Tope de filas permitido: %d
Sentencia:
%s`, dialectName(req.Engine), req.Op, req.Table, req.AffectedRows, req.MaxRows, strings.TrimSpace(req.SQL))
}

// formatSchema serializa el esquema compacto para el prompt: una tabla por
// línea con sus columnas y tipos.
func formatSchema(tables []TableSchema) string {
	var sb strings.Builder
	for _, t := range tables {
		sb.WriteString("- ")
		sb.WriteString(t.Name)
		sb.WriteString(" (")
		for i, c := range t.Columns {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString(c.Name)
			if c.Type != "" {
				sb.WriteString(" ")
				sb.WriteString(c.Type)
			}
		}
		sb.WriteString(")\n")
	}
	return sb.String()
}

// dialectName da un nombre legible del motor para el prompt.
func dialectName(engine string) string {
	switch strings.ToLower(strings.TrimSpace(engine)) {
	case "mysql":
		return "MySQL/MariaDB"
	case "postgres":
		return "PostgreSQL"
	default:
		return "SQL"
	}
}
