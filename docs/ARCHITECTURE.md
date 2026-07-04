# Arquitectura

Deitafix es un servicio self-hosted para ejecutar **escrituras ocasionales**
(`UPDATE` / `DELETE` / `INSERT`) sobre una base de **producción**, con un flujo
**preview → confirm**. Este documento describe cómo está construido: el flujo,
las capas de seguridad, dónde vive el estado y cómo se organizan los paquetes.

Para el **detalle del modelo de amenazas** (qué ataque mitiga cada capa, las
decisiones de política sobre casos ambiguos) ver el [threat model](SECURITY.md).
Este documento describe la **arquitectura**; no lo duplica.

---

## Visión general y principio de diseño

El principio rector es uno solo:

> **Nada se ejecuta a ciegas.** Toda escritura pasa primero por un *preview* que
> mide su impacto real sin persistir nada, y recién un *confirm* explícito la
> ejecuta.

De ahí se derivan las decisiones de diseño:

- El **preview** parsea, valida y **mide el impacto** ejecutando la sentencia
  dentro de una transacción que **siempre hace `ROLLBACK`**: se obtiene el número
  real de filas afectadas sin tocar la base.
- El **confirm** nunca acepta SQL: solo un **token de un solo uso** que
  referencia la sentencia ya validada y guardada del lado servidor. Se ejecuta
  exactamente lo que se previsualizó.
- Ninguna garantía descansa en una sola capa: guardas, tope de filas y usuario
  restringido son **defensa en profundidad** (ver [threat model](SECURITY.md)).
- Los **extras** (capa MCP para agentes, capa de IA) son opcionales y **no tocan
  la ruta segura**: todo pasa por las mismas guardas, el mismo preview y el mismo
  store.

El transporte HTTP (router chi, JSON) se mantiene separado de la lógica de
aplicación (`Service`) para poder testear la orquestación sin levantar un
servidor.

---

## El flujo preview → confirm

### Preview, paso a paso

El corazón del servicio es el método `preview()` de
[`internal/api/service.go`](../internal/api/service.go). Es **compartido** por
las dos superficies (humana y MCP): lo único que varía es el **origen** del
token. Las seis etapas, en orden:

| # | Etapa | Qué hace | Dónde |
|---|-------|----------|-------|
| 0 | **Resolver SQL** | Determina el SQL efectivo según el modo de entrada (SQL crudo **o** operación acotada, exactamente uno). | `resolveSQL` |
| 1 | **Parseo** | Clasifica la sentencia con el **parser real del motor** (no regex) en un `guard.Statement` neutral. | `engine.Parse` |
| 2 | **Guardas de sentencia** | Reglas puras sobre la sentencia parseada: operación permitida, `WHERE` obligatorio en `UPDATE`/`DELETE`, `INSERT` sin `SELECT`, tabla en whitelist. | `guard.Check` |
| 3 | **Medición en transacción con `ROLLBACK`** | Ejecuta la sentencia en una transacción que **siempre revierte**: mide filas afectadas **sin persistir**. | `engine.Preview` |
| 4 | **Tope de filas** | Si el impacto medido supera `MAX_AFFECTED_ROWS`, aborta. | `guard.CheckAffectedRows` |
| 5 | **Token** | Guarda la sentencia validada en el store y emite un **token de un solo uso**, marcado con su **origen** (`ui` \| `mcp`). | `store.PutWithOrigin` |
| 6 | **Enriquecimiento IA** *(opcional, best-effort)* | Si se pidió, adjunta explicación + riesgo + flags. **Aislado**: un fallo devuelve `AI = nil` y no rompe el preview. | `enrichPreview` |

Al terminar el paso 5 ya hay un preview **válido y completo**
(`{ token, affected_rows, summary }`). El paso 6 es un extra que **no** afecta esa
ruta: pase lo que pase con la IA, el resto de la respuesta queda intacto.

### Confirm

El `confirm` toma el token, lo consume (un solo uso) y ejecuta la sentencia
guardada dentro de una transacción con **`COMMIT`** (`engine.Confirm` →
`execute`). Nunca recibe SQL.

Hay un **gating por origen** que se fuerza del lado servidor:

- Un token `origin = ui` se ejecuta por el **confirm humano directo** (`Confirm`).
- Un token `origin = mcp` **no** se ejecuta por esa vía: `Confirm` primero hace
  `Peek` (sin consumir) y, si es del agente, devuelve `ErrMCPRequiresApproval`
  (la capa HTTP lo mapea a `409`). Esos tokens solo se ejecutan vía **aprobación
  humana** (`Approve`). El detalle del human-in-the-loop está en la
  [capa MCP](mcp.md).

