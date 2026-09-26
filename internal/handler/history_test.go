package handler

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestHistoryAndTimings(t *testing.T) {
	db, h := setupCashDB(t)
	cookie := adminLogin(t, h, "0001")
	exec(t, db, `INSERT INTO table_sessions (id, table_id, status) VALUES (600, 1, 'OPEN')`)
	exec(t, db, `INSERT INTO orders (id, session_id, order_type, status) VALUES (600, 600, 'DINE_IN', 'ACTIVE')`)
	// Latte entregado a los 6 min, espresso a los 4; un chilaquiles sin marcar y uno anulado.
	exec(t, db, `INSERT INTO order_items (id, order_id, product_id, unit_price, quantity, status, created_at, delivered_at, delivered_quantity) VALUES
		(61, 600, 2, 65, 1, 'PENDING', datetime('now', '-10 minutes'), datetime('now', '-4 minutes'), 1),
		(62, 600, 1, 45, 1, 'PENDING', datetime('now', '-10 minutes'), datetime('now', '-6 minutes'), 1),
		(63, 600, 3, 150, 1, 'PENDING', datetime('now', '-10 minutes'), NULL, 0),
		(64, 600, 3, 150, 1, 'VOID', datetime('now', '-10 minutes'), NULL, 0)`)
	post(t, h.OpenCash, 0, `{"pin": "0001", "opening_float": 500}`)
	if rec := post(t, h.PayOrder, 600, `{"item_ids": [61, 62, 63], "tip": 20, "payments": [
		{"method": "CASH", "amount": 100, "received": 100}, {"method": "CARD_DEBIT", "amount": 180, "reference": "1234"}]}`); rec.Code != http.StatusCreated {
		t.Fatalf("cobro: %d %s", rec.Code, rec.Body)
	}

	// Lista: la orden aparece cobrada, con su importe y lo cobrado.
	rec := call(h.RequireAdmin(h.AdminHistoryOrders), "GET", "/api/admin/history/orders?status=PAID&q=Mesa", "", cookie, 0)
	var list struct {
		Orders []HistoryOrder
		Total  int
	}
	json.Unmarshal(rec.Body.Bytes(), &list)
	if rec.Code != http.StatusOK || list.Total != 1 || list.Orders[0].Paid != 280 || list.Orders[0].Voids != 1 || list.Orders[0].Items != 3 {
		t.Fatalf("historial: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h.RequireAdmin(h.AdminHistoryOrders), "GET", "/api/admin/history/orders?status=CANCELLED", "", cookie, 0); !json.Valid(rec.Body.Bytes()) {
		t.Fatal("filtro por estado")
	}

	// Detalle: productos con tiempo de salida, cuenta con pagos y cajero, bitácora.
	rec = call(h.RequireAdmin(h.AdminHistoryOrder), "GET", "/", "", cookie, 600)
	var d struct {
		Items  []HistoryItem
		Bills  []HistoryBill
		Events []HistoryEvent
	}
	json.Unmarshal(rec.Body.Bytes(), &d)
	if len(d.Items) != 4 || d.Items[0].ServeMin == nil || *d.Items[0].ServeMin < 5.9 || *d.Items[0].ServeMin > 6.1 {
		t.Fatalf("productos del detalle: %s", rec.Body)
	}
	if len(d.Bills) != 1 || len(d.Bills[0].Payments) != 2 || d.Bills[0].Cashier != "Usuario general" || d.Bills[0].Tip != 20 {
		t.Fatalf("cuentas del detalle: %s", rec.Body)
	}
	if len(d.Events) == 0 || d.Events[0].Action != "BILL_PAID" {
		t.Fatalf("la bitácora de la orden debe incluir el cobro: %+v", d.Events)
	}

	// Pagos: dos, con totales por método.
	rec = call(h.RequireAdmin(h.AdminHistoryPayments), "GET", "/", "", cookie, 0)
	var pays struct {
		Payments []PaymentRow
		Total    float64
	}
	json.Unmarshal(rec.Body.Bytes(), &pays)
	if len(pays.Payments) != 2 || pays.Total != 280 {
		t.Fatalf("pagos: %s", rec.Body)
	}

	// Tiempos: 2 medidos (6 y 4 min), 1 sin marcar; el anulado no cuenta.
	rec = call(h.RequireAdmin(h.AdminTimings), "GET", "/", "", cookie, 0)
	var tm struct {
		Areas              []TimingStat
		Total, Measured    int
		Unmarked, Outliers int
	}
	json.Unmarshal(rec.Body.Bytes(), &tm)
	if tm.Total != 3 || tm.Measured != 2 || tm.Unmarked != 1 {
		t.Fatalf("tiempos: %s", rec.Body)
	}
	var n int
	var sum float64
	for _, a := range tm.Areas {
		n += a.Count
		sum += a.Avg * float64(a.Count)
	}
	if n != 2 || sum < 9.8 || sum > 10.2 {
		t.Fatalf("promedios por área: %+v", tm.Areas)
	}
}
