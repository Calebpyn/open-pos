package handler

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
)

// POST /api/order-items/{id}/deliver - Expo marca una pieza más como
// entregada ("2× Latte" se entrega de a una).
func (h *POSHandler) DeliverItem(w http.ResponseWriter, r *http.Request) {
	h.setItemDelivered(w, r, true)
}

// POST /api/order-items/{id}/undeliver - Deshace una pieza marcada por error.
func (h *POSHandler) UndeliverItem(w http.ResponseWriter, r *http.Request) {
	h.setItemDelivered(w, r, false)
}

func (h *POSHandler) setItemDelivered(w http.ResponseWriter, r *http.Request, delivered bool) {
	ctx := r.Context()
	itemID, ok := pathID(r)
	if !ok {
		http.Error(w, "ID de producto inválido", http.StatusBadRequest)
		return
	}

	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		serverError(w, "Error al iniciar transacción", err)
		return
	}
	defer tx.Rollback()

	var orderID int64
	var orderStatus, itemStatus string
	err = tx.QueryRowContext(ctx, `
		SELECT oi.order_id, o.status, oi.status
		FROM order_items oi JOIN orders o ON o.id = oi.order_id
		WHERE oi.id = ?`, itemID).Scan(&orderID, &orderStatus, &itemStatus)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "Producto no encontrado", http.StatusNotFound)
		return
	}
	if err != nil {
		serverError(w, "Error consultando el producto", err)
		return
	}
	if orderStatus == "CANCELLED" || itemStatus == "VOID" {
		http.Error(w, "El producto fue anulado o la orden cancelada", http.StatusConflict)
		return
	}

	// delivered_at marca cuándo salió la última pieza (congela el timer).
	q := `UPDATE order_items SET
			delivered_by = ?,
			delivered_quantity = delivered_quantity + 1,
			delivered_at = CASE WHEN delivered_quantity + 1 >= quantity
			                    THEN COALESCE(delivered_at, CURRENT_TIMESTAMP) END
		WHERE id = ? AND delivered_quantity < quantity`
	if !delivered {
		q = `UPDATE order_items SET delivered_quantity = delivered_quantity - 1, delivered_at = NULL
			WHERE id = ? AND delivered_quantity > 0`
	}
	args := []any{itemID}
	if delivered {
		args = []any{terminalUser(ctx), itemID}
	}
	if _, err := tx.ExecContext(ctx, q, args...); err != nil {
		serverError(w, "Error actualizando la entrega", err)
		return
	}
	if err := syncOrderDelivered(ctx, tx, orderID); err != nil {
		serverError(w, "Error actualizando la orden", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "Error al confirmar la entrega", err)
		return
	}
	w.Header().Set("HX-Trigger", "reload")
	w.WriteHeader(http.StatusOK)
}

// syncOrderDelivered marca la orden como entregada cuando ya no le quedan
// productos por entregar, y la regresa a pendiente si se destacha alguno.
// Las órdenes de comedor ya cerradas no se tocan: se entregan al cobrar.
func syncOrderDelivered(ctx context.Context, tx *sql.Tx, orderID int64) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE orders SET
			delivered_at = CASE
				WHEN EXISTS (SELECT 1 FROM order_items
				             WHERE order_id = orders.id AND status != 'VOID' AND delivered_quantity < quantity)
				THEN NULL
				ELSE COALESCE(delivered_at, CURRENT_TIMESTAMP) END,
			updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND status != 'CANCELLED' AND NOT (order_type = 'DINE_IN' AND status != 'ACTIVE')`, orderID)
	return err
}
