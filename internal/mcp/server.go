// Copyright 2026 indeclau. Licensed under the Apache License, Version 2.0.
// See the LICENSE file in the repository root for the full license text.

// Package mcp expone el core seguro de Deitafix (preview → confirm) como un
// servidor MCP, para que un agente de IA pueda PROPONER escrituras sobre una
// base de producción obligado a pasar por las mismas guardas y el mismo preview
// que la superficie humana.
//
// La garantía central —el human-in-the-loop forzado a nivel servidor— no vive
// acá sino en el core: la herramienta preview crea tokens marcados como
// origin=mcp, que NUNCA se pueden ejecutar con la credencial del agente. La
// herramienta confirm no ejecuta: solo SOLICITA aprobación humana y devuelve la
// URL donde una persona debe apretar el botón. Este paquete es un transporte
// fino sobre el core; no reimplementa ninguna salvaguarda ni abre ningún atajo.
//
// El paquete no importa la capa HTTP (api): depende solo de la interfaz Core,
// que el llamador satisface con un adaptador. Así el montaje en el router chi
// (api → mcp) no genera un ciclo de importación.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// serverName y serverVersion identifican al servidor MCP ante los clientes.
const (
	serverName    = "deitafix"
	serverVersion = "v0.4.0"
)

// PreviewResult es el resultado neutral de un preview, tal como lo necesita la
// capa MCP. Es un tipo local (no api.PreviewResult) para no importar la capa
// HTTP y evitar el ciclo de importación.
type PreviewResult struct {
	Token        string
	AffectedRows int64
	Summary      string
}

// BoundedOp describe la operación acotada (segundo modo de entrada) de forma
// neutral. El adaptador la traduce al tipo del engine.
type BoundedOp struct {
	Op    string
	Table string
	Set   map[string]any
	Where map[string]any
}

// Core es el subconjunto del servicio que la capa MCP necesita. Lo satisface un
// adaptador sobre *api.Service. Solo expone preview (con origen mcp) y la
// solicitud de aprobación: la ejecución real NO está en esta interfaz, porque la
// credencial del agente jamás debe poder ejecutar.
type Core interface {
	// Engine devuelve el motor real del servidor ("postgres" | "mysql").
	Engine() string

	// PreviewMCP corre el preview del agente (mismas guardas, ROLLBACK, store)
	// y emite un token origin=mcp. Exactamente uno de rawSQL / op no vacío.
	PreviewMCP(ctx context.Context, rawSQL string, op *BoundedOp) (PreviewResult, error)

	// RequestApproval marca un token origin=mcp como pendiente de aprobación
	// humana, sin tocar la base. Es lo que dispara la herramienta confirm.
	RequestApproval(ctx context.Context, token string) error
}

// Config son los parámetros de presentación del servidor MCP.
type Config struct {
	// ApprovalBaseURL es la base pública desde donde un humano aprueba (por
	// ejemplo "https://host:8080"). La herramienta confirm devuelve
	// ApprovalBaseURL + ApprovalPath como approval_url. Puede quedar vacía: en
	// ese caso approval_url es una ruta relativa.
	ApprovalBaseURL string
}

// approvalPath es la ruta de la UI de aprobaciones pendientes (superficie
// humana) a la que apunta la herramienta confirm. Es la página HTML donde una
// persona revisa el impacto y aprueba/rechaza; distinta del endpoint JSON
// GET /pending que la consume por debajo.
const approvalPath = "/approvals"

// --- Entrada / salida de las herramientas (inferidas a JSON Schema por el SDK) ---

// previewInput refleja el body de POST /preview: SQL crudo u operación acotada.
type previewInput struct {
	Engine    string        `json:"engine,omitempty" jsonschema:"motor de la base (postgres|mysql); informativo, el servidor usa su propio motor"`
	SQL       string        `json:"sql,omitempty" jsonschema:"SQL crudo a previsualizar (UPDATE/DELETE/INSERT); usar esto O operation, no ambos"`
	Operation *operationArg `json:"operation,omitempty" jsonschema:"operación acotada (modo estructurado); usar esto O sql, no ambos"`
}

// operationArg es la operación acotada dentro del input de preview.
type operationArg struct {
	Op    string         `json:"op" jsonschema:"operación: UPDATE o DELETE"`
	Table string         `json:"table" jsonschema:"tabla objetivo (debe estar en la whitelist)"`
	Set   map[string]any `json:"set,omitempty" jsonschema:"columnas a asignar (solo UPDATE)"`
	Where map[string]any `json:"where" jsonschema:"condiciones del WHERE, unidas con AND (obligatorio)"`
}

// previewOutput es el resultado de la herramienta preview.
type previewOutput struct {
	Token        string `json:"token" jsonschema:"token de un solo uso para solicitar la confirmación"`
	AffectedRows int64  `json:"affected_rows" jsonschema:"filas que la operación afectaría (medidas con ROLLBACK)"`
	Summary      string `json:"summary" jsonschema:"resumen legible del impacto"`
}

