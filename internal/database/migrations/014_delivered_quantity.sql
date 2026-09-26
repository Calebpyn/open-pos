-- Entrega por pieza: una línea "2× Latte" se puede marcar de a una.
-- delivered_at sigue siendo el momento en que se entregó la última pieza
-- (congela el timer del producto).
ALTER TABLE order_items ADD COLUMN delivered_quantity INTEGER NOT NULL DEFAULT 0;

UPDATE order_items SET delivered_quantity = quantity WHERE delivered_at IS NOT NULL;
