-- Configuración general del negocio (clave/valor).
CREATE TABLE IF NOT EXISTS settings (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

INSERT OR IGNORE INTO settings (key, value) VALUES
('business_name', 'Open POS'),
('tax_rate', '0.16'),
('prices_include_tax', '1'),
('ticket_footer', '¡Gracias por su visita!');

-- Clientes frecuentes / para facturación.
CREATE TABLE IF NOT EXISTS customers (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    phone TEXT,
    email TEXT,
    rfc TEXT,
    notes TEXT,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_customers_name ON customers (name);

-- Cuentas: subtotal = suma de productos, discount = descuento aplicado,
-- tax = IVA (incluido o agregado según settings), total = a pagar sin propina.
ALTER TABLE bills ADD COLUMN customer_id INTEGER REFERENCES customers(id);
ALTER TABLE bills ADD COLUMN discount_type TEXT; -- 'PERCENT' | 'AMOUNT'
ALTER TABLE bills ADD COLUMN discount_value REAL DEFAULT 0;
ALTER TABLE bills ADD COLUMN discount_reason TEXT;
ALTER TABLE bills ADD COLUMN tip REAL DEFAULT 0;
ALTER TABLE bills ADD COLUMN created_at DATETIME;
ALTER TABLE bills ADD COLUMN paid_at DATETIME;

-- Efectivo recibido y cambio entregado.
ALTER TABLE payments ADD COLUMN amount_received REAL;
ALTER TABLE payments ADD COLUMN change_given REAL DEFAULT 0;

-- Ciclo de vida de la orden: ACTIVE -> PAID | CANCELLED.
-- delivered_at permite que una orden para llevar pagada siga en Expo
-- hasta que se entregue.
ALTER TABLE orders ADD COLUMN closed_at DATETIME;
ALTER TABLE orders ADD COLUMN delivered_at DATETIME;
ALTER TABLE orders ADD COLUMN cancel_reason TEXT;

CREATE INDEX IF NOT EXISTS idx_orders_status ON orders (status);
CREATE INDEX IF NOT EXISTS idx_order_items_order ON order_items (order_id);
CREATE INDEX IF NOT EXISTS idx_order_items_bill ON order_items (bill_id);
