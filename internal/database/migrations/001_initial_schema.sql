PRAGMA foreign_keys = ON;

-- 1. CONFIGURACIÓN DE HARDWARE Y OPERACIÓN
CREATE TABLE IF NOT EXISTS printers (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    system_name TEXT NOT NULL,
    connection_type TEXT DEFAULT 'CUPS',
    ip_address TEXT,
    port INTEGER DEFAULT 9100,
    is_active BOOLEAN DEFAULT 1
);

CREATE TABLE IF NOT EXISTS categories (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    color_hex TEXT DEFAULT '#3B82F6',
    sort_order INTEGER DEFAULT 0,
    printer_id INTEGER,
    is_active BOOLEAN DEFAULT 1,
    FOREIGN KEY (printer_id) REFERENCES printers(id)
);

-- 2. PRODUCTOS Y MODIFICADORES
CREATE TABLE IF NOT EXISTS products (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    category_id INTEGER NOT NULL,
    name TEXT NOT NULL,
    description TEXT,
    price REAL NOT NULL,
    sku TEXT,
    is_available BOOLEAN DEFAULT 1,
    sort_order INTEGER DEFAULT 0,
    FOREIGN KEY (category_id) REFERENCES categories(id)
);

CREATE TABLE IF NOT EXISTS modifier_groups (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    min_selectable INTEGER DEFAULT 0,
    max_selectable INTEGER DEFAULT 1
);

CREATE TABLE IF NOT EXISTS product_modifier_groups (
    product_id INTEGER NOT NULL,
    modifier_group_id INTEGER NOT NULL,
    PRIMARY KEY (product_id, modifier_group_id),
    FOREIGN KEY (product_id) REFERENCES products(id) ON DELETE CASCADE,
    FOREIGN KEY (modifier_group_id) REFERENCES modifier_groups(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS modifier_options (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    modifier_group_id INTEGER NOT NULL,
    name TEXT NOT NULL,
    price_extra REAL DEFAULT 0.00,
    is_available BOOLEAN DEFAULT 1,
    FOREIGN KEY (modifier_group_id) REFERENCES modifier_groups(id) ON DELETE CASCADE
);

-- 3. MESAS Y SESIONES
CREATE TABLE IF NOT EXISTS dining_tables (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    zone TEXT DEFAULT 'General',
    status TEXT DEFAULT 'FREE',
    sort_order INTEGER DEFAULT 0
);

CREATE TABLE IF NOT EXISTS table_sessions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    table_id INTEGER NOT NULL,
    guest_count INTEGER DEFAULT 1,
    opened_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    closed_at DATETIME,
    status TEXT DEFAULT 'OPEN',
    FOREIGN KEY (table_id) REFERENCES dining_tables(id)
);

-- 4. ORDENES, ÍTEMS Y SUB-CUENTAS
CREATE TABLE IF NOT EXISTS orders (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id INTEGER NOT NULL,
    user_id INTEGER,
    order_type TEXT DEFAULT 'DINE_IN',
    status TEXT DEFAULT 'ACTIVE',
    notes TEXT,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (session_id) REFERENCES table_sessions(id)
);

CREATE TABLE IF NOT EXISTS bills (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    order_id INTEGER NOT NULL,
    bill_number INTEGER DEFAULT 1,
    subtotal REAL DEFAULT 0.00,
    tax REAL DEFAULT 0.00,
    discount REAL DEFAULT 0.00,
    total REAL DEFAULT 0.00,
    status TEXT DEFAULT 'UNPAID',
    FOREIGN KEY (order_id) REFERENCES orders(id)
);

CREATE TABLE IF NOT EXISTS order_items (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    order_id INTEGER NOT NULL,
    bill_id INTEGER,
    product_id INTEGER NOT NULL,
    unit_price REAL NOT NULL,
    quantity INTEGER DEFAULT 1,
    printed_quantity INTEGER DEFAULT 0,
    notes TEXT,
    status TEXT DEFAULT 'PENDING',
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (order_id) REFERENCES orders(id),
    FOREIGN KEY (bill_id) REFERENCES bills(id),
    FOREIGN KEY (product_id) REFERENCES products(id)
);

CREATE TABLE IF NOT EXISTS order_item_modifiers (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    order_item_id INTEGER NOT NULL,
    modifier_option_id INTEGER NOT NULL,
    unit_price REAL NOT NULL,
    FOREIGN KEY (order_item_id) REFERENCES order_items(id) ON DELETE CASCADE,
    FOREIGN KEY (modifier_option_id) REFERENCES modifier_options(id)
);

-- 5. PAGOS Y AUDITORÍA
CREATE TABLE IF NOT EXISTS payments (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    bill_id INTEGER NOT NULL,
    payment_method TEXT NOT NULL,
    amount REAL NOT NULL,
    tip_amount REAL DEFAULT 0.00,
    reference_code TEXT,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (bill_id) REFERENCES bills(id)
);

CREATE TABLE IF NOT EXISTS audit_logs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER,
    action TEXT NOT NULL,
    details TEXT,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