> Se hace `Peek` antes de consumir a propósito: consumir el token antes de
> chequear el origen permitiría *griefear* una propuesta pendiente mandándola a
> `/confirm`, dejándola sin poder aprobarse.

### Diagrama del flujo

```
                 ENTRADA (uno de dos modos)
        ┌───────────────────┐   ┌──────────────────────┐
        │  SQL crudo         │   │  Operación acotada    │
        │  { sql: "..." }    │   │  { operation: {...} } │
        └─────────┬─────────┘   └──────────┬───────────┘
                  └──────────┬─────────────┘
                             ▼
                    ┌──────────────────┐
                    │ 0. resolveSQL     │  (exactamente uno)
                    └────────┬─────────┘
                             ▼
   ┌─────────────────────── preview() ───────────────────────┐
   │  1. engine.Parse ....... parser real → guard.Statement   │
   │  2. guard.Check ........ operación / WHERE / whitelist    │
   │  3. engine.Preview ..... TX  ─►  EXEC  ─►  ROLLBACK       │
   │        (mide filas afectadas, NADA se persiste)           │
   │  4. guard.CheckAffectedRows .. tope MAX_AFFECTED_ROWS     │
   │  5. store.PutWithOrigin ...... token 1 solo uso + origen  │
   │  6. enrichPreview (IA) ....... opcional, aislado, best-eff│
   └────────────────────────────┬─────────────────────────────┘
                                ▼
                { token, affected_rows, summary, ai? }
                                │
              ┌─────────────────┴──────────────────┐
     origin=ui│                                     │origin=mcp
              ▼                                      ▼
   POST /confirm (humano)                RequestApproval → pending_approval
              │                                      │
       store.Take (consume)                   humano revisa /pending
              │                          ┌───────────┴───────────┐
              ▼                     Approve                    Reject
   engine.Confirm ─► TX ─► COMMIT   (consume) ─► COMMIT      (descarta)
              │                          │
              ▼                          ▼
     { affected_rows, summary }   { affected_rows, summary }
```

---

## Capas de seguridad

Estas son las salvaguardas que atraviesa toda escritura. El **detalle de qué
ataque mitiga cada una** vive en el [threat model](SECURITY.md); acá va el mapa
de dónde están en el código.

| Capa | Qué es | Paquete |
|------|--------|---------|
| **Parser real por motor** | Cada motor clasifica la sentencia con su **parser real** (no regex): Postgres vía `pg_query_go`, MySQL/MariaDB vía el parser de TiDB. Producen un `guard.Statement` neutral. | `internal/engine` |
| **Guardas puras** | `guard.Check` es una función **pura** sobre el `Statement` ya parseado: solo `UPDATE`/`DELETE`/`INSERT`, `WHERE` obligatorio en `UPDATE`/`DELETE`, `INSERT` sin `SELECT`, whitelist de tabla (exacta, case-sensitive). Sin base de datos: trivial de testear en tablas. | `internal/guard` |
| **Tope de filas en TX con `ROLLBACK`** | El impacto se mide ejecutando en una transacción que **siempre revierte** (`engine.Preview`). Si supera `MAX_AFFECTED_ROWS`, no se emite token. Es la red contra escrituras masivas aun con un `WHERE` trivial (`WHERE 1=1`). | `internal/engine` + `guard.CheckAffectedRows` |
| **Usuario restringido de la base** | La `DATABASE_URL` apunta a un usuario con grants mínimos (solo DML sobre las tablas de la whitelist, sin DDL). Es la red que nunca falla: aunque una guarda tuviera un bug, el motor niega la operación. | Operacional (config) |
| **Token de un solo uso** | El `confirm` solo acepta un token opaco (128 bits, `crypto/rand`), con TTL, consumido atómicamente. Nunca SQL. | `internal/store` |

El principio: **el parser es la primera línea, pero el usuario restringido es la
red que nunca falla.** Las guardas y el parser podrían tener un bug; el motor
sigue sin conceder permisos que el usuario no tiene.

### Modo de entrada y guardas

Los **dos modos de entrada** (ver más abajo) convergen en la **misma** barrera:

- El **SQL crudo** pasa por las guardas completas tal cual.
- La **operación acotada** se traduce a SQL parametrizado con identificadores
  citados (`engine.BuildSQL`) y **luego** pasa por exactamente las mismas
  guardas.

El SQL que propone la IA también llega a `preview()` como SQL crudo, **sin
distinción de origen**: pasa por la misma barrera que cualquier otro SQL humano.

---

## Dónde vive el estado

