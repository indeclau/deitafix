package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeCore es un Core en memoria para testear la capa MCP sin base de datos ni
// la capa HTTP. Registra las llamadas y permite inyectar errores.
type fakeCore struct {
	engineName string

	previewErr     error
	previewResult  PreviewResult
	lastPreviewOp  *BoundedOp
	lastPreviewSQL string

	requestApprovalErr error
	approvedToken      string
}

func (c *fakeCore) Engine() string { return c.engineName }

func (c *fakeCore) PreviewMCP(_ context.Context, rawSQL string, op *BoundedOp) (PreviewResult, error) {
	c.lastPreviewSQL = rawSQL
	c.lastPreviewOp = op
	if c.previewErr != nil {
		return PreviewResult{}, c.previewErr
	}
	return c.previewResult, nil
}

func (c *fakeCore) RequestApproval(_ context.Context, token string) error {
	if c.requestApprovalErr != nil {
		return c.requestApprovalErr
	}
	c.approvedToken = token
	return nil
}

// connect arma el servidor MCP con el core dado y devuelve una sesión de cliente
// conectada por transporte in-memory (sin red).
func connect(t *testing.T, core Core, cfg Config) *mcpsdk.ClientSession {
	t.Helper()
	ctx := context.Background()

	srv := NewServer(core, cfg)
	t1, t2 := mcpsdk.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, t1, nil); err != nil {
		t.Fatalf("server.Connect: %v", err)
	}

	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test-client", Version: "v0"}, nil)
	cs, err := client.Connect(ctx, t2, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// callTool invoca una herramienta y devuelve el resultado crudo.
func callTool(t *testing.T, cs *mcpsdk.ClientSession, name string, args map[string]any) *mcpsdk.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s): %v", name, err)
	}
	return res
}

// decodeStructured decodifica el StructuredContent del resultado en dst.
func decodeStructured(t *testing.T, res *mcpsdk.CallToolResult, dst any) {
	t.Helper()
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured: %v", err)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		t.Fatalf("unmarshal structured: %v", err)
	}
}

func TestToolsAreListed(t *testing.T) {
	cs := connect(t, &fakeCore{engineName: "postgres"}, Config{})

	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	got := map[string]bool{}
	for _, tool := range res.Tools {
		got[tool.Name] = true
	}
	for _, want := range []string{"preview", "confirm"} {
		if !got[want] {
			t.Fatalf("falta la herramienta %q en la lista %v", want, got)
		}
	}
}

func TestPreviewToolRawSQL(t *testing.T) {
	core := &fakeCore{
		engineName:    "postgres",
		previewResult: PreviewResult{Token: "tok123", AffectedRows: 3, Summary: "UPDATE afectaría 3 filas"},
	}
	cs := connect(t, core, Config{})

	res := callTool(t, cs, "preview", map[string]any{
		"sql": "UPDATE CollectionBox SET status = 1 WHERE id = 42",
	})
	if res.IsError {
		t.Fatalf("preview devolvió error inesperado: %+v", res.Content)
	}

	var out previewOutput
	decodeStructured(t, res, &out)
	if out.Token != "tok123" || out.AffectedRows != 3 {
		t.Fatalf("output inesperado: %+v", out)
	}
	if core.lastPreviewSQL != "UPDATE CollectionBox SET status = 1 WHERE id = 42" {
		t.Fatalf("SQL no llegó al core: %q", core.lastPreviewSQL)
	}
	if core.lastPreviewOp != nil {
		t.Fatalf("no debía haber operación acotada, got %+v", core.lastPreviewOp)
	}
}

