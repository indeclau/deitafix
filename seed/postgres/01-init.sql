-- Seed PostgreSQL para desarrollo local de Deitafix.
-- Corre una sola vez, al crear la base por primera vez.

-- 1. Tabla de ejemplo (simula una tabla de la app).
CREATE TABLE "CollectionBox" (
    id     SERIAL PRIMARY KEY,
    owner  TEXT        NOT NULL,
    status INTEGER     NOT NULL DEFAULT 0,
    amount NUMERIC(12,2) NOT NULL DEFAULT 0
);

INSERT INTO "CollectionBox" (owner, status, amount) VALUES
    ('cliente-a', 0, 1500.00),
    ('cliente-b', 1,  200.50),
    ('cliente-c', 0,    0.00);

-- Tabla que NO esta en la whitelist (para testear que el usuario restringido
-- NO puede tocarla: el intento debe fallar con permission denied).
CREATE TABLE "AuditSensitive" (
    id   SERIAL PRIMARY KEY,
    note TEXT
);

-- 2. Usuario restringido: la salvaguarda a nivel motor.
CREATE USER prod_datafix WITH PASSWORD 'dev_datafix_pw';

-- Sin permisos por defecto.
REVOKE ALL ON ALL TABLES IN SCHEMA public FROM prod_datafix;
REVOKE ALL ON SCHEMA public FROM prod_datafix;

-- Puede "ver" el schema para resolver nombres de tabla.
GRANT USAGE ON SCHEMA public TO prod_datafix;

-- Whitelist EXPLICITA: solo esta tabla, solo datos. Sin DDL, sin DROP/TRUNCATE.
GRANT SELECT, INSERT, UPDATE, DELETE ON "CollectionBox" TO prod_datafix;
-- INSERT necesita la secuencia del id autoincremental:
GRANT USAGE, SELECT ON SEQUENCE "CollectionBox_id_seq" TO prod_datafix;

-- Deliberadamente NO se otorga nada sobre "AuditSensitive".
