-- Plano del salón: cada zona es una cuadrícula donde las mesas y los
-- elementos fijos (paredes, barra, puerta, textos) ocupan celdas completas.

ALTER TABLE dining_tables ADD COLUMN pos_x INTEGER NOT NULL DEFAULT 0;
ALTER TABLE dining_tables ADD COLUMN pos_y INTEGER NOT NULL DEFAULT 0;
ALTER TABLE dining_tables ADD COLUMN width INTEGER NOT NULL DEFAULT 2;
ALTER TABLE dining_tables ADD COLUMN height INTEGER NOT NULL DEFAULT 2;
-- SQUARE (esquinas redondeadas) o ROUND; con ancho distinto al alto queda
-- rectangular u ovalada.
ALTER TABLE dining_tables ADD COLUMN shape TEXT NOT NULL DEFAULT 'SQUARE';

-- Acomodo inicial: 4 mesas por fila en cada zona, separadas una celda.
UPDATE dining_tables SET
    pos_x = 1 + ((SELECT rn FROM (SELECT id, ROW_NUMBER() OVER (PARTITION BY zone ORDER BY sort_order, id) - 1 AS rn
                                   FROM dining_tables) r WHERE r.id = dining_tables.id) % 4) * 3,
    pos_y = 1 + ((SELECT rn FROM (SELECT id, ROW_NUMBER() OVER (PARTITION BY zone ORDER BY sort_order, id) - 1 AS rn
                                   FROM dining_tables) r WHERE r.id = dining_tables.id) / 4) * 3;

CREATE TABLE IF NOT EXISTS floor_items (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    zone TEXT NOT NULL,
    kind TEXT NOT NULL, -- WALL, COUNTER, DOOR, LABEL
    label TEXT NOT NULL DEFAULT '',
    pos_x INTEGER NOT NULL DEFAULT 0,
    pos_y INTEGER NOT NULL DEFAULT 0,
    width INTEGER NOT NULL DEFAULT 1,
    height INTEGER NOT NULL DEFAULT 1
);

CREATE INDEX IF NOT EXISTS idx_floor_items_zone ON floor_items (zone);
