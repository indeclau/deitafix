# API HTTP

Contrato de la API HTTP de Deitafix: el flujo **`preview → confirm`** sobre una
base de producción, con las guardas y el token de un solo uso forzados del lado
del servidor.

La idea es simple y no negociable: **nada se ejecuta sin previsualizarse
primero**. Un `POST /preview` mide el impacto dentro de una transacción con
`ROLLBACK` (no persiste nada) y devuelve un **token**; recién un `POST /confirm`
con ese token ejecuta y hace `COMMIT`. El motor de la base es una propiedad del
servidor, no algo que el cliente elija por request.

Para la superficie del agente (MCP) y el flujo de aprobación humana, ver
[`mcp.md`](./mcp.md). Este documento describe la superficie HTTP humana y las
probes.

---

## Índice

- [Convenciones](#convenciones)
- [Autenticación](#autenticación)
- [`POST /preview`](#post-preview)
- [`POST /confirm`](#post-confirm)
- [`POST /ai/suggest`](#post-aisuggest)
- [Aprobación humana (`/pending`)](#aprobación-humana-pending)
- [Probes: `/healthz` y `/readyz`](#probes-healthz-y-readyz)
- [Tabla resumen de códigos de error](#tabla-resumen-de-códigos-de-error)

---

## Convenciones

- **Content-Type**: todas las respuestas son `application/json`. Los cuerpos de
  request se esperan en JSON.
- **Errores**: siempre con la misma forma, un objeto con un único campo `error`:

  ```json
  { "error": "api: falta el token" }
  ```

- **Campos desconocidos**: el decodificador usa `DisallowUnknownFields`. Un
  campo que no esté en el contrato (un typo, un campo de más) hace que el
  request falle con **`400`**, en vez de ignorarlo en silencio. Esto es
  especialmente relevante en `/confirm` (ver más abajo).
- **Feature flag maestro**: si `DATAFIX_ENABLED` no está en un valor afirmativo
  (`1`, `true`, `yes`, `on`), las rutas de escritura (`/preview`, `/confirm`,
  `/ai/suggest`, `/pending*`) responden **`503`** sin tocar la base. Las probes
  (`/healthz`, `/readyz`) siguen respondiendo.
- **El motor lo elige el servidor**: el campo `engine` se acepta en los cuerpos
  que lo aceptan, pero **se ignora**. El motor real es el de la base a la que
  apunta `DATABASE_URL` (`postgres` | `mysql`); no es algo que el cliente pueda
  elegir por request.

---

## Autenticación

La autenticación bearer es **opcional** y se configura por variable de entorno.
Hay dos superficies con dos credenciales distintas:

| Superficie | Rutas | Variable | Requerido |
|---|---|---|---|
| **Humana (UI / API)** | `/preview`, `/confirm`, `/ai/suggest`, `/pending*` | `UI_AUTH_TOKEN` | Opcional (recomendado) |
| **MCP (agente)** | `/mcp` (ver [`mcp.md`](./mcp.md)) | `MCP_AUTH_TOKEN` | Obligatorio si `MCP_ENABLED=true` |

- Si `UI_AUTH_TOKEN` está **vacío**, la superficie humana **no exige** bearer
  (comportamiento histórico). Si está seteado, hay que mandar el header
  `Authorization: Bearer <UI_AUTH_TOKEN>`.
- Con el bearer requerido, un header ausente o incorrecto devuelve **`401`** con
  `WWW-Authenticate: Bearer`. La comparación del token es en tiempo constante.
- La credencial MCP **no** da acceso a la superficie humana, y viceversa. Es
  defensa en profundidad: la garantía dura de human-in-the-loop se apoya en el
  origen del token, no en esta credencial (ver [`mcp.md`](./mcp.md)).
- Las probes (`/healthz`, `/readyz`) **no** requieren auth.

Header, cuando aplica:

```
Authorization: Bearer <UI_AUTH_TOKEN>
```

---

## `POST /preview`

Valida una operación de escritura, mide su impacto dentro de una transacción con
`ROLLBACK` (**no persiste nada**) y devuelve un **token de un solo uso** para el
`confirm` posterior.

### Request

El SQL efectivo puede venir de **dos modos mutuamente excluyentes** — hay que
enviar **exactamente uno**:

**Modo 1 — SQL crudo:**

```json
{
  "sql": "UPDATE CollectionBox SET status = 'closed' WHERE id = 42",
  "ai": true
}
```

**Modo 2 — Operación acotada:** el servicio traduce a SQL parametrizado.

```json
{
  "operation": {
    "op": "UPDATE",
    "table": "CollectionBox",
    "set": { "status": "closed" },
    "where": { "id": 42 }
  }
}
```

Campos:

| Campo | Tipo | Descripción |
|---|---|---|
| `sql` | string | SQL crudo. Excluyente con `operation`. |
| `operation` | objeto | Operación acotada. Excluyente con `sql`. |
| `operation.op` | string | `UPDATE` o `DELETE` (únicos válidos en modo acotado). |
| `operation.table` | string | Tabla objetivo (debe estar en la whitelist). |
| `operation.set` | objeto | Columnas a asignar. Solo para `UPDATE`. |
| `operation.where` | objeto | Condición. Obligatoria para `UPDATE`/`DELETE`. |
| `ai` | bool (opcional) | Enriquecimiento de IA best-effort. Ausente = usar IA si está habilitada (default on); `false` la omite. |
| `engine` | string (opcional) | **Se acepta pero se IGNORA.** El motor lo define el servidor. |

Notas:

- Enviar `sql` **y** `operation` a la vez es ambiguo → **`400`**.
- No enviar ninguno de los dos → **`400`**.
- `ai` es un booleano opcional. Al estar **ausente**, la IA se usa si está
  habilitada (default on). Con `"ai": false` se omite el enriquecimiento para no
  pagar latencia/costo. Aunque se pida, la IA es best-effort y aislada: si está
  apagada o falla, `ai` vuelve `null` y el resto del preview queda intacto.

### Response `200`

```json
{
  "token": "e3b0c44298fc1c14",
  "affected_rows": 1,
  "summary": "UPDATE sobre \"CollectionBox\" afectaría 1 fila(s). Revisá antes de confirmar.",
  "ai": {
    "explanation": "Cierra una única caja de colecta por id.",
    "risk_level": "low",
    "flags": [
      { "severity": "info", "message": "Operación acotada a un solo registro." }
    ]
  }
}
```

| Campo | Tipo | Descripción |
|---|---|---|
| `token` | string | Token de un solo uso para `POST /confirm`. |
| `affected_rows` | int | Filas que la operación afectaría (medido con `ROLLBACK`). |
| `summary` | string | Resumen legible del impacto. |
| `ai` | objeto \| `null` | Bloque de enriquecimiento de IA, o `null` si la IA está apagada, se pidió omitir (`ai:false`) o falló. |
| `ai.explanation` | string | Explicación en lenguaje natural. |
| `ai.risk_level` | string | Nivel de riesgo evaluado por la IA. |
| `ai.flags` | array | Observaciones del revisor IA. |
| `ai.flags[].severity` | string | Severidad del flag. |
| `ai.flags[].message` | string | Detalle del flag. |

Cuando no hay IA, el campo `ai` es explícitamente `null` (no se omite):

```json
{
  "token": "e3b0c44298fc1c14",
  "affected_rows": 1,
  "summary": "UPDATE sobre \"CollectionBox\" afectaría 1 fila(s). Revisá antes de confirmar.",
  "ai": null
}
```

### Códigos de error

| Código | Cuándo |
|---|---|
| `200` | Preview exitoso. |
| `400` | Input vacío (ni `sql` ni `operation`); input ambiguo (ambos); JSON inválido o con campo desconocido; `operation.op` distinto de `UPDATE`/`DELETE`; SQL vacío o no parseable. |
| `422` | Rechazo de guarda: `UPDATE`/`DELETE` sin `WHERE`; tabla fuera de la whitelist; tope de filas afectadas excedido (`MAX_AFFECTED_ROWS`); operación no permitida (p. ej. `SELECT`/DDL/`DROP`/`TRUNCATE`/multi-statement); `INSERT ... SELECT`. |
| `503` | Servicio deshabilitado (`DATAFIX_ENABLED=false`). |
| `401` | Falta o es incorrecto el bearer, si `UI_AUTH_TOKEN` está seteado. |

> Los rechazos de guarda son **`422`** a propósito: el request es sintácticamente
> válido, pero la operación no está permitida. Es distinto de un `400` (cuerpo
> mal formado).

### Ejemplo (PowerShell)

```powershell
$body = @{
  sql = "UPDATE CollectionBox SET status = 'closed' WHERE id = 42"
  ai  = $true
} | ConvertTo-Json

Invoke-RestMethod -Method Post -Uri "http://localhost:8080/preview" `
  -ContentType "application/json" `
  -Headers @{ Authorization = "Bearer $env:UI_AUTH_TOKEN" } `
  -Body $body
```

Con `curl.exe` (continuación de línea con backtick):

```powershell
curl.exe -X POST "http://localhost:8080/preview" `
  -H "Content-Type: application/json" `
  -H "Authorization: Bearer $env:UI_AUTH_TOKEN" `
  -d '{\"sql\":\"UPDATE CollectionBox SET status = ''closed'' WHERE id = 42\",\"ai\":true}'
```

---

## `POST /confirm`

Ejecuta y persiste (`COMMIT`) la operación asociada a un token de un solo uso. El
token se invalida al recuperarlo, aunque la ejecución falle.

> **Solo acepta el token, nunca SQL.** Este endpoint **no** re-recibe la
> sentencia: ejecuta lo que ya se validó y guardó en el `preview`. Gracias a
> `DisallowUnknownFields`, mandar cualquier campo de más —incluido `sql`— da
> **`400`**. No hay forma de que el `confirm` ejecute algo distinto de lo
> previsualizado.

### Request

```json
{ "token": "e3b0c44298fc1c14" }
```

| Campo | Tipo | Descripción |
|---|---|---|
| `token` | string | El token devuelto por `POST /preview`. Único campo aceptado. |

### Response `200`

```json
{
  "affected_rows": 1,
  "summary": "Confirmado: 1 fila(s) afectada(s) en \"CollectionBox\"."
}
```

| Campo | Tipo | Descripción |
|---|---|---|
| `affected_rows` | int | Filas efectivamente afectadas por el `COMMIT`. |
| `summary` | string | Resumen legible del resultado. |

### Códigos de error

| Código | Cuándo |
|---|---|
| `200` | Ejecución confirmada. |
| `400` | Falta `token` (vacío o ausente); JSON inválido; **cualquier campo desconocido** (incl. `sql`). |
| `404` | Token inexistente, expirado o ya usado. |
| `409` | El token es de origen `mcp` y requiere aprobación humana (no se ejecuta por `confirm`); o el token está en un estado incorrecto para esta acción. |
| `503` | Servicio deshabilitado (`DATAFIX_ENABLED=false`). |
| `401` | Falta o es incorrecto el bearer, si `UI_AUTH_TOKEN` está seteado. |

> Un token creado por el agente (`origin=mcp`) **no** se ejecuta por esta vía: da
> **`409`** y debe pasar por [aprobación humana](#aprobación-humana-pending). La
> garantía de human-in-the-loop se fuerza del lado del servidor, no se confía al
> cliente. Ver [`mcp.md`](./mcp.md).

### Ejemplo (PowerShell)

```powershell
$body = @{ token = "e3b0c44298fc1c14" } | ConvertTo-Json

Invoke-RestMethod -Method Post -Uri "http://localhost:8080/confirm" `
  -ContentType "application/json" `
  -Headers @{ Authorization = "Bearer $env:UI_AUTH_TOKEN" } `
  -Body $body
```

---

## `POST /ai/suggest`

Traduce una intención en **lenguaje natural** a un **SQL candidato**. Es una
ayuda de redacción: el candidato **no está validado** y **no toca la base**
(salvo introspección de solo lectura del esquema). El SQL propuesto **debe volver
por [`POST /preview`](#post-preview)** para pasar por las guardas antes de poder
confirmarse.

### Request

```json
{
  "intent": "cerrar todas las cajas de colecta creadas antes de 2025",
  "schema": "CollectionBox(id, status, created_at)"
}
```

| Campo | Tipo | Descripción |
|---|---|---|
| `intent` | string | La intención en lenguaje natural. **Obligatorio.** |
| `schema` | string (opcional) | Contexto de esquema para guiar la generación. |
| `engine` | string (opcional) | **Se acepta pero se IGNORA.** El motor lo define el servidor. |

### Response `200`

```json
{
  "sql": "UPDATE CollectionBox SET status = 'closed' WHERE created_at < '2025-01-01'",
  "rationale": "Filtra por fecha de creación y actualiza el estado a cerrado.",
  "engine": "postgres",
  "note": "SQL candidato SIN validar: envialo a POST /preview para pasar por las guardas antes de confirmar."
}
```

| Campo | Tipo | Descripción |
|---|---|---|
| `sql` | string | SQL candidato **sin validar**. |
| `rationale` | string | Por qué la IA propone ese SQL. |
| `engine` | string | Motor del servidor (informativo). |
| `note` | string | Recordatorio de que el candidato aún no pasó por las guardas. |

### Códigos de error

| Código | Cuándo |
|---|---|
| `200` | Candidato generado. |
| `400` | Falta `intent` (vacío o solo espacios); JSON inválido o con campo desconocido. |
| `503` | La capa de IA está deshabilitada (no hay `AI_API_KEY`). Degradación limpia, **no** `500`. |
| `503` | Servicio deshabilitado (`DATAFIX_ENABLED=false`). |
| `401` | Falta o es incorrecto el bearer, si `UI_AUTH_TOKEN` está seteado. |

> Si no hay `AI_API_KEY` configurada, la respuesta es un **`503`** limpio (la IA
> simplemente no se ofrece), no un error de servidor. El resto de la API funciona
> idéntico.

### Ejemplo (PowerShell)

```powershell
$body = @{
  intent = "cerrar todas las cajas de colecta creadas antes de 2025"
  schema = "CollectionBox(id, status, created_at)"
} | ConvertTo-Json

Invoke-RestMethod -Method Post -Uri "http://localhost:8080/ai/suggest" `
  -ContentType "application/json" `
  -Headers @{ Authorization = "Bearer $env:UI_AUTH_TOKEN" } `
  -Body $body
```

---

## Aprobación humana (`/pending`)

Estos endpoints son la superficie humana del flujo de **aprobación de propuestas
del agente** (tokens `origin=mcp`). Un preview creado por el agente vía MCP
queda a la espera de que **una persona** lo apruebe o rechace: la credencial del
agente físicamente no puede ejecutar.

El detalle completo del flujo (cómo el agente propone, el modelo de seguridad,
cómo conectar un cliente MCP) está en [`mcp.md`](./mcp.md). Acá va el contrato
HTTP de la superficie humana.

### `GET /pending`

Lista los tokens `origin=mcp` a la espera de aprobación.

Response `200`:

```json
{
  "pending": [
    {
      "token": "e3b0c44298fc1c14",
      "op": "UPDATE",
      "table": "CollectionBox",
      "affected_rows": 3,
      "sql": "UPDATE CollectionBox SET status = 'closed' WHERE created_at < '2025-01-01'",
      "expires_in_sec": 240
    }
  ]
}
```

| Campo | Tipo | Descripción |
|---|---|---|
| `pending[].token` | string | Token de la propuesta. |
| `pending[].op` | string | Operación (`UPDATE`, `DELETE`, `INSERT`). |
| `pending[].table` | string | Tabla objetivo. |
| `pending[].affected_rows` | int | Impacto medido en el preview. |
| `pending[].sql` | string | SQL exacto que se ejecutaría. |
| `pending[].expires_in_sec` | int | Segundos restantes de TTL. |

### `POST /pending/{token}/approve`

Aprueba y **ejecuta** (`COMMIT`) la propuesta. Es el equivalente humano del
`confirm` para los tokens del agente; **solo acá** ocurre el `COMMIT` de un
token `origin=mcp`. Token de un solo uso.

Response `200` (misma forma que `POST /confirm`):

```json
{
  "affected_rows": 3,
  "summary": "Confirmado: 3 fila(s) afectada(s) en \"CollectionBox\"."
}
```

### `POST /pending/{token}/reject`

Descarta la propuesta **sin ejecutar nada**. Consume el token.

Response `200`:

```json
{ "status": "rejected" }
```

### Códigos de error (`/pending*`)

| Código | Cuándo |
|---|---|
| `200` | Listado / aprobación / rechazo exitoso. |
| `400` | Falta el `{token}` en la ruta (approve / reject). |
| `404` | Token inexistente, expirado o ya usado. |
| `409` | Token en estado incorrecto (p. ej. no está `pending_approval`). |
| `503` | Servicio deshabilitado (`DATAFIX_ENABLED=false`). |
| `401` | Falta o es incorrecto el bearer, si `UI_AUTH_TOKEN` está seteado. |

### Ejemplo (PowerShell)

```powershell
# Listar pendientes
Invoke-RestMethod -Method Get -Uri "http://localhost:8080/pending" `
  -Headers @{ Authorization = "Bearer $env:UI_AUTH_TOKEN" }

# Aprobar y ejecutar
$token = "e3b0c44298fc1c14"
Invoke-RestMethod -Method Post -Uri "http://localhost:8080/pending/$token/approve" `
  -Headers @{ Authorization = "Bearer $env:UI_AUTH_TOKEN" }

# Rechazar
Invoke-RestMethod -Method Post -Uri "http://localhost:8080/pending/$token/reject" `
  -Headers @{ Authorization = "Bearer $env:UI_AUTH_TOKEN" }
```

---

## Probes: `/healthz` y `/readyz`

Ambas son públicas (sin auth) y **no** dependen de `DATAFIX_ENABLED`. Están
pensadas para orquestadores (Kubernetes, etc.).

### `GET /healthz` — liveness

Indica que el proceso está vivo y sirviendo HTTP. **No toca la base.** Siempre
responde **`200`**:

```json
{ "status": "ok" }
```

### `GET /readyz` — readiness

Hace `ping` a la base con el usuario restringido (con un timeout acotado). Sirve
para que el orquestador no enrute tráfico hasta que la base sea alcanzable.

- **`200`** si conecta:

  ```json
  { "status": "ok" }
  ```

- **`503`** si no conecta:

  ```json
  { "status": "unavailable" }
  ```

### Ejemplo (PowerShell)

```powershell
Invoke-RestMethod -Method Get -Uri "http://localhost:8080/healthz"
Invoke-RestMethod -Method Get -Uri "http://localhost:8080/readyz"
```

---

## Tabla resumen de códigos de error

| Código | Significado | Endpoints / causas típicas |
|---|---|---|
| `200` | OK | Todos los endpoints, camino feliz. |
| `400` | Bad Request | JSON inválido; campo desconocido (`DisallowUnknownFields`); `/preview` con input vacío o ambiguo, `op` inválido, SQL vacío/no parseable; `/confirm` sin `token` o con `sql` u otro campo de más; `/ai/suggest` sin `intent`; falta el `{token}` en la ruta de approve/reject. |
| `401` | Unauthorized | Bearer ausente o incorrecto cuando `UI_AUTH_TOKEN` (superficie humana) o `MCP_AUTH_TOKEN` (MCP) están seteados. |
| `404` | Not Found | Token inexistente, expirado o ya usado (`/confirm`, `/pending/{token}/…`). |
| `409` | Conflict | Token `origin=mcp` enviado a `/confirm` (requiere aprobación humana); token en estado incorrecto para la acción (p. ej. aprobar algo que no está pendiente). |
| `422` | Unprocessable Entity | Rechazo de guarda en `/preview`: `UPDATE`/`DELETE` sin `WHERE`, tabla fuera de whitelist, tope de filas excedido, operación no permitida (`SELECT`/DDL/`DROP`/`TRUNCATE`/multi-statement), `INSERT ... SELECT`. |
| `503` | Service Unavailable | `DATAFIX_ENABLED=false` (rutas de escritura); `/readyz` sin conexión a la base; `/ai/suggest` sin `AI_API_KEY` (IA deshabilitada). |
| `500` | Internal Server Error | Error inesperado no clasificado (fallo del motor, etc.). |

---

## Ver también

- [`mcp.md`](./mcp.md) — la superficie MCP para agentes, el modelo de seguridad
  de dos credenciales y el flujo de aprobación humana en detalle.
