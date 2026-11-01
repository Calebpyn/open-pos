-- Plugins: programas aparte que se instalan sobre Open POS (p. ej. inventario).
--
-- plugin_events es la bitácora de lo que pasa en el POS para los plugins. Se
-- escribe en la MISMA transacción que la venta, así un plugin caído o
-- instalado después no pierde ni duplica nada: lee desde su último seq.
CREATE TABLE IF NOT EXISTS plugin_events (
    seq INTEGER PRIMARY KEY AUTOINCREMENT,
    type TEXT NOT NULL,
    payload TEXT NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_plugin_events_created ON plugin_events (created_at);

-- Estado de cada plugin instalado. cursor es el último evento que el plugin
-- confirmó haber procesado (sirve para depurar la bitácora).
CREATE TABLE IF NOT EXISTS plugins (
    id TEXT PRIMARY KEY,
    enabled INTEGER NOT NULL DEFAULT 1,
    cursor INTEGER NOT NULL DEFAULT 0,
    installed_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
