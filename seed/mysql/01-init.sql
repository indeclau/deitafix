-- Seed MariaDB para desarrollo local de Deitafix.
-- Corre una sola vez, al crear la base por primera vez, sobre deitafix_dev.

-- 1. Tabla de ejemplo (simula una tabla de la app).
CREATE TABLE CollectionBox (
    id     INT AUTO_INCREMENT PRIMARY KEY,
    owner  VARCHAR(255)   NOT NULL,
    status INT            NOT NULL DEFAULT 0,
    amount DECIMAL(12,2)  NOT NULL DEFAULT 0
);

INSERT INTO CollectionBox (owner, status, amount) VALUES
    ('cliente-a', 0, 1500.00),
    ('cliente-b', 1,  200.50),
    ('cliente-c', 0,    0.00);

-- Tabla que NO esta en la whitelist (para testear permission denied).
CREATE TABLE AuditSensitive (
    id   INT AUTO_INCREMENT PRIMARY KEY,
    note TEXT
);

-- 2. Usuario restringido: la salvaguarda a nivel motor.
-- (En MySQL/MariaDB no hay secuencias: AUTO_INCREMENT no requiere grant extra.)
CREATE USER 'prod_deitafix'@'%' IDENTIFIED BY 'dev_deitafix_pw';

-- Whitelist EXPLICITA: solo esta tabla, solo datos. Sin DDL, sin DROP/TRUNCATE.
GRANT SELECT, INSERT, UPDATE, DELETE ON deitafix_dev.CollectionBox TO 'prod_deitafix'@'%';

-- Deliberadamente NO se otorga nada sobre AuditSensitive.
FLUSH PRIVILEGES;
