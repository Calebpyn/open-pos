-- 1. Áreas de producción (Barra, Cocina...). Cada producto se prepara en un
--    área y las comandas se imprimen por área, no por categoría: la categoría
--    queda solo para organizar el menú.
CREATE TABLE IF NOT EXISTS production_areas (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    color_hex TEXT NOT NULL DEFAULT '#2a78d6',
    sort_order INTEGER NOT NULL DEFAULT 0,
    is_active BOOLEAN NOT NULL DEFAULT 1
);

CREATE TABLE IF NOT EXISTS printer_areas (
    printer_id INTEGER NOT NULL,
    area_id INTEGER NOT NULL,
    PRIMARY KEY (printer_id, area_id),
    FOREIGN KEY (printer_id) REFERENCES printers(id) ON DELETE CASCADE,
    FOREIGN KEY (area_id) REFERENCES production_areas(id) ON DELETE CASCADE
);

ALTER TABLE products ADD COLUMN area_id INTEGER REFERENCES production_areas(id);

-- Se conserva lo que ya se imprimía: un área por impresora (con su nombre) y
-- cada producto va al área de la impresora que recibía su categoría. Si una
-- categoría salía en varias impresoras, se toma la primera; el área se puede
-- asignar después a más impresoras en Dispositivos.
INSERT INTO production_areas (id, name, sort_order)
SELECT id, name, id * 10 FROM printers
WHERE id IN (SELECT DISTINCT printer_id FROM printer_categories);

INSERT INTO printer_areas (printer_id, area_id)
SELECT id, id FROM production_areas;

UPDATE products SET area_id = (
    SELECT MIN(pc.printer_id) FROM printer_categories pc WHERE pc.category_id = products.category_id
);

DROP TABLE printer_categories;

-- 2. Subcategorías (un nivel): Bebidas -> Con café / Sin café.
ALTER TABLE categories ADD COLUMN parent_id INTEGER REFERENCES categories(id);

-- 3. Modificadores. Las tablas existen desde el esquema inicial; se completan.
ALTER TABLE modifier_groups ADD COLUMN sort_order INTEGER DEFAULT 0;
ALTER TABLE modifier_groups ADD COLUMN is_active BOOLEAN DEFAULT 1;
ALTER TABLE modifier_options ADD COLUMN sort_order INTEGER DEFAULT 0;
ALTER TABLE product_modifier_groups ADD COLUMN sort_order INTEGER DEFAULT 0;

-- El precio de la línea (unit_price) ya incluye los extras de sus
-- modificadores, así cobros, reportes y cortes no cambian. El detalle queda en
-- order_item_modifiers y el texto se guarda tal como se vendió para mostrarlo
-- en comandas, Expo y tickets aunque después se renombre la opción.
ALTER TABLE order_items ADD COLUMN modifiers_text TEXT;
ALTER TABLE order_item_modifiers ADD COLUMN name TEXT;

CREATE INDEX IF NOT EXISTS idx_order_item_modifiers_item ON order_item_modifiers (order_item_id);
