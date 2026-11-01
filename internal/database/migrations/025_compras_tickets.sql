-- Tickets de compra: la foto del ticket se lee con OCR y, después de
-- revisarla, se registra la compra (se tacha lo que estaba en la lista y se
-- actualizan los precios). La imagen vive en compras-tickets/ junto a la base.
CREATE TABLE IF NOT EXISTS compras_receipts (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    status TEXT NOT NULL DEFAULT 'DRAFT' CHECK (status IN ('DRAFT', 'CONFIRMED')),
    place_id INTEGER REFERENCES compras_places(id) ON DELETE SET NULL,
    purchased_on TEXT NOT NULL DEFAULT '', -- AAAA-MM-DD
    total_cents INTEGER,
    image TEXT NOT NULL DEFAULT '',     -- archivo dentro de compras-tickets/
    ocr_text TEXT NOT NULL DEFAULT '',  -- renglones leídos, para revisar
    ocr_error TEXT NOT NULL DEFAULT '',
    notes TEXT NOT NULL DEFAULT '',
    list_id INTEGER REFERENCES compras_lists(id) ON DELETE SET NULL,
    uploaded_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
    confirmed_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    confirmed_at DATETIME
);
CREATE INDEX IF NOT EXISTS idx_compras_receipts_status ON compras_receipts (status, created_at);

CREATE TABLE IF NOT EXISTS compras_receipt_lines (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    receipt_id INTEGER NOT NULL REFERENCES compras_receipts(id) ON DELETE CASCADE,
    description TEXT NOT NULL,
    quantity REAL NOT NULL DEFAULT 1,
    by_weight INTEGER NOT NULL DEFAULT 0,
    amount_cents INTEGER NOT NULL DEFAULT 0,
    supply_id INTEGER REFERENCES compras_supplies(id) ON DELETE SET NULL,
    item_id INTEGER REFERENCES compras_items(id) ON DELETE SET NULL,
    sort_order INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_compras_receipt_lines ON compras_receipt_lines (receipt_id);

-- Lo que se aprende al confirmar: "LECHE ENT LALA 12PK" es Leche entera. La
-- próxima vez ese renglón sale ya relacionado.
CREATE TABLE IF NOT EXISTS compras_receipt_aliases (
    text_key TEXT PRIMARY KEY,
    supply_id INTEGER NOT NULL REFERENCES compras_supplies(id) ON DELETE CASCADE,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
