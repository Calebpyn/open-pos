-- Sesiones de la sección de administración. Se guarda el hash del token,
-- nunca el token: quien lea la base no puede suplantar una sesión.
CREATE TABLE IF NOT EXISTS sessions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    token_hash TEXT NOT NULL UNIQUE,
    user_id INTEGER NOT NULL REFERENCES users(id),
    scope TEXT NOT NULL DEFAULT 'ADMIN',
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    last_seen_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    revoked_at DATETIME,
    ip TEXT,
    user_agent TEXT
);

CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions (user_id);

-- Roles: ADMIN (Administrador), MANAGER (Encargado), CASHIER (Cajero),
-- WAITER (Mesero). El usuario genérico existente queda como ADMIN.

-- Productos y mesas ya usados en órdenes no se borran (romperían el
-- historial): se archivan. is_available sigue significando "agotado hoy".
ALTER TABLE products ADD COLUMN is_active BOOLEAN DEFAULT 1;
ALTER TABLE dining_tables ADD COLUMN is_active BOOLEAN DEFAULT 1;
