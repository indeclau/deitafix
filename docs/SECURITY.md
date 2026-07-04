# Modelo de amenazas (threat model)

Deitafix ejecuta escrituras ocasionales (`UPDATE` / `DELETE` / `INSERT`) sobre una
base de producción con un flujo **preview → confirm**: nada se ejecuta a ciegas.
Este documento describe **qué ataques mitiga cada capa** de defensa y qué
decisiones de política se tomaron para los casos ambiguos.

La política de **reporte de vulnerabilidades** vive en
[`SECURITY.md`](../SECURITY.md) (raíz del repo, para que GitHub la reconozca).
Este documento es el detalle técnico del modelo de amenazas.

---

## Principio de diseño: defensa en profundidad

Ninguna garantía descansa en una sola capa. Un atacante que vulnere una barrera
todavía choca con la siguiente. Las capas, de afuera hacia adentro:

| # | Capa | Qué es | Qué ataque mitiga |
|---|------|--------|-------------------|
| 1 | **Guardas de sentencia** | Parser real del motor + reglas puras (`internal/guard`, `internal/engine/*_parse.go`) | SQL peligroso o fuera de alcance: `UPDATE`/`DELETE` sin `WHERE`, multi-statement, DDL/`DROP`/`TRUNCATE`, `INSERT ... SELECT`, tabla fuera de la whitelist. |
| 2 | **Tope de filas** (`MAX_AFFECTED_ROWS`) | Medición del impacto en una transacción con `ROLLBACK`, antes de confirmar | Escrituras masivas accidentales o maliciosas, incluso con un `WHERE` sintácticamente válido pero trivial (`WHERE 1=1`). |
| 3 | **Usuario restringido de la base** | La `DATABASE_URL` apunta a un usuario con grants mínimos (solo `SELECT/INSERT/UPDATE/DELETE` sobre las tablas de la whitelist; sin DDL) | Cualquier operación que se saltara las capas 1–2: el motor mismo niega DDL, `TRUNCATE`, o el acceso a tablas no autorizadas (`permission denied`). |
| 4 | **Flujo preview → confirm con token** | El `confirm` solo acepta un **token** de un solo uso, nunca SQL; la ejecución la dispara un humano | Ejecución a ciegas, replay, y que un agente de IA ejecute por su cuenta (human-in-the-loop forzado a nivel servidor). |

El principio rector: **el parser es la primera línea, pero el usuario restringido
es la red que nunca falla.** Aunque una guarda tuviera un bug, el motor sigue sin
conceder permisos que el usuario no tiene.

---

## Guardas de sentencia (capa 1)

Las guardas **nunca** usan regex sobre el SQL. Usan el **parser real de cada
motor**:

- **PostgreSQL**: `libpg_query` vía `pg_query_go` (el mismo parser del servidor).
- **MySQL/MariaDB**: el parser de TiDB (`github.com/pingcap/tidb/pkg/parser`), Go
  puro.

Cada parser destila la sentencia a un `guard.Statement` neutral (operación, tabla
objetivo, si tiene `WHERE`, si el `INSERT` viene de un `SELECT`). El checker
(`guard.Check`) aplica reglas puras sobre esa estructura. Consecuencias:

- **`UPDATE` / `DELETE` sin `WHERE` → rechazado.** El parser ve la ausencia real
  de la cláusula. Un `WHERE` **comentado** (`-- WHERE ...`, `/* ... */`) no cuenta:
  el parser descarta los comentarios, así que la sentencia sigue "sin `WHERE`" y se
  rechaza.
- **Multi-statement (stacking) → rechazado.** El parser cuenta las sentencias del
  árbol; más de una es un rechazo inmediato (`ErrMultipleStatements`). Inmune a
  intentos de esconder el `;` tras un comentario.
- **DDL / `DROP` / `TRUNCATE` / `SELECT` → rechazado por operación no soportada.**
  Solo `UPDATE` / `DELETE` / `INSERT` están permitidos.
- **`INSERT` solo con `VALUES` explícitos.** `INSERT ... SELECT` se rechaza (fuera
  de alcance de v1).
- **Tabla fuera de la whitelist → rechazado.** Comparación exacta y
  **case-sensitive** contra la whitelist configurada.

Cada uno de estos casos tiene un test que lo demuestra contra el parser real de
**ambos** motores (`internal/engine/security_parse_test.go`).

### Nota de casing: cómo cada motor normaliza los identificadores

La whitelist se compara de forma exacta, así que hay que escribir el nombre de la
tabla **tal como el motor lo normaliza**:

- **PostgreSQL** *foldea* los identificadores **sin comillas** a minúscula
  (`UPDATE CollectionBox` → tabla `collectionbox`). Con comillas dobles preserva el
  casing (`"CollectionBox"` → `CollectionBox`).
- **MySQL/MariaDB** preserva el casing tal como se escribe.

Esto está fijado por tests (`TestPostgresFoldsUnquotedIdentifiers`,
`TestMySQLPreservesIdentifierCasing`) y documentado en
[`docs/RESTRICTED-USER.md`](RESTRICTED-USER.md). Una whitelist mal escrita
respecto del casing no es un agujero de seguridad (falla cerrada: rechaza), pero sí
puede rechazar operaciones legítimas.

