# Capa MCP

Deitafix expone su core seguro (`preview → confirm`) como un **servidor MCP**
([Model Context Protocol](https://modelcontextprotocol.io)), para que un agente
de IA pueda **proponer** escrituras sobre una base de producción **obligado a
pasar por las mismas guardas y el mismo preview** que la superficie humana.

La ejecución sigue siendo **decisión de una persona** (human-in-the-loop), y eso
está **forzado a nivel servidor**: la credencial del agente físicamente no puede
ejecutar. No se confía en que el cliente MCP pida aprobación.

---

## Qué expone

El servidor MCP registra dos herramientas:

### `preview`

Funcionalidad completa para el agente. Refleja el body de `POST /preview`:

- `engine`: `"postgres"` | `"mysql"` (informativo; el motor real es el del
  servidor, según a qué base apunta `DATABASE_URL`).
- **Modo SQL crudo**: `sql` (string), **o**
- **Modo acotado**: `operation` con `op` (`UPDATE` | `DELETE`), `table`,
  `set` (solo `UPDATE`) y `where`.

Comportamiento: mismas guardas + medición de impacto dentro de una transacción
con `ROLLBACK` + token en el store con TTL. **Nada se persiste.**

Salida: `{ token, affected_rows, summary }`.

Las guardas aplican **idénticas** vía MCP: `UPDATE`/`DELETE` sin `WHERE`
rechazado, tope de filas afectadas, whitelist de tabla + operación. Un input
inválido devuelve un **error de herramienta** (no un pending).

### `confirm`

**Solicita** confirmación humana; **no ejecuta**.

- Input: `{ token }`.
- Comportamiento: valida el token (existe, no expiró, es de origen `mcp`), lo
  marca como `pending_approval` y devuelve la URL donde un humano debe aprobar.
  **No toca la base.**
- Salida: `{ status: "pending_approval", approval_url, message }`, con un mensaje
  claro de que el agente no puede ejecutar.

---

## Modelo de seguridad

El agente y el humano comparten el mismo binario, así que "confiar en que el
cliente MCP pida aprobación" no alcanza. La garantía es más fuerte: **la
credencial del agente físicamente no puede ejecutar.**

### Dos superficies, dos credenciales

| Superficie | Ruta | Credencial | Puede |
|---|---|---|---|
| **MCP (agente)** | `/mcp` | `MCP_AUTH_TOKEN` (bearer) | `preview` y **solicitar** confirmación. **Nunca** ejecuta ni hace `COMMIT`. |
| **Humana (UI / API)** | `/`, `/preview`, `/confirm`, `/pending*`, `/approvals` | `UI_AUTH_TOKEN` (bearer, opcional) | Previsualizar y **aprobar/ejecutar**. |

El token de MCP **no** da acceso a la superficie humana.

### Origen y estado del token

Cada token en el store lleva:

- **`origin`**: `ui` (creado desde la superficie humana) o `mcp` (creado por el
  agente).
- **`state`**: `previewed` → `pending_approval` → `executed` / `rejected` /
  `expired`.

### Gating de ejecución

- Un token con `origin = mcp` **solo** se ejecuta vía la acción de **aprobación
  humana** (`POST /pending/{token}/approve`), nunca por un path alcanzable con la
  credencial del agente.
- `POST /confirm` (flujo UI existente) sigue funcionando para tokens
  `origin = ui`; para tokens `origin = mcp` responde **`409`** (deben pasar por
  aprobación).

> 🔒 **No negociable:** el `confirm` lo aprieta **siempre un humano**. El agente
> previsualiza y propone; ejecutar es de una persona.

---

## Cómo conectar un cliente MCP

El transporte es **Streamable HTTP**, servido por el mismo binario y el mismo
puerto que la API.

- **URL**: `http(s)://<host>:8080/mcp` (o la ruta que configures en `MCP_PATH`).
- **Header**: `Authorization: Bearer <MCP_AUTH_TOKEN>`.

### Ejemplo de configuración (cliente genérico)

```json
{
  "mcpServers": {
    "deitafix": {
      "type": "streamable-http",
      "url": "https://deitafix.midominio.com/mcp",
      "headers": {
        "Authorization": "Bearer EL_MCP_AUTH_TOKEN"
      }
    }
  }
}
```

> El formato exacto depende del cliente MCP. Lo esencial es: transporte
> Streamable HTTP, la URL del endpoint y el header `Authorization` con el bearer.

---

## Flujo de aprobación humano, paso a paso

1. **El agente previsualiza.** Llama a la herramienta `preview` (SQL crudo u
   operación acotada). Deitafix parsea, aplica las guardas y mide el impacto con
   `ROLLBACK`. Devuelve `{ token, affected_rows, summary }`. Nada se ejecutó.

2. **El agente solicita confirmación.** Llama a la herramienta `confirm` con el
   token. Deitafix marca el token como `pending_approval` y devuelve
   `{ status: "pending_approval", approval_url, message }`. **La base no se
   tocó.** El agente no puede hacer más: no tiene forma de ejecutar.

3. **Un humano revisa.** Abre `approval_url` (la pantalla **"Aprobaciones
   pendientes"**, `/approvals`). Ve la lista de propuestas del agente y, en el
   detalle de cada una, el **impacto** (filas afectadas), el **SQL exacto** que
   se ejecutaría y el TTL restante.

4. **Un humano decide.**
   - **Aprobar y ejecutar** → `POST /pending/{token}/approve`: recién acá ocurre
     el `COMMIT`. El token se consume (un solo uso).
   - **Rechazar** → `POST /pending/{token}/reject`: se descarta el token, nada
     cambia en la base.

Si el token expira antes de aprobarse, deja de ser aprobable (error claro) y hay
que volver a previsualizar.

### Endpoints de la superficie humana

| Método | Ruta | Descripción |
|---|---|---|
| `GET` | `/pending` | Lista los tokens `origin = mcp` en `pending_approval` (JSON). |
| `POST` | `/pending/{token}/approve` | Ejecuta + `COMMIT`. Token de un solo uso. |
| `POST` | `/pending/{token}/reject` | Descarta el token. |
| `GET` | `/approvals` | Pantalla web "Aprobaciones pendientes" (a la que apunta `approval_url`). |

---

## Variables de entorno

| Variable | Descripción |
|---|---|
| `MCP_ENABLED` | On/off de la capa MCP. `false` (default) = endpoint `/mcp` apagado. |
| `MCP_AUTH_TOKEN` | Bearer para `/mcp`. **Obligatorio** si `MCP_ENABLED=true`. |
| `MCP_PATH` | Ruta del endpoint MCP (default `/mcp`). |
| `MCP_APPROVAL_BASE_URL` | Base pública (esquema + host + puerto) para la `approval_url` de la herramienta `confirm`. Si se omite, la URL es relativa (`/approvals`). |
| `UI_AUTH_TOKEN` | *(Opcional, recomendado)* Bearer que protege la superficie humana; la credencial MCP no debe alcanzarla. |

**Degradación limpia** (consistente con `DATAFIX_ENABLED` / `AI_API_KEY`): si
`MCP_ENABLED=false` o falta `MCP_AUTH_TOKEN`, el endpoint MCP no se registra y el
resto del servicio queda intacto. Si `MCP_ENABLED=true` **sin** token, el
arranque aborta con un error claro (no se expone un endpoint MCP sin auth).

---

## Alcance

La capa MCP es un transporte fino sobre el core: **no** reimplementa ninguna
salvaguarda ni abre ningún atajo. Todo pasa por las guardas, el preview y el
store de tokens existentes.

Fuera del alcance de esta versión (v0.4.0):

- Features LLM (NL→SQL, explicación de impacto, revisor IA) → **v0.5.0**.
- Aprobación *four-eyes* y *audit log* persistente → **v2+**.
