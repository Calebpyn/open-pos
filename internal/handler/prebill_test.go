package handler

import (
	"bytes"
	"net/http"
	"testing"
)

func TestPreBill(t *testing.T) {
	db, h := setupCashDB(t)
	fp := newFakePrinter(t)
	exec(t, db, `UPDATE printers SET is_active = 1, prints_receipts = 1, connection_type = 'NETWORK',
		ip_address = '127.0.0.1', port = ? WHERE id = 1`, fp.port())
	exec(t, db, `INSERT INTO table_sessions (id, table_id, status) VALUES (600, 1, 'OPEN')`)
	exec(t, db, `INSERT INTO orders (id, session_id, order_type, status) VALUES (600, 600, 'DINE_IN', 'ACTIVE')`)
	exec(t, db, `INSERT INTO order_items (id, order_id, product_id, unit_price, quantity) VALUES (61, 600, 1, 45, 2), (62, 600, 2, 65, 1)`)

	// Toda la cuenta, sin turno de caja abierto: no es un cobro.
	if rec := post(t, h.PrintPreBill, 600, ""); rec.Code != http.StatusOK {
		t.Fatalf("pre-cuenta: %d %s", rec.Code, rec.Body)
	}
	job := fp.jobCount(t, 1)[0]
	for _, want := range []string{"PRE-CUENTA", "$155.00", "no es", "Propina sugerida", "$16.00"} {
		if !bytes.Contains(job, []byte(want)) {
			t.Fatalf("la pre-cuenta debe incluir %q:\n%s", want, job)
		}
	}
	var bills int
	db.QueryRow(`SELECT COUNT(*) FROM bills`).Scan(&bills)
	if bills != 0 {
		t.Fatal("imprimir la pre-cuenta no debe registrar un cobro")
	}

	// Solo lo seleccionado, con 10% de descuento: 90 - 9 = 81.
	if rec := post(t, h.PrintPreBill, 600, `{"item_ids": [61], "discount_type": "PERCENT", "discount_value": 10}`); rec.Code != http.StatusOK {
		t.Fatalf("pre-cuenta parcial: %d %s", rec.Code, rec.Body)
	}
	if job := fp.jobCount(t, 2)[1]; !bytes.Contains(job, []byte("$81.00")) || bytes.Contains(job, []byte("$65.00")) {
		t.Fatalf("pre-cuenta parcial con descuento:\n%s", job)
	}

	exec(t, db, `UPDATE orders SET status = 'PAID' WHERE id = 600`)
	if rec := post(t, h.PrintPreBill, 600, ""); rec.Code != http.StatusConflict {
		t.Fatalf("una orden cerrada no tiene pre-cuenta, dio %d", rec.Code)
	}
}

// "2× Pepperoni": se cobra una pieza en cada cuenta.
func TestPayPartOfRepeatedItem(t *testing.T) {
	db, h := setupCashDB(t)
	exec(t, db, `INSERT INTO table_sessions (id, table_id, status) VALUES (600, 1, 'OPEN')`)
	exec(t, db, `INSERT INTO orders (id, session_id, order_type, status) VALUES (600, 600, 'DINE_IN', 'ACTIVE')`)
	// 3 piezas: 3 ya en comanda, 1 entregada.
	exec(t, db, `INSERT INTO order_items (id, order_id, product_id, unit_price, quantity, printed_quantity, delivered_quantity, modifiers_text, created_at)
		VALUES (61, 600, 2, 100, 3, 3, 1, 'Orilla rellena', datetime('now', '-15 minutes'))`)
	exec(t, db, `INSERT INTO modifier_groups (id, name) VALUES (900, 'Orilla')`)
	exec(t, db, `INSERT INTO modifier_options (id, modifier_group_id, name) VALUES (900, 900, 'Orilla rellena')`)
	exec(t, db, `INSERT INTO order_item_modifiers (order_item_id, modifier_option_id, unit_price, name) VALUES (61, 900, 0, 'Orilla rellena')`)
	post(t, h.OpenCash, 0, `{"pin": "0001", "opening_float": 500}`)

	if rec := post(t, h.PayOrder, 600, `{"item_ids": [61], "quantities": {"61": 4}, "payments": [{"method": "CASH", "amount": 400}]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("más piezas de las que hay debe rechazarse, dio %d", rec.Code)
	}
	// Pre-cuenta de una sola pieza.
	fp := newFakePrinter(t)
	exec(t, db, `UPDATE printers SET is_active = 1, prints_receipts = 1, connection_type = 'NETWORK', ip_address = '127.0.0.1', port = ? WHERE id = 1`, fp.port())
	if rec := post(t, h.PrintPreBill, 600, `{"item_ids": [61], "quantities": {"61": 1}}`); rec.Code != http.StatusOK {
		t.Fatalf("pre-cuenta parcial: %d %s", rec.Code, rec.Body)
	}
	if job := fp.jobCount(t, 1)[0]; !bytes.Contains(job, []byte("1 Latte")) || !bytes.Contains(job, []byte("$100.00")) {
		t.Fatalf("la pre-cuenta debe traer solo una pieza:\n%s", job)
	}

	if rec := post(t, h.PayOrder, 600, `{"item_ids": [61], "quantities": {"61": 1}, "payments": [{"method": "CASH", "amount": 100}]}`); rec.Code != http.StatusCreated {
		t.Fatalf("cobrar 1 de 3: %d %s", rec.Code, rec.Body)
	}
	type row struct{ qty, printed, delivered, mods int }
	get := func(q string) (r row) {
		db.QueryRow(`SELECT quantity, printed_quantity, delivered_quantity,
			(SELECT COUNT(*) FROM order_item_modifiers WHERE order_item_id = oi.id) FROM order_items oi WHERE `+q).
			Scan(&r.qty, &r.printed, &r.delivered, &r.mods)
		return
	}
	// La pieza cobrada se lleva la entregada; el resto sigue pendiente, ya impreso.
	if paid := get(`bill_id IS NOT NULL`); paid != (row{1, 1, 1, 1}) {
		t.Fatalf("pieza cobrada: %+v", paid)
	}
	if rest := get(`id = 61`); rest != (row{2, 2, 0, 1}) {
		t.Fatalf("piezas pendientes: %+v", rest)
	}
	var total float64
	db.QueryRow(`SELECT total FROM bills`).Scan(&total)
	if total != 100 {
		t.Fatalf("la cuenta debe ser de una pieza, fue %v", total)
	}
	var status string
	db.QueryRow(`SELECT status FROM orders WHERE id = 600`).Scan(&status)
	if status != "ACTIVE" {
		t.Fatal("con piezas pendientes la orden sigue abierta")
	}

	// Las otras dos en otra cuenta: la orden se cierra.
	if rec := post(t, h.PayOrder, 600, `{"item_ids": [61], "payments": [{"method": "CARD_DEBIT", "amount": 200}]}`); rec.Code != http.StatusCreated {
		t.Fatalf("cobrar el resto: %d %s", rec.Code, rec.Body)
	}
	db.QueryRow(`SELECT status FROM orders WHERE id = 600`).Scan(&status)
	var pieces int
	db.QueryRow(`SELECT SUM(quantity) FROM order_items WHERE order_id = 600`).Scan(&pieces)
	if status != "PAID" || pieces != 3 {
		t.Fatalf("orden %s con %d piezas (deben seguir siendo 3)", status, pieces)
	}
}
