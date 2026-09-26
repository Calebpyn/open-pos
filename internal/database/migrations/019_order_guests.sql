-- Cuentas por persona: una orden (mesa grande, pedido de oficina) se divide
-- desde la toma en personas. Cada producto pertenece a una persona o a la
-- mesa en general (guest_id NULL). La persona se identifica por su número
-- dentro de la orden (1, 2, 3...) y puede tener nombre.
CREATE TABLE IF NOT EXISTS order_guests (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    order_id INTEGER NOT NULL REFERENCES orders(id),
    position INTEGER NOT NULL,
    name TEXT NOT NULL DEFAULT '',
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (order_id, position)
);

ALTER TABLE order_items ADD COLUMN guest_id INTEGER REFERENCES order_guests(id);
