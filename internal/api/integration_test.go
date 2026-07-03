package api_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	tcmysql "github.com/testcontainers/testcontainers-go/modules/mysql"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/indeclau/deitafix/internal/api"
	"github.com/indeclau/deitafix/internal/engine"
	"github.com/indeclau/deitafix/internal/store"
)

// Credenciales del usuario restringido, replicando el seed de docker-compose.
const (
	restrictedUser = "prod_datafix"
	restrictedPass = "dev_datafix_pw"
)

// engineFixture es lo que necesita el suite de integración: la connection
// string del usuario RESTRINGIDO (con la que corre el servicio) y una función
// para consultar el estado real de la base como admin (para verificar
// persistencia). Todo con el schema del seed ya aplicado.
//
// tbl es el nombre de la tabla objetivo tal como debe escribirse en SQL crudo
// para cada motor: Postgres la creó case-sensitive y requiere comillas dobles
// ("CollectionBox"); MySQL/MariaDB usa el nombre pelado (CollectionBox).
type engineFixture struct {
	engineName    string
	restrictedURL string
	tbl           string
	adminQuery    func(t *testing.T, query string, args ...any) int
}

// TestPreviewConfirmIntegration ejecuta el suite de integración contra el motor
// indicado por DEITAFIX_TEST_ENGINE. Si la variable no está seteada, se salta
// (así `go test ./...` en local sin Docker no falla).
func TestPreviewConfirmIntegration(t *testing.T) {
	engineName := os.Getenv("DEITAFIX_TEST_ENGINE")
	if engineName == "" {
		t.Skip("DEITAFIX_TEST_ENGINE no seteada; se omite el suite de integración")
	}

	ctx := context.Background()

	var fx engineFixture
	switch engineName {
	case "postgres":
		fx = setupPostgres(ctx, t)
	case "mysql":
		fx = setupMySQL(ctx, t)
	default:
		t.Fatalf("DEITAFIX_TEST_ENGINE inválido: %q", engineName)
	}

	// Servicio configurado con el usuario RESTRINGIDO y whitelist = CollectionBox.
	eng, err := engine.Open(ctx, fx.engineName, fx.restrictedURL)
	if err != nil {
		t.Fatalf("abriendo engine restringido: %v", err)
	}
	t.Cleanup(func() { _ = eng.Close() })

	svc := api.NewService(eng, store.New(time.Minute), []string{"CollectionBox"}, 50)

	t.Run("preview hace ROLLBACK (no persiste)", func(t *testing.T) {
		before := fx.adminQuery(t, fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE status = 0", fx.tbl))

		res, err := svc.Preview(ctx,
			fmt.Sprintf(`UPDATE %s SET status = 1 WHERE id > 0`, fx.tbl), nil)
		if err != nil {
			t.Fatalf("preview: %v", err)
		}
		if res.Token == "" {
			t.Fatal("preview no devolvió token")
		}

		after := fx.adminQuery(t, fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE status = 0", fx.tbl))
		if before != after {
			t.Fatalf("preview persistió cambios: status=0 antes=%d, después=%d", before, after)
		}
	})

	t.Run("affected_rows correcto", func(t *testing.T) {
		// status = 0 en 2 de las 3 filas del seed.
		res, err := svc.Preview(ctx,
			fmt.Sprintf(`UPDATE %s SET status = 9 WHERE status = 0`, fx.tbl), nil)
		if err != nil {
			t.Fatalf("preview: %v", err)
		}
		if res.AffectedRows != 2 {
			t.Fatalf("affected_rows = %d, want 2", res.AffectedRows)
		}
	})

	t.Run("confirm hace COMMIT (persiste)", func(t *testing.T) {
		// Cambiamos la fila id=1 a un status distintivo y confirmamos.
		res, err := svc.Preview(ctx,
			fmt.Sprintf(`UPDATE %s SET status = 777 WHERE id = 1`, fx.tbl), nil)
		if err != nil {
			t.Fatalf("preview: %v", err)
		}
		if res.AffectedRows != 1 {
			t.Fatalf("preview affected = %d, want 1", res.AffectedRows)
		}

		if _, err := svc.Confirm(ctx, res.Token); err != nil {
			t.Fatalf("confirm: %v", err)
		}

		got := fx.adminQuery(t, fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE status = 777", fx.tbl))
		if got != 1 {
			t.Fatalf("confirm no persistió: filas con status=777 = %d, want 1", got)
		}
	})

	t.Run("bounded op UPDATE persiste en confirm", func(t *testing.T) {
		op := &engine.BoundedOp{
			Op:    "UPDATE",
			Table: "CollectionBox",
			Set:   map[string]any{"status": 555},
			Where: map[string]any{"id": 2},
		}
		res, err := svc.Preview(ctx, "", op)
		if err != nil {
			t.Fatalf("preview acotado: %v", err)
		}
		if _, err := svc.Confirm(ctx, res.Token); err != nil {
			t.Fatalf("confirm acotado: %v", err)
		}
		got := fx.adminQuery(t, fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE status = 555", fx.tbl))
		if got != 1 {
			t.Fatalf("bounded op no persistió: status=555 = %d, want 1", got)
		}
	})

	t.Run("permission denied fuera de whitelist", func(t *testing.T) {
		// AuditSensitive no tiene grants para el usuario restringido: aunque
		// pasara las guardas del código, la base debe negar el acceso. Para
		// aislar la contención a nivel motor, usamos un servicio con la tabla
		// en whitelist y verificamos que el error venga de la base.
		permissive := api.NewService(eng, store.New(time.Minute),
			[]string{"CollectionBox", "AuditSensitive"}, 50)

		auditTbl := "AuditSensitive"
		if fx.engineName == "postgres" {
			auditTbl = `"AuditSensitive"`
		}

		_, err := permissive.Preview(ctx,
			fmt.Sprintf(`UPDATE %s SET note = 'x' WHERE id = 1`, auditTbl), nil)
		if err == nil {
			t.Fatal("se esperaba permission denied de la base, got nil")
		}
		if !isPermissionError(err) {
			t.Fatalf("se esperaba error de permisos, got: %v", err)
		}
	})
}

// isPermissionError detecta el error de permisos de cada motor.
func isPermissionError(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "permission denied") || // postgres
		strings.Contains(msg, "denied") || // mysql/mariadb: "command denied"
		strings.Contains(msg, "insufficient privilege")
}

// --- Setup Postgres ---

func setupPostgres(ctx context.Context, t *testing.T) engineFixture {
	t.Helper()

	container, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("deitafix_dev"),
		tcpostgres.WithUsername("app"),
		tcpostgres.WithPassword("dev_app_pw"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("levantando postgres: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	adminURL, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	// Aplicar el schema como admin. La conexión vive hasta el final del test
	// (no con defer) porque el closure adminQuery la usa en los subtests.
	adminConn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("conectando como admin: %v", err)
	}
	t.Cleanup(func() { _ = adminConn.Close(context.Background()) })

	for _, stmt := range postgresSchema() {
		if _, err := adminConn.Exec(ctx, stmt); err != nil {
			t.Fatalf("aplicando schema (%.40s...): %v", stmt, err)
		}
	}

	restrictedURL := withUserPass(t, adminURL, restrictedUser, restrictedPass)

	return engineFixture{
		engineName:    "postgres",
		restrictedURL: restrictedURL,
		tbl:           `"CollectionBox"`,
		adminQuery: func(t *testing.T, query string, args ...any) int {
			t.Helper()
			var n int
			if err := adminConn.QueryRow(ctx, query, args...).Scan(&n); err != nil {
				t.Fatalf("admin query %q: %v", query, err)
			}
			return n
		},
	}
}

// postgresSchema replica seed/postgres/01-init.sql, adaptado para el usuario
// "app" (dueño de las tablas) creando el usuario restringido y sus grants.
func postgresSchema() []string {
	return []string{
		`CREATE TABLE "CollectionBox" (
			id     SERIAL PRIMARY KEY,
			owner  TEXT NOT NULL,
			status INTEGER NOT NULL DEFAULT 0,
			amount NUMERIC(12,2) NOT NULL DEFAULT 0
		)`,
		`INSERT INTO "CollectionBox" (owner, status, amount) VALUES
			('cliente-a', 0, 1500.00),
			('cliente-b', 1, 200.50),
			('cliente-c', 0, 0.00)`,
		`CREATE TABLE "AuditSensitive" (id SERIAL PRIMARY KEY, note TEXT)`,
		fmt.Sprintf(`CREATE USER %s WITH PASSWORD '%s'`, restrictedUser, restrictedPass),
		fmt.Sprintf(`REVOKE ALL ON ALL TABLES IN SCHEMA public FROM %s`, restrictedUser),
		fmt.Sprintf(`GRANT USAGE ON SCHEMA public TO %s`, restrictedUser),
		fmt.Sprintf(`GRANT SELECT, INSERT, UPDATE, DELETE ON "CollectionBox" TO %s`, restrictedUser),
		fmt.Sprintf(`GRANT USAGE, SELECT ON SEQUENCE "CollectionBox_id_seq" TO %s`, restrictedUser),
	}
}

// --- Setup MySQL/MariaDB ---

func setupMySQL(ctx context.Context, t *testing.T) engineFixture {
	t.Helper()

	// El seed (tablas + usuario restringido + grants) se pasa como script de
	// init, que el entrypoint del container corre como root al arrancar —igual
	// que el docker-compose real—. Así evitamos que el usuario "app" (que no es
	// root) tenga que ejecutar CREATE USER / GRANT.
	scriptPath := writeTempScript(t, "seed-mysql-*.sql", mysqlSeedScript())

	container, err := tcmysql.Run(ctx, "mariadb:11",
		tcmysql.WithDatabase("deitafix_dev"),
		tcmysql.WithUsername("app"),
		tcmysql.WithPassword("dev_app_pw"),
		tcmysql.WithScripts(scriptPath),
		testcontainers.WithWaitStrategy(
			wait.ForListeningPort("3306/tcp").WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("levantando mariadb: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	// El módulo mysql expone un DSN nativo del driver para el usuario "app",
	// que es dueño de su DB y puede leer CollectionBox para las verificaciones.
	adminDSN, err := container.ConnectionString(ctx, "parseTime=true")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	adminDB, err := sql.Open("mysql", adminDSN)
	if err != nil {
		t.Fatalf("abriendo admin db: %v", err)
	}
	t.Cleanup(func() { _ = adminDB.Close() })
	if err := adminDB.PingContext(ctx); err != nil {
		t.Fatalf("ping admin: %v", err)
	}

	restrictedDSN := withMySQLUser(t, adminDSN, restrictedUser, restrictedPass)

	return engineFixture{
		engineName:    "mysql",
		restrictedURL: restrictedDSN,
		tbl:           "CollectionBox",
		adminQuery: func(t *testing.T, query string, args ...any) int {
			t.Helper()
			var n int
			if err := adminDB.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
				t.Fatalf("admin query %q: %v", query, err)
			}
			return n
		},
	}
}

// mysqlSeedScript replica seed/mysql/01-init.sql como un único script que corre
// como root en el arranque del container.
func mysqlSeedScript() string {
	return fmt.Sprintf(`
CREATE TABLE CollectionBox (
    id     INT AUTO_INCREMENT PRIMARY KEY,
    owner  VARCHAR(255) NOT NULL,
    status INT NOT NULL DEFAULT 0,
    amount DECIMAL(12,2) NOT NULL DEFAULT 0
);
INSERT INTO CollectionBox (owner, status, amount) VALUES
    ('cliente-a', 0, 1500.00),
    ('cliente-b', 1, 200.50),
    ('cliente-c', 0, 0.00);
CREATE TABLE AuditSensitive (id INT AUTO_INCREMENT PRIMARY KEY, note TEXT);
CREATE USER '%s'@'%%' IDENTIFIED BY '%s';
GRANT SELECT, INSERT, UPDATE, DELETE ON deitafix_dev.CollectionBox TO '%s'@'%%';
FLUSH PRIVILEGES;
`, restrictedUser, restrictedPass, restrictedUser)
}

// --- Helpers de connection string ---

// withUserPass reemplaza usuario y password en una URL postgres://.
func withUserPass(t *testing.T, raw, user, pass string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parseando URL: %v", err)
	}
	u.User = url.UserPassword(user, pass)
	return u.String()
}

// withMySQLUser reemplaza usuario y password en un DSN nativo del driver mysql.
func withMySQLUser(t *testing.T, dsn, user, pass string) string {
	t.Helper()
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parseando DSN: %v", err)
	}
	cfg.User = user
	cfg.Passwd = pass
	return cfg.FormatDSN()
}

// writeTempScript escribe contenido a un archivo temporal (auto-limpiado) y
// devuelve su ruta, para pasarlo como script de init al container.
func writeTempScript(t *testing.T, pattern, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), pattern)
	if err != nil {
		t.Fatalf("creando script temporal: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("escribiendo script temporal: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("cerrando script temporal: %v", err)
	}
	return f.Name()
}
