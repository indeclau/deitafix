# Capa de IA (opcional)

Deitafix suma una **capa de IA opcional** sobre su core seguro (`preview → confirm`):
traduce lenguaje natural a SQL, explica el impacto de una escritura en lenguaje
claro y funciona como un segundo par de ojos sobre la sentencia. Es **estrictamente
asistiva**.

> 🔒 **Invariante central, no negociable:** la IA **solo propone, nunca ejecuta.**
> Todo SQL que sugiere pasa por **las mismas guardas** y el mismo flujo
> `preview → confirm` que el SQL crudo de un humano. La IA **no tiene camino a
> `confirm`**: no puede hacer `COMMIT`, no toca la base (salvo introspección de
> esquema de solo lectura) y no participa de la ejecución.

La capa es **opcional**: sin `AI_API_KEY` degrada de forma limpia y el resto del
servicio funciona idéntico. A diferencia de la [capa MCP](./mcp.md), **la ausencia
de clave NO aborta el arranque** — es el modo degradado esperado.

---

## Qué aporta

La capa de IA suma tres features, todas del lado del `preview` (nunca del `confirm`):

### 1. NL → SQL — `POST /ai/suggest`

Traduce una intención en lenguaje natural a **una** sentencia SQL de escritura
candidata (`UPDATE`, `DELETE` o `INSERT` con `VALUES` explícitos; nunca `SELECT`,
DDL ni múltiples sentencias). El modelo recibe como contexto el motor de la base y
el esquema acotado a la whitelist (ver [Introspección de esquema](#introspección-de-esquema-para-nl--sql)).

Devuelve `{ sql, rationale, engine }`, donde `rationale` explica en una o dos frases
por qué eligió esa sentencia.

> El `sql` devuelto es un **candidato sin validar**. Todavía NO pasó por las guardas
> ni por el preview. El cliente **debe** mandarlo a `POST /preview`, donde se valida
> igual que cualquier SQL humano.

### 2. Explicación de impacto (en `/preview`)

Sobre una sentencia **ya validada y medida** (el preview ya midió `affected_rows`
dentro de una transacción con `ROLLBACK`), la IA traduce el crudo *"N filas
afectadas"* a lenguaje claro y accionable, y estima una **señal de riesgo**:
`low`, `medium` o `high`.

El riesgo **no lo decide solo el modelo**. Se combina una **heurística
determinística** (un `DELETE` pesa más que un `UPDATE`; cuanto más se acerca
`affected_rows` al tope de filas, más alto el riesgo) con la señal del modelo,
tomando **la más conservadora de las dos**. Aunque el modelo falle o dé timeout,
`risk_level` sigue siendo significativo: es el piso determinístico.

### 3. Revisor IA (campo `flags` del preview)

Un segundo par de ojos **parcial** sobre la sentencia. Marca patrones sospechosos:

- un `WHERE` que parece demasiado amplio para la intención,
- un `DELETE` que afecta muchas filas,
- modificación de columnas sensibles (por ejemplo `password`, saldo, rol, estado
  de pago),
- valores que no matchean el tipo esperado de la columna.

Cada flag lleva una `severity` (`info` | `warning` | `danger`) y un `message`.

> ⚠️ **El revisor informa, no bloquea.** Es un par de ojos parcial, no una barrera
> de seguridad. El **gate real** siguen siendo las guardas del servidor (whitelist,
> tope de filas, `WHERE` obligatorio en `UPDATE`/`DELETE`). Un flag `danger` no
> impide confirmar; una review vacía no autoriza nada.

---

## Invariantes de seguridad

La lógica de aplicación de IA vive separada del core `preview → confirm`. Se apoya
en estos invariantes:

1. **La IA solo propone.** `SuggestSQL` genera un candidato y **no toca la base**
   (a lo sumo introspecciona el esquema, que es de solo lectura). El candidato
   vuelve por `POST /preview`, donde pasa por las mismas guardas.

2. **El candidato de `/ai/suggest` vuelve por `/preview`.** No hay atajo. La
   sugerencia es texto; recién en el preview se parsea, se aplican las guardas y se
   mide el impacto con `ROLLBACK`. Un `UPDATE`/`DELETE` sin `WHERE` sugerido por la
   IA se rechaza en el preview **exactamente igual** que si lo hubiera escrito una
   persona.

3. **El enriquecimiento del preview es best-effort y está AISLADO.** Cuando la IA
   está habilitada, `/preview` intenta agregar explicación + riesgo + flags, pero:
   - se corre con **su propio timeout**, independiente del preview y de la base;
   - un **fallo o timeout de IA NUNCA rompe el preview ni el confirm** — el preview
     ya tiene su `token` y su `affected_rows` **antes** de llamar a la IA;
   - ante un fallo, el bloque `ai` cae a un insight parcial (con el riesgo
     heurístico) o a `null`, jamás a un error que suba al preview.

4. **La IA no llega a `confirm`.** Nada de la capa de IA participa del confirm.
   No hay forma de que una sugerencia se ejecute sin pasar por el
   `preview → confirm` humano.

5. **La salida del modelo es input NO confiable.** Se trata como posible prompt
   injection. El parseo es **defensivo**: si el modelo devuelve algo que no matchea
   el JSON esperado, se degrada con un mensaje neutro en vez de crashear. La
   seguridad la garantizan el parser real y el **usuario restringido de la base**,
   nunca el modelo.

### Desactivar la IA por request

Aunque la capa esté habilitada, se la puede apagar **por request** en `/preview`
mandando `"ai": false` en el body. En ese caso el preview responde con `"ai": null`
y no llama al proveedor. Útil para evitar latencia extra o costo cuando no se quiere
el enriquecimiento.

---

## Configuración

| Variable | Descripción |
|---|---|
| `AI_API_KEY` | **Habilita** la capa de IA. Si está vacía, la IA degrada limpio y el arranque **no falla**. |
| `AI_MODEL` | Modelo a usar. Default `claude-opus-4-8`. |
| `AI_BASE_URL` | Endpoint del proveedor. Default `https://api.anthropic.com` (por ejemplo, para un proxy o gateway compatible). |
| `AI_TIMEOUT` | Timeout por request de IA, **independiente** del timeout de la base. Default `15s`. Formato de `time.ParseDuration` (por ejemplo `10s`, `1m`). |

La capa real habla la **Messages API de Anthropic** por HTTP, con su propio timeout,
y parsea la salida del modelo de forma defensiva.

### Cómo obtener una API key de Anthropic

1. Entrá a la [Anthropic Console](https://console.anthropic.com/).
2. Creá (o iniciá sesión en) tu organización.
3. En **API Keys**, generá una clave nueva.
4. Exportála como `AI_API_KEY` en el entorno del servicio (o cargala vía `.env` en
   desarrollo; en producción, vía el orquestador).

> El modelo por defecto es `claude-opus-4-8` (Opus 4.8), un modelo actual de
> Anthropic. Si Anthropic lo renombra, se cambia con `AI_MODEL` **sin tocar código**.

---

## Degradación limpia sin `AI_API_KEY`

La configuración de IA sigue el mismo criterio que `DEITAFIX_ENABLED` /
`MCP_ENABLED`: **si no hay `AI_API_KEY`, la capa queda apagada y el resto del
servicio intacto.** A diferencia de MCP, la **ausencia de clave NO es un error**: es
el modo degradado esperado.

El arranque **solo falla** si un valor **presente** es inválido — por ejemplo,
`AI_TIMEOUT` no parseable o no positivo —, para no arrancar con una config
silenciosamente rota.

Con la IA apagada:

| Superficie | Comportamiento |
|---|---|
| `POST /ai/suggest` | Responde **`503`** (IA no configurada). |
| `POST /preview` | Devuelve `"ai": null`. El preview funciona idéntico: `token`, `affected_rows`, `summary`. |
| Resto del servicio | **Idéntico.** Guardas, whitelist, tope de filas, `confirm`, MCP: todo sin cambios. |

No crashea ni loguea en loop: el cliente noop devuelve un error centinela y deja que
el handler decida. Así no hay ruido cuando la IA está apagada a propósito.

---

## Introspección de esquema (para NL → SQL)

Para que el modelo genere columnas reales (y no inventadas) en NL → SQL, la capa
**introspecciona el esquema** de las tablas de la whitelist y se lo pasa como
contexto: una lista compacta de tablas con sus columnas y tipos, tomada de
`information_schema` y **acotada a lo que el usuario restringido puede ver**.

Propiedades clave:

- **Acotada a la whitelist.** Solo se introspeccionan las tablas permitidas; nunca
  el esquema completo de la base.
- **Best-effort, con su propio timeout.** Si el motor no soporta introspección, o la
  consulta falla o tarda, se devuelve vacío y el modelo trabaja solo con la
  intención. **Nunca propaga el error:** la sugerencia debe poder generarse igual.
- **Con cache (TTL).** El esquema se cachea con un TTL para no golpear
  `information_schema` en cada sugerencia.
- **Jamás afecta la ruta segura.** Es solo lectura y solo alimenta el prompt de
  NL → SQL. No participa del `preview`, del `confirm` ni de las guardas.

---

## Ejemplos

### `POST /ai/suggest` — NL → SQL

**Request**

```json
POST /ai/suggest
{
  "intent": "marcar como inactivos los usuarios que no ingresan desde 2023"
}
```

**Response (200)** — la sugerencia es un **candidato sin validar**:

```json
{
  "sql": "UPDATE users SET active = false WHERE last_login < '2023-01-01'",
  "rationale": "Marca inactivos solo a los usuarios cuyo último login es anterior a 2023, acotando con WHERE para no tocar el resto.",
  "engine": "postgres"
}
```

El siguiente paso es **obligatorio**: mandar ese `sql` a `POST /preview`.

**Response (503)** — con la IA no configurada (sin `AI_API_KEY`):

```json
{ "error": "ai: capa de IA no configurada (falta AI_API_KEY)" }
```

### El bloque `ai` en `POST /preview`

Con la IA habilitada, el preview de una sentencia ya validada incluye el bloque `ai`:

```json
POST /preview
{ "sql": "UPDATE users SET active = false WHERE last_login < '2023-01-01'" }
```

```json
{
  "token": "…",
  "affected_rows": 128,
  "summary": "UPDATE sobre users, 128 filas",
  "ai": {
    "explanation": "Vas a desactivar 128 usuarios que no ingresan desde antes de 2023. Es una porción moderada respecto del tope permitido; revisá que la fecha de corte sea la correcta antes de confirmar.",
    "risk_level": "medium",
    "flags": [
      {
        "severity": "warning",
        "message": "El WHERE depende de last_login; verificá que ese campo se actualice en cada login real."
      }
    ]
  }
}
```

**Con la IA apagada** (sin key, o `"ai": false` en el request), el mismo preview
devuelve `"ai": null` y todo lo demás igual:

```json
{
  "token": "…",
  "affected_rows": 128,
  "summary": "UPDATE sobre users, 128 filas",
  "ai": null
}
```

---

## Alcance

La capa de IA es **estrictamente asistiva**: no reimplementa ninguna salvaguarda ni
abre ningún atajo. Todo lo que produce vuelve a pasar por las guardas, el `preview` y
el `confirm` existentes. La IA propone; **ejecutar sigue siendo decisión de una
persona.**