func TestPreviewToolBoundedOp(t *testing.T) {
	core := &fakeCore{
		engineName:    "mysql",
		previewResult: PreviewResult{Token: "t", AffectedRows: 1, Summary: "ok"},
	}
	cs := connect(t, core, Config{})

	res := callTool(t, cs, "preview", map[string]any{
		"operation": map[string]any{
			"op":    "UPDATE",
			"table": "CollectionBox",
			"set":   map[string]any{"status": 1},
			"where": map[string]any{"id": 42},
		},
	})
	if res.IsError {
		t.Fatalf("preview acotado devolvió error: %+v", res.Content)
	}
	if core.lastPreviewOp == nil {
		t.Fatal("la operación acotada no llegó al core")
	}
	if core.lastPreviewOp.Op != "UPDATE" || core.lastPreviewOp.Table != "CollectionBox" {
		t.Fatalf("operación acotada inesperada: %+v", core.lastPreviewOp)
	}
}

// TestPreviewToolInvalidOpIsToolError verifica que una operación acotada
// inválida (op fuera de UPDATE|DELETE) devuelve un error de herramienta, no un
// resultado.
func TestPreviewToolInvalidOpIsToolError(t *testing.T) {
	cs := connect(t, &fakeCore{engineName: "postgres"}, Config{})

	res := callTool(t, cs, "preview", map[string]any{
		"operation": map[string]any{
			"op":    "DROP",
			"table": "CollectionBox",
			"where": map[string]any{"id": 1},
		},
	})
	if !res.IsError {
		t.Fatal("operación inválida debía dar error de herramienta")
	}
}

// TestPreviewToolGuardErrorPropagates verifica que un rechazo de las guardas del
// core se propaga como error de herramienta (IsError=true), no como pending.
func TestPreviewToolGuardErrorPropagates(t *testing.T) {
	core := &fakeCore{
		engineName: "postgres",
		previewErr: errors.New("guard: UPDATE/DELETE requiere WHERE"),
	}
	cs := connect(t, core, Config{})

	res := callTool(t, cs, "preview", map[string]any{
		"sql": "UPDATE CollectionBox SET status = 1",
	})
	if !res.IsError {
		t.Fatal("el rechazo de las guardas debía dar error de herramienta")
	}
}

// TestConfirmToolRequestsApproval verifica el corazón del human-in-the-loop: la
// herramienta confirm NO ejecuta; devuelve pending_approval + la URL humana.
func TestConfirmToolRequestsApproval(t *testing.T) {
	core := &fakeCore{engineName: "postgres"}
	cs := connect(t, core, Config{ApprovalBaseURL: "https://deitafix.example.com"})

	res := callTool(t, cs, "confirm", map[string]any{"token": "tok123"})
	if res.IsError {
		t.Fatalf("confirm devolvió error inesperado: %+v", res.Content)
	}

	var out confirmOutput
	decodeStructured(t, res, &out)
	if out.Status != "pending_approval" {
		t.Fatalf("status = %q, want pending_approval", out.Status)
	}
	if out.ApprovalURL != "https://deitafix.example.com/approvals" {
		t.Fatalf("approval_url = %q", out.ApprovalURL)
	}
	if core.approvedToken != "tok123" {
		t.Fatalf("el token no se marcó para aprobación: %q", core.approvedToken)
	}
}

// TestConfirmToolPropagatesCoreError verifica que un token inválido/no-mcp
// devuelve error de herramienta.
func TestConfirmToolPropagatesCoreError(t *testing.T) {
	core := &fakeCore{
		engineName:         "postgres",
		requestApprovalErr: errors.New("store: token inexistente o expirado"),
	}
	cs := connect(t, core, Config{})

	res := callTool(t, cs, "confirm", map[string]any{"token": "nope"})
	if !res.IsError {
		t.Fatal("un token inválido debía dar error de herramienta")
	}
}

// TestConfirmToolEmptyToken verifica que confirm sin token es un error.
func TestConfirmToolEmptyToken(t *testing.T) {
	cs := connect(t, &fakeCore{engineName: "postgres"}, Config{})

	res := callTool(t, cs, "confirm", map[string]any{"token": ""})
	if !res.IsError {
		t.Fatal("confirm sin token debía dar error de herramienta")
	}
}
