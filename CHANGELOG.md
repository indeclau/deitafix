# Changelog

Todos los cambios notables de Deitafix se documentan en este archivo.

El formato sigue [Keep a Changelog](https://keepachangelog.com/es-ES/1.1.0/) y el
proyecto adhiere a [Versionado Semántico](https://semver.org/lang/es/).

## [1.0.0] - 2026-07-04

Primer release estable. Consolida el core seguro, el despliegue, la UI, la capa
MCP y las features de IA, con un ciclo de **endurecimiento**: revisión de
seguridad de las guardas, cobertura de tests sólida y documentación completa. A
partir de este tag, los identificadores públicos (variables de entorno, contrato
de la API) son **estables**.

### ⚠️ BREAKING CHANGES

Se normalizaron los identificadores al nombre del proyecto (`deitafix`) **antes**
de que 1.0 los volviera contrato estable. Si venís de una versión previa, al
actualizar tenés que ajustar:

- **Variable de entorno**: `DATAFIX_ENABLED` → **`DEITAFIX_ENABLED`**. El nombre
  viejo ya no se lee; si no la renombrás, el servicio queda apagado (default
  seguro).
- **Usuario de base de ejemplo**: `prod_datafix` → **`prod_deitafix`** (y la
  password de desarrollo `dev_datafix_pw` → **`dev_deitafix_pw`**). Afecta los
  seeds, `docker-compose.yml`, los manifiestos K8s y tu `DATABASE_URL`. Si ya
  tenés un usuario `prod_datafix` creado, podés mantenerlo y solo apuntar la
  `DATABASE_URL` a él, o recrearlo con el nombre nuevo.

No cambió el contrato de la API ni el comportamiento: es puramente un renombrado
de identificadores para que 1.0 salga coherente.

### Added

- **Modelo de amenazas documentado** ([`docs/SECURITY.md`](docs/SECURITY.md)):
  qué ataque mitiga cada capa (guardas, tope de filas, usuario restringido,
  token) y las decisiones de política explícitas.
- **Política de seguridad** ([`SECURITY.md`](SECURITY.md)) para el reporte
  responsable de vulnerabilidades vía GitHub Security Advisories.
- **Documentación completa**: [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md),
  [`docs/RESTRICTED-USER.md`](docs/RESTRICTED-USER.md),
  [`docs/API.md`](docs/API.md), [`docs/AI.md`](docs/AI.md) y
  [`docs/mcp.md`](docs/mcp.md).
- **Reporte de cobertura en CI**: cada motor mide con `-coverpkg=./internal/...`
  y un job combina ambos perfiles (postgres + mysql), publica el desglose y falla
  si el core baja del 80%.
- **Tests que demuestran cada bypass de seguridad** contra los parsers reales de
  ambos motores: `UPDATE`/`DELETE` sin `WHERE`, comentarios que ocultan la
  intención, multi-statement, DDL/`DROP`/`TRUNCATE`, `INSERT ... SELECT`,
  `WHERE 1=1`, subqueries y CTEs. Defensa en profundidad DDL verificada contra
  bases reales.

### Changed

- **Licencia migrada a Apache 2.0** (antes por definir): texto íntegro en
  [`LICENSE`](LICENSE), archivo [`NOTICE`](NOTICE), y header de licencia en cada
  archivo `.go`. Las contribuciones se aceptan bajo el [CLA](CLA.md).
- Cobertura del core (`internal/...`) elevada por encima del 80% (guardas al
  100%).

### Security

- Se fijó con tests la **política para los casos ambiguos**: `WHERE 1=1` se
  permite y la red es el tope de filas; las subqueries en `WHERE` se permiten; en
  un CTE (`WITH ... UPDATE`) la tabla objetivo validada es la del `UPDATE`, no la
  del CTE.
- Se documentó el **folding de identificadores** por motor (Postgres normaliza a
  minúscula los nombres sin comillas; MySQL preserva el casing), relevante para
  configurar la whitelist correctamente.

## [0.5.0] - 2026-07-03

Capa de IA opcional. La IA **solo propone, nunca ejecuta**: todo SQL que sugiere
pasa por las mismas guardas y el mismo `preview → confirm`.

### Added

- **NL → SQL** (`POST /ai/suggest`): describís la intención en lenguaje natural y
  la IA propone la sentencia candidata, que vuelve por `/preview`.
- **Explicación de impacto** en `/preview`: traduce "N filas afectadas" a lenguaje
  claro más una señal de riesgo (`low` / `medium` / `high`).
- **Revisor IA**: marca patrones sospechosos en el preview (informa, no bloquea).
- **Degradación limpia** sin `AI_API_KEY`: `/ai/suggest` responde `503`,
  `/preview` devuelve `"ai": null`, y el resto del servicio funciona idéntico.
- Introspección de esquema acotada a la whitelist (con cache TTL) para el NL → SQL.

## [0.4.0] - 2026-07-03

Capa MCP: un agente de IA puede **proponer** escrituras, obligado a pasar por las
mismas guardas y el mismo preview que la superficie humana.

### Added

- Servidor **MCP** que expone `preview` y `confirm` como herramientas.
- **Human-in-the-loop forzado a nivel servidor**: la credencial del agente no
  puede ejecutar; el `confirm` sigue siendo una decisión humana (gating por origen
  del token).
- Superficie de aprobación humana: `GET /pending`, `POST /pending/{token}/approve`,
  `POST /pending/{token}/reject`, más la UI de "Aprobaciones pendientes".
- Documentación de conexión MCP ([`docs/mcp.md`](docs/mcp.md)).

## [0.3.0] - 2026-07-03

UI web mobile-first, para el caso de emergencia desde el celular.

### Added

- Frontend Alpine.js + CSS mobile-first, embebido en el binario con `embed`.
- Flujo de dos pantallas: entrada (SQL crudo / operación acotada) → preview de
  impacto → confirmación.
- Servida por el mismo binario que la API.

## [0.2.0] - 2026-07-03

Despliegue: un binario único, contenedor e infraestructura.

### Added

- **Dockerfile** multi-stage → imagen `distroless` (binario único que sirve la API).
- Workflow de **release**: build y publicación de la imagen a GHCR al taggear una
  versión (`vX.Y.Z`).
- Manifiestos de **Kubernetes** (Deployment + Service + Secret para `DATABASE_URL`).
- Endpoints de health: `GET /healthz` (liveness) y `GET /readyz` (readiness).

## [0.1.0] - 2026-07-03

Core seguro: el flujo `preview → confirm` contra los dos motores.

### Added

- Interfaz `Engine` con dos implementaciones: **PostgreSQL** (pgx) y
  **MySQL/MariaDB** (go-sql-driver).
- **Guardas con parser real** (`pg_query_go` para Postgres, parser de TiDB para
  MySQL): rechazo de `UPDATE`/`DELETE` sin `WHERE`, whitelist de operaciones y de
  tablas, y tope de filas.
- `POST /preview`: parseo, validación, medición del impacto en una transacción con
  `ROLLBACK`, y token en memoria con TTL.
- `POST /confirm`: acepta **solo el token**, ejecuta con `COMMIT`, y el token es de
  un solo uso.
- Dos modos de entrada: **SQL crudo** y **operación acotada**.
- Feature flag (`DATAFIX_ENABLED`) y configuración por entorno.
- Tests unitarios (guardas, table-driven) e integración con testcontainers,
  incluido *permission denied* fuera de la whitelist.

[1.0.0]: https://github.com/indeclau/deitafix/releases/tag/v1.0.0
[0.5.0]: https://github.com/indeclau/deitafix/releases/tag/v0.5.0
[0.4.0]: https://github.com/indeclau/deitafix/releases/tag/v0.4.0
[0.3.0]: https://github.com/indeclau/deitafix/releases/tag/v0.3.0
[0.2.0]: https://github.com/indeclau/deitafix/releases/tag/v0.2.0
[0.1.0]: https://github.com/indeclau/deitafix/releases/tag/v0.1.0
