-- Terminales: cada dispositivo que abre el POS desde la red (iPad, teléfono)
-- se identifica con una cookie propia. Por omisión es terminal de mesero
-- (vista simplificada, sin cobro ni admin) y se entra con PIN; el admin puede
-- darle acceso completo de caja. La compu central (el servidor) siempre tiene
-- acceso completo.
CREATE TABLE IF NOT EXISTS terminals (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    token_hash TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL DEFAULT '',
    kind TEXT NOT NULL DEFAULT 'WAITER',     -- WAITER | FULL
    user_id INTEGER REFERENCES users(id),     -- quién la está usando (sesión con PIN)
    session_expires_at DATETIME,
    ip TEXT,
    user_agent TEXT,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    last_seen_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Quién tomó cada producto y quién lo marcó entregado en Expo (NULL = la
-- compu central, sin usuario).
ALTER TABLE order_items ADD COLUMN added_by INTEGER REFERENCES users(id);
ALTER TABLE order_items ADD COLUMN delivered_by INTEGER REFERENCES users(id);
