# Usuario restringido de la base

Esta es la guía de setup del **usuario restringido** de la base: la credencial con
la que el servicio Deitafix se conecta a producción vía `DATABASE_URL`. Es la
**capa 3** del [modelo de amenazas](SECURITY.md#usuario-restringido-capa-3): la red
que nunca falla.

El principio es simple: **la seguridad no vive en el código, vive en la base.**
Aunque una guarda del parser tuviera un bug, aunque el tope de filas fallara, el
motor mismo tiene que negar cualquier operación que este usuario no esté explícita
y mínimamente autorizado a hacer.

---

## Por qué existe (defensa en profundidad)

Deitafix apila varias barreras (ver [`SECURITY.md`](SECURITY.md) para el detalle).
Las guardas de sentencia (capa 1) y el tope de filas (capa 2) viven **en el código
del servicio**. El usuario restringido vive **en el motor de la base**, un dominio
distinto. Esa separación es la que lo hace valioso:

- **Si una guarda fallara** (un bug en el parser, un caso no contemplado), el motor
  sigue sin conceder permisos que el usuario no tiene. Un `DROP TABLE` que se
  escapara de la guarda choca contra un `permission denied` del motor.
- **Falla cerrada.** El usuario solo tiene `SELECT/INSERT/UPDATE/DELETE` sobre las
  tablas de la whitelist. Todo lo demás —DDL, `TRUNCATE`, cualquier tabla no
  autorizada— está negado por ausencia de grant, no por una regla que haya que
  acordarse de escribir.
- **Whitelist, nunca blacklist.** Se nombran una por una las tablas que se pueden
  tocar. Una tabla nueva no es alcanzable hasta que alguien le otorgue el grant a
  mano. No hay "todo menos X".

> 🔒 **No negociable:** la `DATABASE_URL` del servicio **nunca** debe apuntar al
> superusuario ni al owner de las tablas. Ver la [nota de seguridad](#nota-de-seguridad-nunca-uses-el-superusuarioowner)
> al final.

Este documento usa los nombres del setup real del repo
([`seed/postgres/01-init.sql`](../seed/postgres/01-init.sql) y
[`seed/mysql/01-init.sql`](../seed/mysql/01-init.sql)):

| Cosa | Valor |
|---|---|
| Usuario restringido | `prod_deitafix` |
| Password | *(placeholder)* `CAMBIAR` — **poné una password fuerte real** |
| Tabla de la whitelist | `CollectionBox` |
| Base | `deitafix_dev` |

> En los ejemplos, la password aparece como `CAMBIAR`. **Nunca** commitees una
> credencial real: en producción va por secret (variable de entorno, `Secret` de
> Kubernetes, vault, lo que uses), no en un archivo versionado.

---

## PostgreSQL

El setup completo, tal como corre el seed, tiene cinco movimientos: crear el
usuario, sacarle todo, darle `USAGE` del schema, y otorgarle datos **solo** sobre
las tablas de la whitelist (más la sequence de cada tabla con `SERIAL`).

```sql
-- 1. Crear el usuario restringido con una password fuerte.
CREATE USER prod_deitafix WITH PASSWORD 'CAMBIAR';

-- 2. Sacarle TODO por defecto: ninguna tabla, ni el schema.
REVOKE ALL ON ALL TABLES IN SCHEMA public FROM prod_deitafix;
REVOKE ALL ON SCHEMA public FROM prod_deitafix;

-- 3. Puede "ver" el schema para resolver nombres de tabla (pero nada más).
GRANT USAGE ON SCHEMA public TO prod_deitafix;

-- 4. Whitelist EXPLÍCITA: solo esta tabla, solo datos. Sin DDL, sin DROP/TRUNCATE.
GRANT SELECT, INSERT, UPDATE, DELETE ON "CollectionBox" TO prod_deitafix;

-- 5. INSERT sobre una tabla con SERIAL necesita la secuencia del id autoincremental.
GRANT USAGE, SELECT ON SEQUENCE "CollectionBox_id_seq" TO prod_deitafix;
```

Punto por punto:

- **`REVOKE ALL ON ALL TABLES` + `REVOKE ALL ON SCHEMA public`.** Postgres es
  generoso por defecto con el schema `public`. Estos dos `REVOKE` dejan al usuario
  en cero: no puede tocar ninguna tabla ni crear objetos nuevos en el schema.
- **`GRANT USAGE ON SCHEMA public`.** Sin esto, el usuario ni siquiera puede
  *resolver* el nombre de una tabla del schema (falla antes de llegar a chequear el
  grant de la tabla). `USAGE` solo lo deja "entrar" al schema; no le da acceso a
  ninguna tabla por sí solo.
- **`GRANT SELECT, INSERT, UPDATE, DELETE ON "CollectionBox"`.** Solo estas cuatro
  operaciones de datos, solo sobre esta tabla. Sin `TRUNCATE`, sin `ALTER`, sin
  `DROP` (esos son privilegios distintos que no se otorgan).
- **`GRANT USAGE, SELECT ON SEQUENCE "CollectionBox_id_seq"`.** Cuando la tabla
  tiene una columna `SERIAL` (como `id SERIAL PRIMARY KEY`), Postgres crea una
  *sequence* auxiliar llamada `<tabla>_id_seq`. Un `INSERT` que dependa del id
  autoincremental necesita `USAGE` sobre esa sequence, o falla con `permission
  denied for sequence`. Si tu tabla no tiene columnas `SERIAL`/autoincrement, este
  paso no hace falta.

Para **cada tabla adicional** de la whitelist, repetí los pasos 4 y 5 (y el 5 solo
si esa tabla tiene una columna `SERIAL`):

```sql
GRANT SELECT, INSERT, UPDATE, DELETE ON "OtraTabla" TO prod_deitafix;
GRANT USAGE, SELECT ON SEQUENCE "OtraTabla_id_seq" TO prod_deitafix;
```

### El detalle del casing en Postgres

Este es el punto más fácil de arruinar. **Postgres foldea (baja a minúscula) los
identificadores sin comillas.**

- Si creás la tabla **sin comillas** (`CREATE TABLE CollectionBox (...)`), Postgres
  la guarda como `collectionbox`. A partir de ahí, `UPDATE CollectionBox` y
  `UPDATE collectionbox` son lo mismo, y el nombre real es `collectionbox`.
- Si la creás **con comillas dobles** (`CREATE TABLE "CollectionBox" (...)`, como en
  el seed de este repo), Postgres **preserva** el casing: la tabla se llama
  literalmente `CollectionBox`, y para referirte a ella **siempre** tenés que usar
  comillas: `UPDATE "CollectionBox"`.

Consecuencia práctica: **el nombre en el `GRANT`, en la `TABLE_WHITELIST` del
servicio y en el SQL que escribís tienen que coincidir con lo que el motor guardó.**

En este repo la tabla se creó con comillas (`"CollectionBox"`), así que:

```sql
-- ✅ Correcto: comillas dobles, casing exacto.
GRANT SELECT, INSERT, UPDATE, DELETE ON "CollectionBox" TO prod_deitafix;

-- ❌ Mal: sin comillas, Postgres busca "collectionbox" (minúscula) y no existe.
GRANT SELECT, INSERT, UPDATE, DELETE ON CollectionBox TO prod_deitafix;
```

Y en la configuración del servicio, la whitelist se compara **exacta y
case-sensitive** contra el nombre normalizado, así que también va con el casing real:

```powershell
-e TABLE_WHITELIST="CollectionBox"
```

> Una whitelist mal escrita respecto del casing **no es un agujero de seguridad**
> (falla cerrada: el servicio rechaza la operación), pero sí puede rechazar
> operaciones legítimas. Ver la
> [nota de casing en `SECURITY.md`](SECURITY.md#nota-de-casing-cómo-cada-motor-normaliza-los-identificadores).

---

## MySQL / MariaDB

En MySQL/MariaDB el setup es más corto: no hay que hacer `REVOKE` explícito (un
`CREATE USER` nace sin privilegios sobre las bases) ni hay sequences (el
`AUTO_INCREMENT` no requiere ningún grant extra). Alcanza con crear el usuario y
otorgarle datos sobre cada tabla de la whitelist, calificando por base.

```sql
-- 1. Crear el usuario restringido. '%' = puede conectarse desde cualquier host;
--    ajustá el host si tu red lo requiere (por ejemplo 'prod_deitafix'@'10.0.%').
CREATE USER 'prod_deitafix'@'%' IDENTIFIED BY 'CAMBIAR';

-- 2. Whitelist EXPLÍCITA: solo esta tabla, solo datos. Sin DDL, sin DROP/TRUNCATE.
GRANT SELECT, INSERT, UPDATE, DELETE ON deitafix_dev.CollectionBox TO 'prod_deitafix'@'%';

-- 3. Aplicar los cambios de privilegios.
FLUSH PRIVILEGES;
```

Punto por punto:

- **`CREATE USER 'prod_deitafix'@'%'`.** El `@'%'` es el host desde el que el usuario
  puede conectarse (`%` = cualquiera). Si tu servicio corre en una red conocida,
  acotalo (por ejemplo `'prod_deitafix'@'10.0.%'`) para reducir superficie.
- **`GRANT ... ON deitafix_dev.CollectionBox`.** El grant se califica como
  `base.tabla`. Solo `SELECT/INSERT/UPDATE/DELETE`, solo sobre esa tabla de esa
  base. **No** uses `ON deitafix_dev.*` (eso otorgaría sobre *todas* las tablas de
  la base, rompiendo la whitelist).
- **`FLUSH PRIVILEGES`.** Recarga las tablas de privilegios para que los cambios
  tomen efecto de inmediato.

Para **cada tabla adicional** de la whitelist, repetí el `GRANT`:

```sql
GRANT SELECT, INSERT, UPDATE, DELETE ON deitafix_dev.OtraTabla TO 'prod_deitafix'@'%';
```

### Casing en MySQL/MariaDB

**MySQL/MariaDB preserva el casing tal como se escribe** el nombre de la tabla (no
foldea como Postgres). Escribí `CollectionBox` en el `GRANT`, en el SQL y en la
`TABLE_WHITELIST` con exactamente el mismo casing con que se creó la tabla, y todo
cierra. Sin comillas de por medio.

> Nota: en algunas plataformas la sensibilidad al caso de los **nombres de tabla**
> depende de `lower_case_table_names` del servidor. El default varía según el SO. Si
> tenés dudas, mantené un único casing consistente en el `CREATE TABLE`, el `GRANT`
> y la whitelist, y no dependas de que dos casings distintos "sean iguales".

---

## Verificar que el usuario NO puede hacer DDL

Este es el chequeo que confirma que la capa 3 está de verdad puesta. Conectate con
la credencial **restringida** (no la de admin) y probá una operación que **debe
fallar**. Si el motor la niega, el usuario está bien configurado.

Esto es exactamente lo que verifican los tests de integración del repo contra bases
reales ([`internal/api/integration_test.go`](../internal/api/integration_test.go),
subtest *"defensa en profundidad: el usuario restringido no puede hacer DDL"*).

### PostgreSQL

```powershell
# Conectate con el usuario restringido (te va a pedir la password 'CAMBIAR').
psql "postgres://prod_deitafix@host:5432/deitafix_dev"
```

```sql
-- DDL: el motor debe negarlo.
DROP TABLE "CollectionBox";
-- ERROR:  must be owner of table CollectionBox

TRUNCATE TABLE "CollectionBox";
-- ERROR:  permission denied for table CollectionBox

-- Tabla FUERA de la whitelist: sin grant, el motor la niega.
UPDATE "AuditSensitive" SET note = 'x' WHERE id = 1;
-- ERROR:  permission denied for table AuditSensitive

-- Operación permitida: esta SÍ tiene que funcionar (afecta filas, con WHERE).
UPDATE "CollectionBox" SET status = 1 WHERE id = 1;
-- UPDATE 1
```

### MySQL / MariaDB

```powershell
# Conectate con el usuario restringido.
mysql -h host -u prod_deitafix -p deitafix_dev
```

```sql
-- DDL: el motor debe negarlo.
DROP TABLE CollectionBox;
-- ERROR 1142 (42000): DROP command denied to user 'prod_deitafix'@'...' for table 'CollectionBox'

TRUNCATE TABLE CollectionBox;
-- ERROR 1142 (42000): DROP command denied to user 'prod_deitafix'@'...' for table 'CollectionBox'

-- Tabla FUERA de la whitelist: sin grant, el motor la niega.
UPDATE AuditSensitive SET note = 'x' WHERE id = 1;
-- ERROR 1142 (42000): UPDATE command denied to user 'prod_deitafix'@'...' for table 'AuditSensitive'

-- Operación permitida: esta SÍ tiene que funcionar.
UPDATE CollectionBox SET status = 1 WHERE id = 1;
-- Query OK, 1 row affected
```

> El texto exacto del error varía por versión y motor (`permission denied`,
> `must be owner`, `command denied`, `insufficient privilege`), pero el resultado es
> el mismo: **la operación no ocurre.** Lo que importa es que el `DROP`/`TRUNCATE`
> falle y que la tabla siga existiendo intacta después.

---

## Construir la `DATABASE_URL`

El servicio lee la conexión de la variable de entorno `DATABASE_URL`, que **debe**
usar el usuario restringido. El formato depende del motor.

### PostgreSQL

```
postgres://<usuario>:<password>@<host>:<puerto>/<base>
```

Con los valores de esta guía:

```powershell
$env:DATABASE_URL = "postgres://prod_deitafix:CAMBIAR@host:5432/deitafix_dev"
```

### MySQL / MariaDB

```
mysql://<usuario>:<password>@<host>:<puerto>/<base>
```

```powershell
$env:DATABASE_URL = "mysql://prod_deitafix:CAMBIAR@host:3306/deitafix_dev"
```

> Si la password tiene caracteres especiales (`@`, `:`, `/`, `#`, etc.), hay que
> URL-encodearlos, o el parseo de la URL se rompe. Una password sin esos caracteres
> te evita el problema.

Ejemplo completo pasándola al container (PowerShell, consistente con el
[README](../README.md#quickstart-docker)):

```powershell
docker run --rm `
  -p 8080:8080 `
  -e DATABASE_URL="postgres://prod_deitafix:CAMBIAR@host:5432/deitafix_dev" `
  -e DEITAFIX_ENGINE="postgres" `
  -e DEITAFIX_ENABLED="true" `
  -e MAX_AFFECTED_ROWS="50" `
  -e TABLE_WHITELIST="CollectionBox" `
  ghcr.io/indeclau/deitafix:latest
```

El motor se infiere del esquema de la URL (`postgres://` → Postgres, `mysql://` →
MySQL), o podés fijarlo explícito con `DEITAFIX_ENGINE`.

---

## Nota de seguridad: nunca uses el superusuario/owner

La `DATABASE_URL` del servicio **nunca** debe apuntar a:

- el **superusuario** de la base (`postgres`, `root`, etc.),
- el **owner de las tablas** (en este repo, el usuario `app`, dueño del schema),
- ni ningún usuario con más permisos que los cuatro grants de datos de la whitelist.

Si el servicio corriera con el superusuario o el owner, **toda la capa 3 se
evapora**: un `DROP TABLE` que se escapara de las guardas ya no chocaría con ningún
`permission denied` — el motor lo ejecutaría porque el usuario *sí* puede. La
defensa en profundidad se cae y queda todo el peso sobre el parser, que es
justamente lo que esta capa está para respaldar.

La regla operativa:

- **Un usuario para crear/migrar el schema** (owner o admin): se usa una vez, en el
  setup y las migraciones. **Nunca** es la `DATABASE_URL` del servicio corriendo.
- **Un usuario restringido** (`prod_deitafix`): el único que va en la `DATABASE_URL`
  del servicio en producción.

Y, otra vez: la password va por **secret**, nunca hardcodeada en un archivo
versionado ni en la imagen.

---

## Ver también

- [`SECURITY.md`](SECURITY.md) — el modelo de amenazas completo; esta guía es el
  setup de la [capa 3](SECURITY.md#usuario-restringido-capa-3).
- [`seed/postgres/01-init.sql`](../seed/postgres/01-init.sql) y
  [`seed/mysql/01-init.sql`](../seed/mysql/01-init.sql) — el setup real de
  desarrollo, del que sale todo lo de esta guía.
- [`README.md`](../README.md#configurar-el-usuario-restringido) — el resumen rápido
  del usuario restringido.
- [`internal/api/integration_test.go`](../internal/api/integration_test.go) — los
  tests que verifican, contra bases reales, que el usuario restringido no puede
  hacer DDL ni tocar tablas fuera de la whitelist.
