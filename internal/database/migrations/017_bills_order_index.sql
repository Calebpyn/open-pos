-- Cuentas de una orden (Expo, pantalla de cobro, ticket): sin índice, cada
-- consulta recorría todas las cuentas del historial y Expo de comedor
-- tardaba más de un segundo con un año de ventas.
CREATE INDEX IF NOT EXISTS idx_bills_order ON bills (order_id);
