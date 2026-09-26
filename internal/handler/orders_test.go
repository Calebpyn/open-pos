package handler

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestAddToOrderIsIdempotent(t *testing.T) {
	db := setupComandaDB(t)
	h := NewPOSHandler(db)
	fp := newFakePrinter(t)
	exec(t, db, `UPDATE printers SET is_active = 1, port = ? WHERE id = 1`, fp.port())
	exec(t, db, `UPDATE printers SET is_active = 0 WHERE id = 2`)

	body := `{"client_ref": "abc-123", "order_type": "DINE_IN", "table_id": 1,
		"items": [{"product_id": 1, "quantity": 2}]}`
	first := post(t, h.AddToOrder, 0, body)
	if first.Code != http.StatusCreated {
		t.Fatalf("primer envío: %d %s", first.Code, first.Body)
	}
	// El mismo envío otra vez (doble toque): no agrega nada ni imprime.
	second := post(t, h.AddToOrder, 0, body)
	var out struct {
		Duplicate bool  `json:"duplicate"`
		OrderID   int64 `json:"order_id"`
	}
	json.Unmarshal(second.Body.Bytes(), &out)
	if second.Code != http.StatusOK || !out.Duplicate || out.OrderID == 0 {
		t.Fatalf("segundo envío: %d %s", second.Code, second.Body)
	}
	var qty int
	db.QueryRow(`SELECT SUM(quantity) FROM order_items WHERE order_id = ?`, out.OrderID).Scan(&qty)
	if qty != 2 {
		t.Fatalf("la orden debe tener 2 piezas, tiene %d", qty)
	}
	fp.jobCount(t, 1) // una sola comanda

	// Otro envío (otra referencia) sí se agrega a la misma mesa.
	post(t, h.AddToOrder, 0, `{"client_ref": "abc-124", "order_type": "DINE_IN", "table_id": 1, "items": [{"product_id": 1, "quantity": 1}]}`)
	db.QueryRow(`SELECT SUM(quantity) FROM order_items WHERE order_id = ?`, out.OrderID).Scan(&qty)
	if qty != 3 {
		t.Fatalf("un envío nuevo debe agregarse, hay %d piezas", qty)
	}
}
