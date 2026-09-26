-- Índices para que las consultas no recorran todo el historial.

-- Pagos de una cuenta (ticket, corte, reportes, CSV): antes cada consulta
-- recorría la tabla completa de pagos.
CREATE INDEX IF NOT EXISTS idx_payments_bill ON payments (bill_id);

-- Reportes por rango de fechas: se filtra por paid_at (UTC) con índice y solo
-- después se agrupa por día y hora locales.
CREATE INDEX IF NOT EXISTS idx_bills_paid_at ON bills (paid_at) WHERE status = 'PAID';

-- Control de reportes y corte de caja: eventos de la bitácora por fecha.
CREATE INDEX IF NOT EXISTS idx_audit_logs_created ON audit_logs (created_at, action);

-- Expo: órdenes activas y las de mostrador pagadas pendientes de entregar.
CREATE INDEX IF NOT EXISTS idx_orders_status_delivered ON orders (status, delivered_at);

-- Cortes cerrados en un rango.
CREATE INDEX IF NOT EXISTS idx_cash_sessions_closed ON cash_sessions (closed_at) WHERE status = 'CLOSED';
