-- Cada lugar puede vender el insumo en otra presentación (Costco: burbuja de
-- 400 g; Fresh Market: de 200 g) o a granel (precio por kg). Con el
-- contenido se compara el precio por kilo/litro/pieza entre lugares.
ALTER TABLE compras_sources ADD COLUMN pack_label TEXT NOT NULL DEFAULT '';   -- presentación en ese lugar
ALTER TABLE compras_sources ADD COLUMN content_qty REAL;                      -- contenido de la presentación
ALTER TABLE compras_sources ADD COLUMN content_unit TEXT NOT NULL DEFAULT ''; -- g, kg, ml, L, pz
ALTER TABLE compras_sources ADD COLUMN by_weight INTEGER NOT NULL DEFAULT 0;  -- 1 = a granel: price_cents es por kg

-- Cantidades en kg (1.5 kg de aguacate): las columnas INTEGER de SQLite
-- guardan decimales sin cambiar el esquema.
