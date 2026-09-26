-- Usuarios con PIN. Por ahora solo existe un usuario genérico (PIN 0001)
-- hasta que haya un panel de administración para darlos de alta.
-- pin_hash = sha256(pin_salt || ':' || pin) en hexadecimal.
CREATE TABLE IF NOT EXISTS users (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    role TEXT NOT NULL DEFAULT 'ADMIN',
    pin_salt TEXT NOT NULL,
    pin_hash TEXT NOT NULL,
    is_active BOOLEAN DEFAULT 1,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

INSERT INTO users (name, role, pin_salt, pin_hash)
SELECT 'Usuario general', 'ADMIN', 'open-pos-dummy',
       '7653b0613e9eb7acaebf28b02806abe4713ecdbdf2b1f51ad5d937de6ae47d41'
WHERE NOT EXISTS (SELECT 1 FROM users);

-- Turnos de caja. Cada cuenta cobrada pertenece al turno abierto al cobrarla.
-- Al cerrar se guarda el arqueo ciego: lo que el cajero contó/reportó y lo
-- que el sistema esperaba, congelado en ese momento.
CREATE TABLE IF NOT EXISTS cash_sessions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    status TEXT NOT NULL DEFAULT 'OPEN', -- OPEN | CLOSED
    opened_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    opened_by INTEGER NOT NULL REFERENCES users(id),
    opening_float REAL NOT NULL DEFAULT 0,
    opening_notes TEXT,
    closed_at DATETIME,
    closed_by INTEGER REFERENCES users(id),
    counted_cash REAL,          -- efectivo contado por el cajero
    count_detail TEXT,          -- JSON: piezas por denominación
    expected_cash REAL,         -- efectivo que el sistema esperaba
    reported_card REAL,         -- ventas reportadas por la terminal (Mercado Pago)
    reported_card_tips REAL,    -- propinas reportadas por la terminal
    expected_card REAL,         -- cobros con tarjeta registrados en el POS
    reported_transfer REAL,     -- NULL si no se capturó
    expected_transfer REAL,
    close_notes TEXT
);

-- Solo puede haber un turno abierto a la vez.
CREATE UNIQUE INDEX IF NOT EXISTS idx_cash_sessions_one_open
    ON cash_sessions (status) WHERE status = 'OPEN';

-- Entradas y salidas de efectivo que no son ventas.
CREATE TABLE IF NOT EXISTS cash_movements (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    cash_session_id INTEGER NOT NULL REFERENCES cash_sessions(id),
    kind TEXT NOT NULL,      -- DEPOSIT | OTHER_IN | WITHDRAWAL | EXPENSE | TIP_PAYOUT | OTHER_OUT
    amount REAL NOT NULL,    -- siempre positivo; el signo lo da kind
    reason TEXT NOT NULL,
    user_id INTEGER NOT NULL REFERENCES users(id),
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_cash_movements_session ON cash_movements (cash_session_id);

-- Notas agregadas después del cierre (el turno cerrado no se edita).
CREATE TABLE IF NOT EXISTS cash_session_notes (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    cash_session_id INTEGER NOT NULL REFERENCES cash_sessions(id),
    user_id INTEGER NOT NULL REFERENCES users(id),
    note TEXT NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- La cuenta (no el pago) se liga al turno: así también cuentan las cortesías
-- al 100%, que no generan pagos.
ALTER TABLE bills ADD COLUMN cash_session_id INTEGER REFERENCES cash_sessions(id);
CREATE INDEX IF NOT EXISTS idx_bills_cash_session ON bills (cash_session_id);