Entre el preview y el confirm, el servicio guarda la sentencia **ya validada**
(nunca vuelve a aceptar SQL en el confirm). Ese estado vive en el **store en
memoria** ([`internal/store/store.go`](../internal/store/store.go)): un `map`
protegido por un `sync.Mutex`.

Cada entrada tiene:

- **Token opaco de un solo uso** — 128 bits de `crypto/rand`, en hex. No
  adivinable, sin patrón secuencial.
- **TTL** — pasado el tiempo configurado, el token deja de ser válido (ni
  ejecutable ni aprobable). `GC()` limpia los expirados; `Take` / `Approve` /
  `Reject` también los descartan al toparse con ellos.
- **Un solo uso** — las transiciones terminales (`Take`, `Approve`, `Reject`)
  borran el token de forma atómica: no se puede confirmar dos veces la misma
  operación. El borrado ocurre **aunque la ejecución posterior falle**.
- **Origen** (`ui` \| `mcp`) — quién creó el preview; decide **cómo** puede
  ejecutarse.
- **Estado** — dónde está la entrada en su ciclo de vida.

### Máquina de estados

```
  previewed ─┬─ Take            (solo origin=ui) ───────────► executed
             └─ RequestApproval (solo origin=mcp) ─► pending_approval
  pending_approval ─┬─ Approve ─► executed
                    └─ Reject  ─► rejected

  executed / rejected / expired  = terminales
```

Un token expirado no es aprobable ni ejecutable. `Peek` mira una entrada **sin**
consumirla ni cambiar su estado (lo usa la superficie de aprobación para mostrar
el detalle antes de decidir, y el gating del confirm humano).

### Una sola instancia (deliberado para v1)

El estado vive **en el proceso**: no hay persistencia ni coordinación entre
réplicas. Es una decisión **deliberada** para v1, pensada para un despliegue de
**una sola instancia**:

- Un despliegue multi-réplica necesitaría un store compartido (fuera de alcance
  de v1).
- El **aislamiento cross-engine** cae de acá gratis: cada instancia corre **un
  solo motor** (el de su `DATABASE_URL`) y los tokens viven solo en su memoria.
  Un token de una instancia Postgres no existe en el map de una instancia MySQL.
  Ver la nota de aislamiento en el [threat model](SECURITY.md).

---

## Los dos modos de entrada

`POST /preview` (y la herramienta MCP `preview`) aceptan la escritura de **dos
formas mutuamente excluyentes** (exactamente una; si vienen ambas o ninguna, es
error — `ErrAmbiguousInput` / `ErrEmptyInput`):

| Modo | Campo | Qué es | Cómo llega al SQL |
|------|-------|--------|-------------------|
| **SQL crudo** | `sql` | La sentencia SQL tal cual la escribe el cliente. Máxima flexibilidad; soporta `UPDATE`, `DELETE` e `INSERT`. | Se usa tal cual y pasa por las guardas completas. |
| **Operación acotada** (`BoundedOp`) | `operation` | Una operación **estructurada** (`op`, `table`, `set`, `where`) que el servicio traduce a SQL. Solo `UPDATE` y `DELETE`; el `where` debe ser no vacío. | `engine.BuildSQL` arma SQL **parametrizado** con identificadores citados y valores como placeholders. |

La operación acotada es más segura de construir (nunca puede quedar sin `WHERE`,
y los valores viajan como parámetros), pero **ambos modos terminan en la misma
barrera de guardas y el mismo preview con `ROLLBACK`**. `BoundedOp` vive en
[`internal/engine/engine.go`](../internal/engine/engine.go).

> El campo `engine` del request se **ignora**: el motor es una propiedad del
> servidor (a qué base apunta `DATABASE_URL`), no algo que el cliente elija por
> request. La UI lo muestra como indicador read-only.

---

## Los extras opcionales (no tocan la ruta segura)

Dos capas se montan **encima** del core seguro sin reimplementar ni saltear
ninguna salvaguarda. Ambas degradan de forma limpia: si no están configuradas, el
resto del servicio queda idéntico.

### Capa MCP (agentes)

La [capa MCP](mcp.md) expone el core (`preview → confirm`) como un **servidor
MCP** para que un agente de IA pueda **proponer** escrituras, obligado a pasar por
las mismas guardas y el mismo preview que la superficie humana. La ejecución
sigue siendo **decisión de una persona**, y eso está **forzado a nivel
servidor**: la credencial del agente físicamente no puede ejecutar (los tokens
`origin = mcp` solo se ejecutan vía aprobación humana). Es un transporte fino:
**no** abre ningún atajo. Detalle completo en [`docs/mcp.md`](mcp.md).

