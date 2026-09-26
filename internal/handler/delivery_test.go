package handler

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func post(t *testing.T, fn http.HandlerFunc, id int64, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.SetPathValue("id", strconv.FormatInt(id, 10))
	rec := httptest.NewRecorder()
	fn(rec, req)
	return rec
}

func orderDelivered(t *testing.T, db *sql.DB, orderID int64) bool {
	t.Helper()
	var delivered bool
	if err := db.QueryRow(`SELECT delivered_at IS NOT NULL FROM orders WHERE id = ?`, orderID).Scan(&delivered); err != nil {
		t.Fatal(err)
	}
	return delivered
}

func TestItemDeliverySyncsOrder(t *testing.T) {
	db := setupComandaDB(t)
	h := NewPOSHandler(db)

	exec(t, db, `INSERT INTO table_sessions (id, table_id, status) VALUES (200, NULL, 'OPEN')`)
	exec(t, db, `INSERT INTO orders (id, session_id, order_type, status) VALUES (200, 200, 'TAKEAWAY', 'PAID')`)
	exec(t, db, `INSERT INTO order_items (id, order_id, product_id, unit_price, quantity, status) VALUES
		(21, 200, 1, 45, 1, 'PENDING'), (22, 200, 2, 65, 1, 'PENDING'), (23, 200, 3, 120, 1, 'VOID')`)

	if rec := post(t, h.DeliverItem, 21, ""); rec.Code != http.StatusOK {
		t.Fatalf("deliver: %d %s", rec.Code, rec.Body)
	}
	if orderDelivered(t, db, 200) {
		t.Fatal("con un producto pendiente la orden no debe estar entregada")
	}

	// El producto anulado no cuenta: con el segundo, la orden queda entregada.
	post(t, h.DeliverItem, 22, "")
	if !orderDelivered(t, db, 200) {
		t.Fatal("con todo entregado la orden debe quedar entregada")
	}

	// Destachar un producto regresa la orden a Expo.
	post(t, h.UndeliverItem, 22, "")
	if orderDelivered(t, db, 200) {
		t.Fatal("al destachar un producto la orden vuelve a estar pendiente")
	}

	if rec := post(t, h.DeliverItem, 23, ""); rec.Code != http.StatusConflict {
		t.Fatalf("un producto anulado no se puede entregar, código %d", rec.Code)
	}
}

func lastAudit(t *testing.T, db *sql.DB) (string, map[string]any) {
	t.Helper()
	var action, details string
	if err := db.QueryRow(`SELECT action, details FROM audit_logs ORDER BY id DESC LIMIT 1`).Scan(&action, &details); err != nil {
		t.Fatal(err)
	}
	var d map[string]any
	if err := json.Unmarshal([]byte(details), &d); err != nil {
		t.Fatal(err)
	}
	return action, d
}

func TestCloseOrderWithoutPayment(t *testing.T) {
	db := setupComandaDB(t)
	h := NewPOSHandler(db)

	exec(t, db, `UPDATE dining_tables SET status = 'OCCUPIED' WHERE id = 1`)
	exec(t, db, `INSERT INTO table_sessions (id, table_id, status) VALUES (300, 1, 'OPEN')`)
	exec(t, db, `INSERT INTO orders (id, session_id, order_type, status) VALUES (300, 300, 'DINE_IN', 'ACTIVE')`)
	exec(t, db, `INSERT INTO order_items (id, order_id, product_id, unit_price, quantity, printed_quantity, delivered_at, delivered_quantity) VALUES
		(31, 300, 1, 45, 2, 2, CURRENT_TIMESTAMP, 2), (32, 300, 4, 85, 1, 0, NULL, 0)`)

	if rec := post(t, h.CancelOrder, 300, `{"reason": ""}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("sin motivo debe rechazarse, código %d", rec.Code)
	}

	rec := post(t, h.CancelOrder, 300, `{"reason": "Error de captura del mesero: mesa equivocada"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body)
	}

	var status, reason, tableStatus string
	db.QueryRow(`SELECT status, cancel_reason FROM orders WHERE id = 300`).Scan(&status, &reason)
	db.QueryRow(`SELECT status FROM dining_tables WHERE id = 1`).Scan(&tableStatus)
	if status != "CANCELLED" || reason != "Error de captura del mesero: mesa equivocada" || tableStatus != "FREE" {
		t.Fatalf("orden %s (%q), mesa %s", status, reason, tableStatus)
	}

	action, d := lastAudit(t, db)
	if action != "ORDER_CANCELLED" || d["comanda_sent"] != true ||
		d["items_sent"] != 2.0 || d["items_delivered"] != 2.0 || d["unpaid_amount"] != 175.0 {
		t.Fatalf("bitácora incompleta: %s %v", action, d)
	}
	if items, _ := d["items"].([]any); len(items) != 2 {
		t.Fatalf("la bitácora debe guardar los productos: %v", d["items"])
	}
}

