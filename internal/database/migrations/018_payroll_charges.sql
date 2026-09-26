-- Consumo de colaboradores con cargo a nómina: se vende (con su descuento),
-- no entra dinero a caja y se descuenta en la nómina semanal.

-- Colaboradores a quienes se puede cargar consumo. Son aparte de los
-- usuarios del POS: cocina o mantenimiento no necesitan PIN ni acceso.
CREATE TABLE IF NOT EXISTS collaborators (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    is_active BOOLEAN NOT NULL DEFAULT 1,
    weekly_limit REAL,               -- tope de cargos por semana (NULL = sin tope)
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Se empieza con un colaborador por cada usuario activo.
INSERT INTO collaborators (name)
SELECT name FROM users WHERE COALESCE(is_active, 1) = 1 ORDER BY id;

-- Semana descontada en nómina: agrupa los cargos que ya se aplicaron.
CREATE TABLE IF NOT EXISTS payroll_settlements (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    collaborator_id INTEGER NOT NULL REFERENCES collaborators(id),
    week_start TEXT NOT NULL,        -- lunes de la semana, AAAA-MM-DD (hora local)
    amount REAL NOT NULL,
    settled_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    settled_by INTEGER REFERENCES users(id)
);

-- Un pago "Cargo a nómina" (payment_method = 'PAYROLL') dice a quién se carga,
-- quién de caja lo autorizó con su PIN y, al descontarse, en qué liquidación.
ALTER TABLE payments ADD COLUMN collaborator_id INTEGER REFERENCES collaborators(id);
ALTER TABLE payments ADD COLUMN authorized_by INTEGER REFERENCES users(id);
ALTER TABLE payments ADD COLUMN settlement_id INTEGER REFERENCES payroll_settlements(id);

CREATE INDEX IF NOT EXISTS idx_payments_collaborator ON payments (collaborator_id) WHERE collaborator_id IS NOT NULL;