// confirmInput es el input de la herramienta confirm: solo el token.
type confirmInput struct {
	Token string `json:"token" jsonschema:"token devuelto por preview"`
}

// confirmOutput es el resultado de confirm: NO es una ejecución, es una
// solicitud de aprobación humana.
type confirmOutput struct {
	Status      string `json:"status" jsonschema:"siempre 'pending_approval': el agente no ejecuta"`
	ApprovalURL string `json:"approval_url" jsonschema:"URL donde un humano debe aprobar y ejecutar"`
	Message     string `json:"message" jsonschema:"mensaje claro de que la ejecución la decide una persona"`
}

// NewServer construye el servidor MCP con las herramientas preview y confirm
// registradas sobre el core.
func NewServer(core Core, cfg Config) *mcpsdk.Server {
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{
		Name:    serverName,
		Version: serverVersion,
	}, nil)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name: "preview",
		Description: "Previsualiza una escritura (UPDATE/DELETE/INSERT) contra la base: " +
			"la parsea con el parser real del motor, aplica las guardas (rechaza UPDATE/DELETE " +
			"sin WHERE, tope de filas, whitelist de tabla) y mide el impacto dentro de una " +
			"transacción con ROLLBACK. No persiste nada. Devuelve un token para solicitar la " +
			"confirmación humana. Un input inválido devuelve un error de herramienta.",
	}, previewHandler(core))

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name: "confirm",
		Description: "Solicita la confirmación humana de un preview. NO ejecuta ni hace COMMIT: " +
			"marca el token como pendiente de aprobación y devuelve la URL donde una persona debe " +
			"aprobar y ejecutar. La ejecución es siempre decisión de un humano; el agente no puede " +
			"ejecutar por diseño.",
	}, confirmHandler(core, cfg))

	return srv
}

// NewStreamableHandler devuelve un http.Handler (transporte Streamable HTTP)
// que sirve el servidor MCP dado. Se monta en el router chi bajo MCP_PATH. Usa
// el mismo server para todas las requests (un solo binario, un solo core).
func NewStreamableHandler(srv *mcpsdk.Server) http.Handler {
	return mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server {
		return srv
	}, nil)
}

// previewHandler adapta la herramienta preview al core. Traduce el input a los
// dos modos de entrada y propaga cualquier rechazo de las guardas como error de
// herramienta (el SDK lo empaqueta con IsError=true).
func previewHandler(core Core) mcpsdk.ToolHandlerFor[previewInput, previewOutput] {
	return func(ctx context.Context, _ *mcpsdk.CallToolRequest, in previewInput) (*mcpsdk.CallToolResult, previewOutput, error) {
		var op *BoundedOp
		if in.Operation != nil {
			parsed, err := toBoundedOp(in.Operation)
			if err != nil {
				return nil, previewOutput{}, err
			}
			op = &parsed
		}

		res, err := core.PreviewMCP(ctx, in.SQL, op)
		if err != nil {
			// Guardas, parseo, tope de filas, permisos del motor: todo se
			// propaga como error de herramienta, nunca como un pending.
			return nil, previewOutput{}, err
		}

		// previewOutput y PreviewResult tienen los mismos campos: conversión directa.
		return nil, previewOutput(res), nil
	}
}

// confirmHandler adapta la herramienta confirm. Marca el token como pendiente de
// aprobación y devuelve la URL humana; jamás ejecuta.
func confirmHandler(core Core, cfg Config) mcpsdk.ToolHandlerFor[confirmInput, confirmOutput] {
	return func(ctx context.Context, _ *mcpsdk.CallToolRequest, in confirmInput) (*mcpsdk.CallToolResult, confirmOutput, error) {
		if in.Token == "" {
			return nil, confirmOutput{}, errors.New("mcp: falta el token")
		}

		if err := core.RequestApproval(ctx, in.Token); err != nil {
			return nil, confirmOutput{}, err
		}

		return nil, confirmOutput{
			Status:      "pending_approval",
			ApprovalURL: cfg.ApprovalBaseURL + approvalPath,
			Message: "El preview quedó a la espera de aprobación humana. El agente no puede ejecutar: " +
				"una persona debe aprobar y ejecutar desde la superficie humana (" + approvalPath + ").",
		}, nil
	}
}

// toBoundedOp valida y traduce la operación acotada del input MCP al tipo
// neutral BoundedOp. Solo UPDATE y DELETE son válidos en este modo (igual que la
// API), replicando la validación de la capa HTTP.
func toBoundedOp(arg *operationArg) (BoundedOp, error) {
	switch arg.Op {
	case "UPDATE", "DELETE":
		// ok
	default:
		return BoundedOp{}, fmt.Errorf("mcp: operación acotada inválida %q (esperado UPDATE|DELETE)", arg.Op)
	}
	return BoundedOp{
		Op:    arg.Op,
		Table: arg.Table,
		Set:   arg.Set,
		Where: arg.Where,
	}, nil
}