func TestCloseOrderWithPartialPayment(t *testing.T) {
	db := setupComandaDB(t)
	h := NewPOSHandler(db)

	exec(t, db, `INSERT INTO table_sessions (id, table_id, status) VALUES (400, 2, 'OPEN')`)
	exec(t, db, `INSERT INTO orders (id, session_id, order_type, status) VALUES (400, 400, 'DINE_IN', 'ACTIVE')`)
	exec(t, db, `INSERT INTO bills (id, order_id, bill_number, total, status) VALUES (40, 400, 1, 45, 'PAID')`)
	exec(t, db, `INSERT INTO order_items (id, order_id, bill_id, product_id, unit_price, quantity) VALUES
		(41, 400, 40, 1, 45, 1), (42, 400, NULL, 2, 65, 1)`)

	rec := post(t, h.CancelOrder, 400, `{"reason": "Cliente se retiró sin pagar"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body)
	}

	var status, itemStatus string
	db.QueryRow(`SELECT status FROM orders WHERE id = 400`).Scan(&status)
	db.QueryRow(`SELECT status FROM order_items WHERE id = 42`).Scan(&itemStatus)
	if status != "PAID" || itemStatus != "VOID" {
		t.Fatalf("con cobros previos la orden queda PAID y lo pendiente VOID; quedó %s / %s", status, itemStatus)
	}

	action, d := lastAudit(t, db)
	if action != "ORDER_CLOSED_WITH_BALANCE" || d["unpaid_amount"] != 65.0 {
		t.Fatalf("bitácora: %s %v", action, d)
	}
}

func TestExpoRendersItems(t *testing.T) {
	db := setupComandaDB(t)
	ui := NewUIHandler(NewPOSHandler(db))

	exec(t, db, `INSERT INTO table_sessions (id, table_id, status) VALUES (500, 3, 'OPEN')`)
	exec(t, db, `INSERT INTO orders (id, session_id, order_type, status) VALUES (500, 500, 'DINE_IN', 'ACTIVE')`)
	exec(t, db, `INSERT INTO order_items (id, order_id, product_id, unit_price, quantity, printed_quantity, notes, delivered_at, delivered_quantity) VALUES
		(51, 500, 1, 45, 2, 2, 'Extra caliente', CURRENT_TIMESTAMP, 2), (52, 500, 3, 120, 1, 0, '', NULL, 0)`)

	rec := httptest.NewRecorder()
	ui.GetExpoHTML(rec, httptest.NewRequest(http.MethodGet, "/api/expo/html?type=DINE_IN", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expo: %d %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Mesa 3", "2× Espresso Doble", "Extra caliente", "1× Chilaquiles Verdes",
		"/api/order-items/51/undeliver", "/api/order-items/52/deliver",
		"2/3 entregados", "Sin comanda", "Cerrar sin pagar",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("falta %q en la Expo", want)
		}
	}
}

func TestExpoCompactsLargeOrders(t *testing.T) {
	small := ExpoOrder{Items: make([]ExpoItem, expoCompactAfter)}
	small.markExtras()
	if small.HiddenLines() != 0 {
		t.Fatal("una orden chica se muestra completa")
	}

	// Entregado, 4 pendientes, entregado: se ven los 3 primeros pendientes.
	big := ExpoOrder{Items: []ExpoItem{
		{ID: 1, Delivered: true}, {ID: 2}, {ID: 3}, {ID: 4}, {ID: 5}, {ID: 6, Delivered: true},
	}}
	big.markExtras()
	var visible []int64
	for _, it := range big.Items {
		if !it.Extra {
			visible = append(visible, it.ID)
		}
	}
	if len(visible) != 3 || visible[0] != 2 || visible[2] != 4 {
		t.Fatalf("visibles %v, se esperaban los pendientes 2, 3 y 4", visible)
	}
	if got := big.HiddenLabel(); got != "Ver 3 más (2 entregado(s))" {
		t.Fatalf("etiqueta %q", got)
	}
}

func TestDeliverPieceByPiece(t *testing.T) {
	db := setupComandaDB(t)
	h := NewPOSHandler(db)
	ui := NewUIHandler(h)
	exec(t, db, `INSERT INTO table_sessions (id, table_id, status) VALUES (900, NULL, 'OPEN')`)
	exec(t, db, `INSERT INTO orders (id, session_id, order_type, status, closed_at) VALUES (900, 900, 'TAKEAWAY', 'PAID', CURRENT_TIMESTAMP)`)
	exec(t, db, `INSERT INTO order_items (id, order_id, product_id, unit_price, quantity, printed_quantity) VALUES (91, 900, 2, 65, 3, 3)`)

	state := func() (qty int, done bool) {
		db.QueryRow(`SELECT delivered_quantity, delivered_at IS NOT NULL FROM order_items WHERE id = 91`).Scan(&qty, &done)
		return
	}
	post(t, h.DeliverItem, 91, "")
	if q, done := state(); q != 1 || done || orderDelivered(t, db, 900) {
		t.Fatalf("una pieza de 3: entregadas %d, completo %v", q, done)
	}
	expo := func() string {
		rec := httptest.NewRecorder()
		ui.GetExpoHTML(rec, httptest.NewRequest(http.MethodGet, "/api/expo/html?type=TAKEAWAY", nil))
		return rec.Body.String()
	}
	if out := expo(); !strings.Contains(out, "1 de 3 entregado(s)") || !strings.Contains(out, "Deshacer la última pieza") {
		t.Fatal("la Expo debe mostrar el avance por pieza y el botón para deshacer")
	}

	post(t, h.DeliverItem, 91, "")
	post(t, h.DeliverItem, 91, "")
	post(t, h.DeliverItem, 91, "") // de más: no pasa de 3
	if q, done := state(); q != 3 || !done || !orderDelivered(t, db, 900) {
		t.Fatalf("con las 3 piezas: entregadas %d, completo %v, orden entregada %v", q, done, orderDelivered(t, db, 900))
	}

	post(t, h.UndeliverItem, 91, "")
	if q, done := state(); q != 2 || done || orderDelivered(t, db, 900) {
		t.Fatalf("al deshacer una: entregadas %d, completo %v", q, done)
	}
}

func TestExpoHidesStalePaidOrders(t *testing.T) {
	db := setupComandaDB(t)
	ui := NewUIHandler(NewPOSHandler(db))
	exec(t, db, `INSERT INTO table_sessions (id, table_id, status) VALUES (950, NULL, 'CLOSED'), (951, NULL, 'CLOSED')`)
	// Pagadas sin entregar: una de hace 1 hora y otra de hace 2 días.
	exec(t, db, `INSERT INTO orders (id, session_id, order_type, status, customer_name, closed_at) VALUES
		(950, 950, 'TAKEAWAY', 'PAID', 'Reciente', datetime('now', '-1 hour')),
		(951, 951, 'TAKEAWAY', 'PAID', 'Olvidada', datetime('now', '-2 days'))`)
	rec := httptest.NewRecorder()
	ui.GetExpoHTML(rec, httptest.NewRequest(http.MethodGet, "/api/expo/html?type=TAKEAWAY", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "Reciente") || strings.Contains(body, "Olvidada") {
		t.Fatalf("la Expo debe ocultar las pagadas sin entregar de hace más de %s", expoPaidMaxAge)
	}
}
