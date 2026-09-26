-- Las ventas Para Llevar y Flash no tienen mesa: table_id pasa a ser opcional.
-- SQLite no permite quitar un NOT NULL con ALTER, así que se reconstruye la tabla.
CREATE TABLE table_sessions_new (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    table_id INTEGER,
    guest_count INTEGER DEFAULT 1,
    opened_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    closed_at DATETIME,
    status TEXT DEFAULT 'OPEN',
    FOREIGN KEY (table_id) REFERENCES dining_tables(id)
);

INSERT INTO table_sessions_new (id, table_id, guest_count, opened_at, closed_at, status)
SELECT id, table_id, guest_count, opened_at, closed_at, status FROM table_sessions;

DROP TABLE table_sessions;
ALTER TABLE table_sessions_new RENAME TO table_sessions;

-- Una mesa solo puede tener una sesión abierta a la vez, aunque dos
-- terminales intenten abrirla al mismo tiempo.
CREATE UNIQUE INDEX idx_table_sessions_one_open
    ON table_sessions (table_id)
    WHERE status = 'OPEN' AND table_id IS NOT NULL;

-- Nombre o folio del cliente para órdenes Para Llevar / Flash.
ALTER TABLE orders ADD COLUMN customer_name TEXT;