### Capa de IA

La capa de IA es un **extra best-effort y aislado** que **solo propone; jamás
ejecuta ni llega al confirm**:

- **Enriquecimiento del preview** (paso 6): explicación del impacto + nivel de
  riesgo + flags. Es best-effort: si la IA está apagada, se pidió omitir
  (`ai: false`) o falla, el preview queda intacto (`AI = nil`).
- **NL → SQL** (`POST /ai/suggest`): a partir de una intención en lenguaje
  natural propone un **SQL candidato SIN validar**. **No toca la base** (salvo
  introspección de esquema de solo lectura, acotada a la whitelist). El candidato
  vuelve por `/preview` para pasar por las guardas antes de confirmar.

Cuando no hay `AI_API_KEY`, la capa es un cliente *disabled* (noop) y los
endpoints de IA degradan a `503` limpio. Detalle en [`docs/AI.md`](AI.md).

---

## Estructura de paquetes

Todo el código de aplicación vive bajo `internal/` (privado al módulo). La
separación sigue la dirección de las dependencias: el core seguro (`guard`,
`engine`, `store`) no conoce a las capas de arriba (`api`, `mcp`, `ai`, `ui`).

| Paquete | Responsabilidad |
|---------|-----------------|
| [`internal/guard`](../internal/guard) | Las **guardas puras**: reglas sobre un `Statement` ya parseado (operación, `WHERE`, `INSERT..SELECT`, whitelist) + tope de filas. Sin base de datos. |
| [`internal/engine`](../internal/engine) | Abstracción del motor (`Engine`): `Parse` (parser real), `BuildSQL` (operación acotada → SQL parametrizado), `Preview` (`ROLLBACK`), `Confirm` (`COMMIT`), introspección de esquema. Implementaciones: Postgres (pgx) y MySQL (database/sql). |
| [`internal/store`](../internal/store) | **Estado en memoria** del flujo: tokens con TTL, un solo uso, origen y máquina de estados. |
| [`internal/api`](../internal/api) | **Capa HTTP** y orquestación: `Service` (el flujo preview → confirm, gating por origen, aprobaciones) + `router.go` (rutas chi, contrato JSON, mapeo de errores → códigos). |
| [`internal/mcp`](../internal/mcp) | Servidor **MCP** (Streamable HTTP): expone `preview` y **solicitar** confirmación como herramientas. Transporte fino sobre `Service`. |
| [`internal/ai`](../internal/ai) | Capa de **IA** (opcional): cliente Anthropic + cliente *disabled* (noop) + prompts. Solo propone. |
| [`internal/config`](../internal/config) | Carga y valida la **configuración** desde el entorno (`DATABASE_URL`, `DEITAFIX_ENGINE`, `DEITAFIX_ENABLED`, `MAX_AFFECTED_ROWS`, whitelist, MCP, IA…). Falla rápido si falta lo obligatorio. |
| [`internal/ui`](../internal/ui) | **UI web** embebida (Alpine.js): la página de preview/confirm y la de aprobaciones pendientes. Recibe el motor real como indicador read-only. |

### Rutas (superficie HTTP)

Registradas en [`internal/api/router.go`](../internal/api/router.go):

| Ruta | Superficie | Feature flag `DEITAFIX_ENABLED` |
|------|-----------|-------------------------------|
| `GET /healthz`, `GET /readyz` | Probes (liveness / readiness) | No (siempre responden) |
| `/mcp` | MCP (agente), bearer `MCP_AUTH_TOKEN` | Sí |
| `POST /preview`, `POST /confirm` | Humana (UI / API) | Sí |
| `POST /ai/suggest` | Humana (IA, opcional) | Sí |
| `GET /pending`, `POST /pending/{token}/approve`, `POST /pending/{token}/reject` | Aprobación humana de propuestas del agente | Sí |
| `/*` (catch-all) | UI web embebida | No (carga igual, refleja el estado) |

La superficie humana se protege **opcionalmente** con `UI_AUTH_TOKEN` (defensa en
profundidad: la credencial MCP no la alcanza). Cuando el feature flag está
apagado, las rutas de escritura responden `503` sin tocar la base.

---

## Referencias

- [Modelo de amenazas (threat model)](SECURITY.md) — qué ataque mitiga cada capa
  y las decisiones de política sobre casos ambiguos.
- [Capa MCP](mcp.md) — cómo un agente propone escrituras con human-in-the-loop
  forzado a nivel servidor.
- [Capa de IA](AI.md) — enriquecimiento del preview y NL → SQL, siempre como
  propuesta.
