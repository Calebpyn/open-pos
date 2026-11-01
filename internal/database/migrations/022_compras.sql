-- Compras: la lista de compras del negocio (reemplaza la hoja de papel con
-- "falta" y "urge"). Caja abre una lista, el equipo anota lo que falta desde
-- sus terminales, quien surte compra por lugar y caja la cierra; lo que no se
-- compró pasa solo a la siguiente.
--
-- Los insumos (lo que se compra) son distintos de los productos del menú.

-- Lugares donde se compra: Costco, el mercado, un proveedor que entrega.
CREATE TABLE IF NOT EXISTS compras_places (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    name_key TEXT NOT NULL UNIQUE, -- nombre sin acentos ni mayúsculas
    kind TEXT NOT NULL DEFAULT 'TIENDA', -- TIENDA, MERCADO, PROVEEDOR, EN_LINEA
    address TEXT NOT NULL DEFAULT '',
    phone TEXT NOT NULL DEFAULT '',
    notes TEXT NOT NULL DEFAULT '',
    sort_order INTEGER NOT NULL DEFAULT 0,
    is_active INTEGER NOT NULL DEFAULT 1,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS compras_supplies (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    name_key TEXT NOT NULL UNIQUE,
    category TEXT NOT NULL DEFAULT '',
    unit TEXT NOT NULL DEFAULT '',     -- presentación: "Bolsa 1 kg", "Caja 12 × 1 L"
    location TEXT NOT NULL DEFAULT '', -- dónde se guarda en el local: refri, alacena...
    notes TEXT NOT NULL DEFAULT '',
    is_active INTEGER NOT NULL DEFAULT 1,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Dónde se consigue cada insumo, en orden de preferencia (rank 1 = el de
-- siempre; los demás son alternativas cuando no hay). Precio por unidad de
-- la presentación, en centavos.
CREATE TABLE IF NOT EXISTS compras_sources (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    supply_id INTEGER NOT NULL REFERENCES compras_supplies(id) ON DELETE CASCADE,
    place_id INTEGER NOT NULL REFERENCES compras_places(id) ON DELETE CASCADE,
    brand TEXT NOT NULL DEFAULT '',
    price_cents INTEGER,
    rank INTEGER NOT NULL DEFAULT 1,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (supply_id, place_id)
);
CREATE INDEX IF NOT EXISTS idx_compras_sources_place ON compras_sources (place_id);

-- Otro insumo que sirve si no hay (Leche Lala → Leche Alpura).
CREATE TABLE IF NOT EXISTS compras_substitutes (
    supply_id INTEGER NOT NULL REFERENCES compras_supplies(id) ON DELETE CASCADE,
    substitute_id INTEGER NOT NULL REFERENCES compras_supplies(id) ON DELETE CASCADE,
    PRIMARY KEY (supply_id, substitute_id),
    CHECK (supply_id <> substitute_id)
);

-- Cada precio capturado al comprar: de aquí sale el presupuesto estimado.
CREATE TABLE IF NOT EXISTS compras_price_history (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    supply_id INTEGER NOT NULL REFERENCES compras_supplies(id) ON DELETE CASCADE,
    place_id INTEGER REFERENCES compras_places(id) ON DELETE SET NULL,
    price_cents INTEGER NOT NULL,
    recorded_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_compras_price_history ON compras_price_history (supply_id, recorded_at);

-- OPEN (se está llenando), SHOPPING (se está comprando; todavía se puede
-- agregar algo urgente), CLOSED.
CREATE TABLE IF NOT EXISTS compras_lists (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    status TEXT NOT NULL DEFAULT 'OPEN' CHECK (status IN ('OPEN', 'SHOPPING', 'CLOSED')),
    notes TEXT NOT NULL DEFAULT '',
    opened_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
    opened_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    shopping_at DATETIME,
    closed_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
    closed_at DATETIME
);
-- Una sola lista activa a la vez.
CREATE UNIQUE INDEX IF NOT EXISTS idx_compras_one_active ON compras_lists ((status <> 'CLOSED')) WHERE status <> 'CLOSED';

-- Un renglón por insumo en la lista, aunque lo pidan varias personas.
-- first_requested_at viaja con el renglón cuando no se surte y pasa a la
-- siguiente lista, para saber cuántos días lleva pendiente.
CREATE TABLE IF NOT EXISTS compras_items (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    list_id INTEGER NOT NULL REFERENCES compras_lists(id) ON DELETE CASCADE,
    supply_id INTEGER NOT NULL REFERENCES compras_supplies(id) ON DELETE CASCADE,
    urgent INTEGER NOT NULL DEFAULT 0,
    quantity INTEGER NOT NULL DEFAULT 1 CHECK (quantity > 0),
    status TEXT NOT NULL DEFAULT 'PENDING' CHECK (status IN ('PENDING', 'BOUGHT', 'NOT_FOUND')),
    first_requested_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    carried_from INTEGER REFERENCES compras_items(id) ON DELETE SET NULL,
    late INTEGER NOT NULL DEFAULT 0, -- se pidió cuando ya estaban comprando
    bought_quantity INTEGER,
    bought_price_cents INTEGER, -- total pagado por el renglón
    bought_place_id INTEGER REFERENCES compras_places(id) ON DELETE SET NULL,
    bought_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
    bought_at DATETIME,
    resolution_note TEXT NOT NULL DEFAULT '',
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (list_id, supply_id)
);

-- Quién pidió qué (y con qué nota). user_id NULL = la compu central.
CREATE TABLE IF NOT EXISTS compras_requests (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    item_id INTEGER NOT NULL REFERENCES compras_items(id) ON DELETE CASCADE,
    user_id INTEGER REFERENCES users(id) ON DELETE SET NULL,
    urgent INTEGER NOT NULL DEFAULT 0,
    note TEXT NOT NULL DEFAULT '',
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_compras_requests_item ON compras_requests (item_id);

-- Quién puede surtir desde su terminal (además de caja y encargados).
CREATE TABLE IF NOT EXISTS compras_shoppers (
    user_id INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE
);
