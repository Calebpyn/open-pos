package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestWaiterTerminal(t *testing.T) {
	db, h := setupCashDB(t)
	ui := NewUIHandler(h)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", ui.ServeIndex)
	mux.HandleFunc("GET /caja", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("caja")) })
	mux.HandleFunc("GET /api/terminal/me", h.TerminalMe)
	mux.HandleFunc("POST /api/terminal/login", h.TerminalLogin)
	mux.HandleFunc("POST /api/orders/add", h.AddToOrder)
	mux.HandleFunc("POST /api/orders/{id}/pay", h.PayOrder)
	mux.HandleFunc("POST /api/order-items/{id}/deliver", h.DeliverItem)
	mux.HandleFunc("GET /api/admin/terminals", h.AdminListTerminals)
	srv := h.TerminalGuard(mux)

	var cookie *http.Cookie
	do := func(method, path, body, remote string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.RemoteAddr = remote
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		for _, c := range rec.Result().Cookies() {
			if c.Name == terminalCookie {
				cookie = c
			}
		}
		return rec
	}
	const ipad = "192.168.1.50:5000"

	// Peticiones sueltas sin cookie (vistas previas) no registran terminales.
	do("GET", "/api/terminal/me", "", ipad)
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM terminals`).Scan(&n)
	if n != 0 || cookie != nil {
		t.Fatalf("solo abrir el POS registra una terminal (hay %d)", n)
	}

	// La primera visita registra el iPad como terminal de mesero y pide PIN.
	if rec := do("GET", "/", "", ipad); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "¿Quién eres?") {
		t.Fatalf("la página del iPad debe pedir PIN: %d", rec.Code)
	}
	if cookie == nil {
		t.Fatal("el iPad debe recibir su cookie de terminal")
	}
	order := `{"order_type": "DINE_IN", "table_id": 1, "items": [{"product_id": 2, "quantity": 1}]}`
	if rec := do("POST", "/api/orders/add", order, ipad); rec.Code != http.StatusUnauthorized {
		t.Fatalf("sin PIN no se toman órdenes, dio %d", rec.Code)
	}
	if rec := do("GET", "/caja", "", ipad); rec.Code != http.StatusSeeOther {
		t.Fatalf("la caja no se abre desde el iPad (redirige), dio %d", rec.Code)
	}
	if rec := do("GET", "/api/admin/terminals", "", ipad); rec.Code != http.StatusForbidden {
		t.Fatalf("el admin no se usa desde el iPad, dio %d", rec.Code)
	}

	if rec := do("POST", "/api/terminal/login", `{"pin": "0001"}`, ipad); rec.Code != http.StatusOK {
		t.Fatalf("entrar con PIN: %d %s", rec.Code, rec.Body)
	}
	rec := do("GET", "/api/terminal/me", "", ipad)
	var me Terminal
	json.Unmarshal(rec.Body.Bytes(), &me)
	if me.Kind != TerminalWaiter || me.UserName != "Usuario general" {
		t.Fatalf("terminal: %+v", me)
	}
	page := do("GET", "/", "", ipad).Body.String()
	if strings.Contains(page, "¿Quién eres?") || strings.Contains(page, `href="/caja"`) || strings.Contains(page, "Cobrar ahora") ||
		!strings.Contains(page, "Usuario general") {
		t.Fatal("con sesión, el iPad ve la vista de mesero con su nombre, sin caja ni cobro")
	}

	// Toma la orden: queda quién la tomó. Flash y cobrar, no.
	if rec := do("POST", "/api/orders/add", order, ipad); rec.Code != http.StatusCreated {
		t.Fatalf("orden desde el iPad: %d %s", rec.Code, rec.Body)
	}
	var itemID, addedBy int64
	db.QueryRow(`SELECT id, added_by FROM order_items ORDER BY id DESC LIMIT 1`).Scan(&itemID, &addedBy)
	if addedBy == 0 {
		t.Fatal("debe guardarse quién tomó el producto")
	}
	if rec := do("POST", "/api/orders/add", `{"order_type": "FLASH", "items": [{"product_id": 2, "quantity": 1}]}`, ipad); rec.Code != http.StatusForbidden {
		t.Fatalf("la venta de mostrador no se hace desde el iPad, dio %d", rec.Code)
	}
	if rec := do("POST", "/api/orders/1/pay", `{}`, ipad); rec.Code != http.StatusForbidden {
		t.Fatalf("no se cobra desde el iPad, dio %d", rec.Code)
	}
	if rec := do("POST", "/api/order-items/"+strconv.FormatInt(itemID, 10)+"/deliver", "", ipad); rec.Code != http.StatusOK {
		t.Fatalf("entregar: %d", rec.Code)
	}
	var deliveredBy int64
	db.QueryRow(`SELECT delivered_by FROM order_items WHERE id = ?`, itemID).Scan(&deliveredBy)
	if deliveredBy != addedBy {
		t.Fatalf("debe guardarse quién entregó (%d)", deliveredBy)
	}

	// Con acceso de caja (lo da el admin), el mismo dispositivo sí cobra.
	exec(t, db, `UPDATE terminals SET kind = 'FULL'`)
	if rec := do("POST", "/api/orders/1/pay", `{}`, ipad); rec.Code == http.StatusForbidden {
		t.Fatal("una terminal con acceso de caja sí puede cobrar")
	}
	// La compu central no necesita cookie ni PIN.
	cookie = nil
	if rec := do("GET", "/caja", "", "127.0.0.1:6000"); rec.Code != http.StatusOK {
		t.Fatalf("la compu central tiene acceso completo, dio %d", rec.Code)
	}
}

func TestTailscaleRemoteAdmin(t *testing.T) {
	_, h := setupCashDB(t)
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }
	mux.HandleFunc("GET /{$}", ok)
	mux.HandleFunc("GET /admin/reportes", ok)
	mux.HandleFunc("GET /api/admin/reports", ok)
	mux.HandleFunc("POST /api/orders/add", ok)
	mux.HandleFunc("GET /caja", ok)
	srv := h.TerminalGuard(mux)
	do := func(method, path string) int {
		req := httptest.NewRequest(method, path, nil)
		req.RemoteAddr = "100.101.102.103:4000"
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec.Code
	}
	if do("GET", "/admin/reportes") != 200 || do("GET", "/api/admin/reports") != 200 {
		t.Fatal("por Tailscale se entra al admin")
	}
	if do("GET", "/") != http.StatusSeeOther || do("GET", "/caja") != http.StatusSeeOther {
		t.Fatal("el POS y la caja mandan al admin")
	}
	if do("POST", "/api/orders/add") != http.StatusForbidden {
		t.Fatal("por Tailscale no se toman órdenes")
	}
}
