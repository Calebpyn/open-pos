-- Cada impresora define qué imprime: tickets de cobro y/o comandas de ciertas
-- categorías. Una categoría puede salir en varias impresoras.
CREATE TABLE IF NOT EXISTS printer_categories (
    printer_id INTEGER NOT NULL,
    category_id INTEGER NOT NULL,
    PRIMARY KEY (printer_id, category_id),
    FOREIGN KEY (printer_id) REFERENCES printers(id) ON DELETE CASCADE,
    FOREIGN KEY (category_id) REFERENCES categories(id) ON DELETE CASCADE
);

-- El ruteo anterior (una impresora por categoría) pasa a la tabla nueva.
INSERT OR IGNORE INTO printer_categories (printer_id, category_id)
SELECT printer_id, id FROM categories
WHERE printer_id IS NOT NULL AND printer_id IN (SELECT id FROM printers);

-- categories.printer_id queda en desuso; se limpia para que nadie lo lea por error.
UPDATE categories SET printer_id = NULL;

ALTER TABLE printers ADD COLUMN prints_receipts INTEGER DEFAULT 0;

-- La impresora de tickets configurada (o, si era automática, la primera
-- activa) conserva los tickets de cobro.
UPDATE printers SET prints_receipts = 1 WHERE id = COALESCE(
    (SELECT p.id FROM printers p
     JOIN settings s ON s.key = 'receipt_printer_id' AND CAST(s.value AS INTEGER) = p.id),
    (SELECT MIN(id) FROM printers WHERE is_active = 1)
);

DELETE FROM settings WHERE key = 'receipt_printer_id';

-- Antes no se imprimían comandas: lo ya registrado se preparó a mano, así que
-- se marca como enviado para que no salga de golpe con el siguiente adicional.
UPDATE order_items SET printed_quantity = quantity WHERE printed_quantity < quantity;
