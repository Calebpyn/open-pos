package handler

import (
	"bytes"
	"net/http"
	"testing"
)

func TestEditOrderItem(t *testing.T) {
	db, h := setupCashDB(t)
	fp := newFakePrinter(t)
	exec(t, db, `UPDATE printers SET is_active = 1, connection_type = 'NETWORK', ip_address = '127.0.0.1', port = ? WHERE id = 1`, fp.port())
	exec(t, db, `INSERT OR IGNORE INTO printer_areas (printer_id, area_id) SELECT 1, id FROM production_areas`)
	exec(t, db, `INSERT INTO table_sessions (id, table_id, status) VALUES (600, 1, 'OPEN')`)
	exec(t, db, `INSERT INTO orders (id, session_id, order_type, status) VALUES (600, 600, 'DINE_IN', 'ACTIVE')`)
	exec(t, db, `INSERT INTO order_items (id, order_id, product_id, unit_price, quantity, printed_quantity, delivered_quantity, delivered_at)
		VALUES (61, 600, 2, 65, 1, 1, 1, CURRENT_TIMESTAMP), (62, 600, 1, 45, 1, 1, 0, NULL)`)
	var espresso float64
	db.QueryRow(`SELECT price FROM products WHERE id = 1`).Scan(&espresso)

	put := func(id int64, body string) (int, string) {
		rec := call(h.EditOrderItem, "PUT", "/", body, nil, id)
		return rec.Code, rec.Body.String()
	}
	if code, _ := put(61, `{"product_id": 1, "quantity": 1}`); code != http.StatusUnauthorized {
		t.Fatalf("sin PIN de caja no se edita, dio %d", code)
	}
	if code, _ := put(61, `{"pin": "0001", "product_id": 2, "quantity": 1}`); code != http.StatusBadRequest {
		t.Fatalf("sin cambios debe decirlo, dio %d", code)
	}

	// Latte (ya entregado) → Espresso: precio del catálogo, se vuelve a
	// preparar y la comanda lleva la línea del cambio.
	if code, body := put(61, `{"pin": "0001", "product_id": 1, "quantity": 1, "notes": "cortito"}`); code != http.StatusOK {
		t.Fatalf("editar: %d %s", code, body)
	}
	var product, printed, delivered int
	var price float64
	db.QueryRow(`SELECT product_id, unit_price, printed_quantity, delivered_quantity FROM order_items WHERE id = 61`).
		Scan(&product, &price, &printed, &delivered)
	if product != 1 || price != espresso || printed != 1 || delivered != 0 {
		t.Fatalf("renglón editado: producto %d precio %v impreso %d entregado %d", product, price, printed, delivered)
	}
	job := fp.jobCount(t, 1)[0]
	for _, want := range []string{"1 x Espresso Doble", "cortito", "(cambio: antes 1 x Latte"} {
		if !bytes.Contains(job, []byte(want)) {
			t.Fatalf("la comanda de la corrección debe incluir %q:\n%s", want, job)
		}
	}
	if action, d := lastAudit(t, db); action != "ITEM_EDITED" || d["user"] != "Usuario general" || d["before"] == nil || d["after"] == nil {
		t.Fatalf("bitácora: %s %v", action, d)
	}

	// Solo cantidad 1 → 3: las 2 nuevas salen como adicional.
	if code, body := put(62, `{"pin": "0001", "product_id": 1, "quantity": 3}`); code != http.StatusOK {
		t.Fatalf("más piezas: %d %s", code, body)
	}
	if job := fp.jobCount(t, 2)[1]; !bytes.Contains(job, []byte("ADICIONAL")) || !bytes.Contains(job, []byte("2 x Espresso Doble")) {
		t.Fatalf("las piezas nuevas deben salir como adicional:\n%s", job)
	}
	// 3 → 1: se avisa que ya no van 2.
	if code, body := put(62, `{"pin": "0001", "product_id": 1, "quantity": 1}`); code != http.StatusOK {
		t.Fatalf("menos piezas: %d %s", code, body)
	}
	if job := fp.jobCount(t, 3)[2]; !bytes.Contains(job, []byte("YA NO VA: 2 x Espresso Doble")) {
		t.Fatalf("debe avisar las piezas que ya no van:\n%s", job)
	}

	// Agregar productos a la orden desde la pantalla de la orden.
	if rec := call(h.AddItemsToOrder, "POST", "/", `{"pin": "0001", "items": [{"product_id": 2, "quantity": 2}]}`, nil, 600); rec.Code != http.StatusOK {
		t.Fatalf("agregar: %d %s", rec.Code, rec.Body)
	}
	if job := fp.jobCount(t, 4)[3]; !bytes.Contains(job, []byte("2 x Latte")) {
		t.Fatalf("lo agregado debe salir en comanda:\n%s", job)
	}
	if action, _ := lastAudit(t, db); action != "ORDER_ITEMS_ADDED" {
		t.Fatalf("agregar debe quedar en la bitácora, quedó %s", action)
	}

	// Lo cobrado ya no se edita.
	exec(t, db, `INSERT INTO bills (id, order_id, status) VALUES (700, 600, 'PAID')`)
	exec(t, db, `UPDATE order_items SET bill_id = 700 WHERE id = 62`)
	if code, _ := put(62, `{"pin": "0001", "product_id": 2, "quantity": 1}`); code != http.StatusConflict {
		t.Fatalf("un producto cobrado no se edita, dio %d", code)
	}
}