---

## Decisiones de política (casos ambiguos)

Estos son SQL válidos que el parser acepta y sobre los que se tomó una decisión
**explícita** para v1. Quedan como contrato estable.

### `WHERE 1=1` / `WHERE true` — **permitido**, la red es el tope de filas

El parser ve que **sí hay** un `WHERE`, así que la guarda de sintaxis pasa. **No**
intentamos detectar tautologías por sintaxis: es un juego perdido (`1=1`, `2>1`,
`true`, `id=id`, `'a'='a'`, … hay infinitas formas). La salvaguarda real es el
**tope de filas** (`MAX_AFFECTED_ROWS`): un `WHERE` trivial que afecte más filas
que el tope hace **abortar el preview**, sin dejar rastro (la medición ocurre en
una transacción con `ROLLBACK`).

### Subqueries en `WHERE` — **permitidas**

`DELETE FROM t WHERE id IN (SELECT ...)` es un `WHERE` legítimo. Se permite: el
tope de filas y el usuario restringido acotan el daño, y la subquery solo puede
**leer** tablas que el usuario restringido ya está autorizado a ver.

### CTEs (`WITH ... UPDATE`) — **permitidas**, con la tabla objetivo correcta

En un `WITH x AS (...) UPDATE t SET ...`, la tabla objetivo es la del `UPDATE`
(`t`), **no** la del CTE. El parser extrae correctamente `t`, y el checker la valida
contra la whitelist. El CTE auxiliar solo lee. Verificado por test en ambos
motores: un `WITH ... UPDATE tabla_whitelisteada` se acepta, y la tabla que se
valida es siempre la del `UPDATE`. Un `WITH ... SELECT` (sin escritura de
top-level) cae en "operación no soportada" y se rechaza.

### Aislamiento entre motores (cross-engine) — **por arquitectura**

Un token generado contra un motor no puede usarse contra otro. Esto se garantiza
por **arquitectura**, no por un campo extra: cada instancia del servicio corre
**un solo motor** (el de su `DATABASE_URL`), y el estado de tokens es **en memoria
del proceso**. Un token de una instancia Postgres simplemente no existe en el mapa
de una instancia MySQL. No hay superficie multi-motor por request.

---

## Tope de filas (capa 2)

El impacto se mide ejecutando la sentencia dentro de una **transacción que siempre
hace `ROLLBACK`** (`engine.Preview`): se obtiene el número real de filas afectadas
**sin persistir nada**. Si supera `MAX_AFFECTED_ROWS`, el preview aborta con
`ErrRowsExceeded` y no se emite token. Es la defensa contra escrituras masivas,
incluso cuando el `WHERE` es sintácticamente válido.

---

## Usuario restringido (capa 3)

La `DATABASE_URL` **debe** apuntar a un usuario con grants mínimos: solo
`SELECT / INSERT / UPDATE / DELETE` sobre las tablas de la whitelist, **sin** DDL,
sin acceso al resto del esquema. Ver [`docs/RESTRICTED-USER.md`](RESTRICTED-USER.md)
para el setup paso a paso en ambos motores.

Esta capa es la que convierte cada guarda en **defensa en profundidad**: aunque una
guarda fallara, el motor niega la operación. Verificado por integración contra
bases reales (testcontainers) en ambos motores:

- Una escritura sobre una tabla fuera de la whitelist devuelve `permission denied`
  del motor, no solo el rechazo de la guarda.
- Un `DROP TABLE` / `TRUNCATE` ejecutado directamente con el usuario restringido
  (saltándose la guarda) es **negado por el motor**.

---

## Flujo preview → confirm y tokens (capa 4)

- **`confirm` solo acepta un token, nunca SQL.** El cuerpo de `POST /confirm` tiene
  un único campo (`token`); el decoder rechaza cualquier campo desconocido
  (`DisallowUnknownFields`), así que un intento de colar SQL en el `confirm` se
  rechaza con `400` **sin procesarse**. Se ejecuta exactamente lo previsualizado (el
  SQL quedó guardado del lado servidor, asociado al token).
- **Tokens de un solo uso.** Se generan con `crypto/rand` (128 bits, hex). Se
  consumen de forma atómica: reusar un token da `ErrNotFound`.
- **TTL.** Cada token expira tras un tiempo configurable; un token expirado no es
  ejecutable ni aprobable.
- **No adivinables.** 128 bits de entropía criptográfica; sin patrón secuencial.
  Verificado por test (`TestTokenEntropyAndFormat`).
- **Human-in-the-loop forzado a nivel servidor.** Un preview originado por un agente
  (MCP) **no** puede ejecutarse con la credencial del agente: queda a la espera de
  una aprobación humana explícita. La credencial MCP físicamente no tiene el camino
  a la ejecución. Ver [`docs/mcp.md`](mcp.md).

---

## Qué NO está en alcance (v1)

- **No** hay detección de tautologías en el `WHERE` (por diseño; ver política).
- **No** se soporta `INSERT ... SELECT` ni operaciones DDL (por diseño).
- El estado de tokens es **en memoria**, sin persistencia ni coordinación entre
  réplicas: pensado para un despliegue de una sola instancia. Un despliegue
  multi-réplica necesitaría un store compartido (fuera de alcance de v1).
