-- Envíos de comanda ya procesados. Cada envío del POS trae un identificador
-- único; si llega dos veces (doble toque, reintento de red) el segundo no
-- vuelve a agregar productos ni a imprimir la comanda.
CREATE TABLE IF NOT EXISTS order_requests (
    ref TEXT PRIMARY KEY,
    order_id INTEGER NOT NULL REFERENCES orders(id),
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
