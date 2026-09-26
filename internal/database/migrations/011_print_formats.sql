-- Formato de las impresiones, editable desde Admin -> Impresiones.
--   key 'receipt'          -> ticket de compra
--   key 'comanda:<area>'   -> comandas de un área de producción
--   key 'logo'             -> logo del ticket (PNG en data)
-- config es JSON; lo que falte toma el valor por omisión, que reproduce el
-- formato que se imprimía antes de existir el editor.
CREATE TABLE IF NOT EXISTS print_formats (
    key TEXT PRIMARY KEY,
    config TEXT NOT NULL DEFAULT '{}',
    data BLOB,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
