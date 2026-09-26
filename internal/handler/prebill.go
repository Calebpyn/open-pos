package handler

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/calebpyn/open-pos/internal/printer"
)

type preBillRequest struct {
	ItemIDs        []int64       `json:"item_ids"`   // vacío = todo lo pendiente
	Quantities     map[int64]int `json:"quantities"` // piezas de un renglón, si no son todas
	DiscountType   string        `json:"discount_type"`
	DiscountValue  float64       `json:"discount_value"`
	DiscountReason string        `json:"discount_reason"`
}

// POST /api/orders/{id}/prebill - Imprime la pre-cuenta: lo pendiente de
// cobro (o los productos elegidos, al dividir la cuenta) con su total, para
// llevarla a la mesa antes de cobrar. No registra ningún cobro.
func (h *POSHandler) PrintPreBill(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orderID, ok := pathID(r)
	if !ok {
		http.Error(w, "ID de orden inválido", http.StatusBadRequest)
		return
	}
	var req preBillRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "JSON inválido", http.StatusBadRequest)
			return
		}
	}

	t := &TicketData{PreBill: true, PaidAt: time.Now(), OrderID: orderID}
	var status string
	err := h.DB.QueryRowContext(ctx, `
		SELECT o.status, o.order_type, COALESCE(dt.name, ''), COALESCE(o.customer_name, '')
		FROM orders o
		LEFT JOIN table_sessions ts ON ts.id = o.session_id
		LEFT JOIN dining_tables dt ON dt.id = ts.table_id
		WHERE o.id = ?`, orderID).Scan(&status, &t.OrderType, &t.TableName, &t.CustomerName)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "Orden no encontrada", http.StatusNotFound)
		return
	}
	if err != nil {
		serverError(w, "Error consultando la orden", err)
		return
	}
	if status != "ACTIVE" {
		http.Error(w, "La orden ya no está activa", http.StatusConflict)
		return
	}
	if t.Settings, err = loadSettings(ctx, h.DB); err != nil {
		serverError(w, "Error cargando configuración", err)
		return
	}

	want := map[int64]bool{}
	for _, id := range req.ItemIDs {
		want[id] = true
	}
	var itemsCents int64
	if err := eachRow(ctx, h.DB, `
		SELECT oi.id, p.name, oi.quantity, oi.unit_price, COALESCE(oi.modifiers_text, ''), COALESCE(oi.notes, '')
		FROM order_items oi JOIN products p ON p.id = oi.product_id
		WHERE oi.order_id = ? AND oi.bill_id IS NULL AND oi.status != 'VOID'
		ORDER BY oi.id`, []any{orderID}, func(scan func(...any) error) error {
		var id int64
		var price float64
		var it TicketItem
		if err := scan(&id, &it.Name, &it.Quantity, &price, &it.Modifiers, &it.Notes); err != nil {
			return err
		}
		if len(want) > 0 && !want[id] {
			return nil
		}
		take, ok := partialQty(req.Quantities, id, int64(it.Quantity))
		if !ok {
			return badRequest("La cantidad de un producto no es válida. Recarga la pantalla.")
		}
		it.Quantity = int(take)
		amount := toCents(price) * take
		it.Amount = fromCents(amount)
		itemsCents += amount
		t.Items = append(t.Items, it)
		return nil
	}); err != nil {
		var bad badRequest
		if errors.As(err, &bad) {
			http.Error(w, bad.Error(), http.StatusBadRequest)
			return
		}
		serverError(w, "Error consultando productos", err)
		return
	}
	if len(t.Items) == 0 {
		http.Error(w, "No hay productos pendientes de cobro en esta orden", http.StatusConflict)
		return
	}

	// Pre-cuenta de una sola persona: "Cuenta de Juan".
	if len(req.ItemIDs) > 0 {
		ph := strings.TrimSuffix(strings.Repeat("?,", len(req.ItemIDs)), ",")
		args := make([]any, len(req.ItemIDs))
		for i, id := range req.ItemIDs {
			args[i] = id
		}
		t.GuestLabel = billGuestLabel(ctx, h.DB, `oi.id IN (`+ph+`)`, args...)
	}

	totals, err := computeTotals(itemsCents, req.DiscountType, req.DiscountValue, 0, t.Settings)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	t.Subtotal, t.Discount, t.Tax = fromCents(totals.Items), fromCents(totals.Discount), fromCents(totals.Tax)
	t.Total, t.Grand = fromCents(totals.Total), fromCents(totals.Total)
	t.DiscountType, t.DiscountValue = req.DiscountType, req.DiscountValue
	t.DiscountReason = strings.TrimSpace(req.DiscountReason)

	f, err := loadReceiptFormat(ctx, h.DB)
	if err != nil {
		serverError(w, "Error cargando el formato del ticket", err)
		return
	}
	logo := loadLogo(ctx, h.DB)
	out, err := printToReceiptPrinters(ctx, h.DB, func(width int) *printer.Ticket { return receiptDoc(t, width, f, logo) })
	if errors.Is(err, errNoReceiptPrinter) {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if err != nil {
		serverError(w, "Error imprimiendo la pre-cuenta", err)
		return
	}
	if len(out.Printed) == 0 {
		http.Error(w, "No se pudo imprimir en "+strings.Join(out.Failed, "; "), http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"printer": strings.Join(out.Printed, ", "), "total": t.Total})
}
