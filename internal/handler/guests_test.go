package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGuestsFromOrderToTicket(t *testing.T) {
	db, h := setupCashDB(t)
	fp := newFakePrinter(t)
	exec(t, db, `UPDATE printers SET is_active = 1, prints_receipts = 1, connection_type = 'NETWORK', ip_address = '127.0.0.1', port = ? WHERE id = 1`, fp.port())
	exec(t, db, `INSERT OR IGNORE INTO printer_areas (printer_id, area_id) SELECT 1, id FROM production_areas`)

	// Mesa 1: Juan pide un latte, la persona 2 (sin nombre) dos espressos,
	// y un latte para la mesa.
	body := `{"order_type": "DINE_IN", "table_id": 1, "guest_names": {"1": "Juan"}, "items": [
		{"product_id": 2, "quantity": 1, "guest": 1},
		{"product_id": 1, "quantity": 2, "guest": 2},
		{"product_id": 2, "quantity": 1}]}`
	if rec := post(t, h.AddToOrder, 0, body); rec.Code != http.StatusCreated {
		t.Fatalf("orden: %d %s", rec.Code, rec.Body)
	}
	comanda := string(fp.jobCount(t, 1)[0])
	iJuan, iP2, iMesa := strings.Index(comanda, "-- Juan --"), strings.Index(comanda, "-- Persona 2 --"), strings.Index(comanda, "-- Mesa --")
	if iJuan < 0 || iP2 < iJuan || iMesa < iP2 {
		t.Fatalf("la comanda debe ir en secciones por persona (Juan, Persona 2, Mesa):\n%s", comanda)
	}

	// La siguiente ronda ve a las personas de la mesa.
	rec := call(h.TableGuests, "GET", "/", "", nil, 1)
	var tg struct{ Guests []Guest }
	json.Unmarshal(rec.Body.Bytes(), &tg)
	if len(tg.Guests) != 2 || tg.Guests[0].Label != "Juan" || tg.Guests[1].Label != "Persona 2" {
		t.Fatalf("personas de la mesa: %s", rec.Body)
	}

	// Área con "una comanda por persona": tres comandas.
	exec(t, db, `INSERT OR REPLACE INTO print_formats (key, config) SELECT 'comanda:' || id, '{"guest_tickets": true, "copies": 1, "feed_lines": 1}' FROM production_areas`)
	var orderID int64
	db.QueryRow(`SELECT id FROM orders WHERE status = 'ACTIVE'`).Scan(&orderID)
	if rec := call(h.ReprintComanda, "POST", "/", "", nil, orderID); rec.Code != http.StatusOK {
		t.Fatalf("reimprimir: %d %s", rec.Code, rec.Body)
	}
	jobs := fp.jobCount(t, 4)
	// (el "·" va convertido al juego de caracteres de la impresora)
	if !bytes.Contains(jobs[1], []byte("Juan")) || !bytes.Contains(jobs[2], []byte("Persona 2")) ||
		bytes.Contains(jobs[1], []byte("Espresso")) || !bytes.Contains(jobs[3], []byte(" Mesa\n")) {
		t.Fatalf("una comanda por persona debe decir de quién es:\n%s\n%s", jobs[1], jobs[3])
	}

	// Expo: el nombre de cada persona sobre sus platillos.
	out := httptest.NewRecorder()
	NewUIHandler(h).GetExpoHTML(out, httptest.NewRequest("GET", "/api/expo/html?type=DINE_IN", nil))
	if html := out.Body.String(); !strings.Contains(html, ">Juan</li>") || !strings.Contains(html, ">Persona 2</li>") {
		t.Fatal("Expo debe agrupar por persona")
	}

	// Mover un espresso de la persona 2 a Juan (parte el renglón).
	var p2Item int64
	db.QueryRow(`SELECT oi.id FROM order_items oi JOIN order_guests g ON g.id = oi.guest_id WHERE g.position = 2`).Scan(&p2Item)
	if rec := call(h.MoveItemToGuest, "PUT", "/", `{"position": 1, "quantity": 1}`, nil, p2Item); rec.Code != http.StatusOK {
		t.Fatalf("mover: %d %s", rec.Code, rec.Body)
	}
	var juanPieces int
	db.QueryRow(`SELECT SUM(oi.quantity) FROM order_items oi JOIN order_guests g ON g.id = oi.guest_id WHERE g.position = 1`).Scan(&juanPieces)
	if juanPieces != 2 {
		t.Fatalf("Juan debe tener 2 piezas, tiene %d", juanPieces)
	}

	// Cobrar solo lo de Juan: el ticket dice "Cuenta de Juan".
	rec = call(h.GetCheckout, "GET", "/", "", nil, orderID)
	var co CheckoutData
	json.Unmarshal(rec.Body.Bytes(), &co)
	var ids []int64
	var total float64
	for _, it := range co.Items {
		if it.Guest == "Juan" {
			ids = append(ids, it.ID)
			total += it.UnitPrice * float64(it.Quantity)
		}
	}
	if len(co.Guests) != 2 || len(ids) != 2 {
		t.Fatalf("cobro: personas %+v, productos de Juan %v", co.Guests, ids)
	}
	post(t, h.OpenCash, 0, `{"pin": "0001", "opening_float": 0}`)
	idsJSON, _ := json.Marshal(ids)
	totalJSON, _ := json.Marshal(total)
	if rec := post(t, h.PayOrder, orderID, `{"item_ids": `+string(idsJSON)+`, "payments": [{"method": "CASH", "amount": `+string(totalJSON)+`}]}`); rec.Code != http.StatusCreated {
		t.Fatalf("cobrar a Juan: %d %s", rec.Code, rec.Body)
	}
	var billID int64
	db.QueryRow(`SELECT id FROM bills`).Scan(&billID)
	if rec := post(t, h.PrintBill, billID, ""); rec.Code != http.StatusOK {
		t.Fatalf("ticket: %d", rec.Code)
	}
	if ticket := fp.jobCount(t, 5)[4]; !bytes.Contains(ticket, []byte("Cuenta de")) || !bytes.Contains(ticket, []byte("Juan")) {
		t.Fatalf("el ticket debe decir de quién es la cuenta:\n%s", ticket)
	}
}
