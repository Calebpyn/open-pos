-- Entrega por producto: Expo tacha cada producto conforme sale a la mesa.
-- orders.delivered_at se sigue usando para la orden completa y se llena solo
-- cuando ya se entregaron todos sus productos.
ALTER TABLE order_items ADD COLUMN delivered_at DATETIME;

-- Las órdenes ya entregadas traen todos sus productos entregados.
UPDATE order_items SET delivered_at = (
    SELECT o.delivered_at FROM orders o WHERE o.id = order_items.order_id
)
WHERE delivered_at IS NULL
  AND order_id IN (SELECT id FROM orders WHERE delivered_at IS NOT NULL);
