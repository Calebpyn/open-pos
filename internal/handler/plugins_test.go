package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/calebpyn/open-pos/internal/plugins"
)

type testEvent struct {
	Type    string
	Payload map[string]any
}

// eventsAfter regresa los eventos registrados después de seq.
func eventsAfter(t *testing.T, db *sql.DB, seq int64) ([]testEvent, int64) {
	t.Helper()
	evs, err := plugins.ReadEvents(context.Background(), db, seq, 500, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]testEvent, len(evs))
	for i, e := range evs {
		out[i].Type = e.Type
		json.Unmarshal(e.Payload, &out[i].Payload)
		seq = e.Seq
	}
	return out, seq
}

func eventTypes(evs []testEvent) string {
	var s []string
	for _, e := range evs {
		s = append(s, e.Type)
	}
	return strings.Join(s, ",")
}

func TestPluginEventsFollowTheSale(t *testing.T) {
	db, h := setupCashDB(t)
	exec(t, db, `INSERT INTO modifier_groups (id, name) VALUES (900, 'Leche')`)
	exec(t, db, `INSERT INTO modifier_options (id, modifier_group_id, name, price_extra) VALUES (901, 900, 'Almendra', 10)`)
	exec(t, db, `INSERT INTO product_modifier_groups (product_id, modifier_group_id) VALUES (1, 900)`)
	if rec := post(t, h.OpenCash, 0, `{"pin": "0001", "opening_float": 500}`); rec.Code != http.StatusCreated {
		t.Fatalf("abrir turno: %d %s", rec.Code, rec.Body)
	}
	_, seq := eventsAfter(t, db, 0)

	// Comanda: 2 espressos con almendra ($55 c/u) y un latte ($65).
	rec := post(t, h.AddToOrder, 0, `{"order_type": "DINE_IN", "table_id": 1, "items": [
		{"product_id": 1, "quantity": 2, "modifier_ids": [901]}, {"product_id": 2, "quantity": 1}]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("comanda: %d %s", rec.Code, rec.Body)
	}
	evs, seq := eventsAfter(t, db, seq)
	if eventTypes(evs) != "items.added" {
		t.Fatalf("eventos de la comanda: %s", eventTypes(evs))
	}
	items := evs[0].Payload["items"].([]any)
	first := items[0].(map[string]any)
	mods := first["modifiers"].([]any)
	if len(items) != 2 || first["quantity"].(float64) != 2 || first["unit_price"].(float64) != 55 ||
		len(mods) != 1 || mods[0].(map[string]any)["option_id"].(float64) != 901 || evs[0].Payload["order_type"] != "DINE_IN" {
		t.Fatalf("items.added: %+v", evs[0].Payload)
	}
	orderID := int64(evs[0].Payload["order_id"].(float64))
	espresso, latte := int64(first["item_id"].(float64)), int64(items[1].(map[string]any)["item_id"].(float64))

	// Se cobra solo 1 de los 2 espressos: el renglón se parte.
	rec = post(t, h.PayOrder, orderID, fmt.Sprintf(`{"item_ids": [%d], "quantities": {"%d": 1},
		"payments": [{"method": "CASH", "amount": 55}]}`, espresso, espresso))
	if rec.Code != http.StatusCreated {
		t.Fatalf("cobro parcial: %d %s", rec.Code, rec.Body)
	}
	evs, seq = eventsAfter(t, db, seq)
	if eventTypes(evs) != "item.split,bill.paid" {
		t.Fatalf("eventos del cobro parcial: %s", eventTypes(evs))
	}
	split, bill := evs[0].Payload, evs[1].Payload
	paid := bill["items"].([]any)
	if split["item_id"].(float64) != float64(espresso) || split["quantity"].(float64) != 1 || split["remaining"].(float64) != 1 ||
		len(paid) != 1 || paid[0].(map[string]any)["item_id"] != split["new_item_id"] || bill["total"].(float64) != 55 {
		t.Fatalf("split %+v / bill %+v", split, bill)
	}

	// Expo entrega el latte; luego se cobra el resto y la orden se cierra.
	if rec := post(t, h.DeliverItem, latte, ``); rec.Code != http.StatusOK {
		t.Fatalf("entrega: %d", rec.Code)
	}
	rec = post(t, h.PayOrder, orderID, fmt.Sprintf(`{"item_ids": [%d, %d], "payments": [{"method": "CARD_DEBIT", "amount": 120}]}`, espresso, latte))
	if rec.Code != http.StatusCreated {
		t.Fatalf("cobro final: %d %s", rec.Code, rec.Body)
	}
	evs, seq = eventsAfter(t, db, seq)
	if eventTypes(evs) != "items.delivered,bill.paid,order.closed" {
		t.Fatalf("eventos del cierre: %s", eventTypes(evs))
	}
	if d := evs[0].Payload["items"].([]any)[0].(map[string]any); d["item_id"].(float64) != float64(latte) || d["delta"].(float64) != 1 {
		t.Fatalf("entrega: %+v", evs[0].Payload)
	}
	if closed := evs[2].Payload; closed["status"] != "PAID" || len(closed["unpaid_items"].([]any)) != 0 {
		t.Fatalf("order.closed: %+v", closed)
	}

	// Otra orden que se anula completa: se cancela sin venta.
	post(t, h.AddToOrder, 0, `{"order_type": "DINE_IN", "table_id": 2, "items": [{"product_id": 3, "quantity": 1}]}`)
	evs, seq = eventsAfter(t, db, seq)
	item := int64(evs[0].Payload["items"].([]any)[0].(map[string]any)["item_id"].(float64))
	if rec := post(t, h.VoidItem, item, `{"reason": "Se equivocó el mesero"}`); rec.Code != http.StatusOK {
		t.Fatalf("anular: %d %s", rec.Code, rec.Body)
	}
	evs, seq = eventsAfter(t, db, seq)
	if eventTypes(evs) != "item.voided,order.closed" || evs[0].Payload["reason"] != "Se equivocó el mesero" ||
		evs[1].Payload["status"] != "CANCELLED" {
		t.Fatalf("anulación: %s %+v", eventTypes(evs), evs)
	}

	// Cambios al catálogo desde el panel.
	cookie := adminLogin(t, h, "0001")
	if rec := call(h.RequireAdmin(h.AdminSetAvailability), "POST", "/", `{"available": false}`, cookie, 3); rec.Code != http.StatusOK {
		t.Fatalf("agotar: %d %s", rec.Code, rec.Body)
	}
	evs, _ = eventsAfter(t, db, seq)
	if eventTypes(evs) != "catalog.changed" || evs[0].Payload["action"] != "PRODUCT_AVAILABILITY" {
		t.Fatalf("catálogo: %s %+v", eventTypes(evs), evs)
	}

	// Una operación rechazada no deja eventos.
	_, before := eventsAfter(t, db, 0)
	if rec := post(t, h.VoidItem, item, `{"reason": "otra vez"}`); rec.Code != http.StatusConflict {
		t.Fatalf("anular dos veces: %d", rec.Code)
	}
	if _, after := eventsAfter(t, db, 0); after != before {
		t.Fatal("una operación fallida no debe dejar eventos")
	}
}

// devPlugin arranca un Manager con un plugin "en desarrollo" (no se lanza
// ningún proceso) y regresa su token y su socket.
func devPlugin(t *testing.T, h *POSHandler, perms []string) (token, socket string) {
	t.Helper()
	dir := t.TempDir()
	b, _ := json.Marshal(map[string]any{"id": "prueba", "name": "Prueba", "version": "1.0.0", "api_version": 1,
		"executable": "prueba", "permissions": perms, "admin": map[string]string{"label": "🧪 Prueba"}})
	os.WriteFile(filepath.Join(dir, "plugin.json"), b, 0o644)
	runDir, err := os.MkdirTemp("/tmp", "opos")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(runDir) })
	ctx, cancel := context.WithCancel(context.Background())
	m := plugins.New(plugins.Config{DevDirs: []string{dir}, RunDir: runDir, POSVersion: "dev", DB: h.DB,
		Dir: filepath.Join(dir, "nada")})
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); m.Wait() })
	h.Plugins = m
	p, _ := m.Get("prueba")
	return p.Token(), filepath.Join(runDir, "prueba.sock")
}

// pluginMux arma las rutas de plugins junto a la raíz del POS, como en
// cmd/pos, para que un patrón que choque truene aquí y no al arrancar.
func pluginMux(h *POSHandler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(http.ResponseWriter, *http.Request) {})
	mux.HandleFunc("GET /admin/{section}", func(http.ResponseWriter, *http.Request) {})
	h.RegisterPluginRoutes(mux)
	return mux
}

func pluginCall(h *POSHandler, method, path, token, remote, body string) *httptest.ResponseRecorder {
	mux := pluginMux(h)
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = remote
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestPluginAPIAuth(t *testing.T) {
	_, h := setupCashDB(t)
	token, _ := devPlugin(t, h, []string{plugins.PermEvents, plugins.PermCatalog})
	local := "127.0.0.1:5555"

	if rec := pluginCall(h, "GET", "/api/plugin/v1/info", "", local, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("sin token: %d", rec.Code)
	}
	if rec := pluginCall(h, "GET", "/api/plugin/v1/info", "inventado", local, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("token inventado: %d", rec.Code)
	}
	if rec := pluginCall(h, "GET", "/api/plugin/v1/info", token, "192.168.1.20:5555", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("desde otra máquina, aun con token: %d", rec.Code)
	}
	rec := pluginCall(h, "GET", "/api/plugin/v1/info", token, local, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"plugin_id":"prueba"`) {
		t.Fatalf("info: %d %s", rec.Code, rec.Body)
	}
	if rec := pluginCall(h, "GET", "/api/plugin/v1/catalog", token, local, ""); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), "Espresso Doble") {
		t.Fatalf("catálogo: %d", rec.Code)
	}
	// No pidió products:availability.
	if rec := pluginCall(h, "POST", "/api/plugin/v1/products/1/availability", token, local, `{"available": false}`); rec.Code != http.StatusForbidden {
		t.Fatalf("sin permiso: %d", rec.Code)
	}

	// Leer eventos guarda hasta dónde va el plugin.
	exec(t, h.DB, `INSERT INTO plugin_events (type, payload) VALUES ('a', '{}'), ('b', '{}')`)
	rec = pluginCall(h, "GET", "/api/plugin/v1/events?after=1", token, local, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"type":"b"`) || strings.Contains(rec.Body.String(), `"type":"a"`) {
		t.Fatalf("eventos: %d %s", rec.Code, rec.Body)
	}
	var cursor int64
	h.DB.QueryRow(`SELECT cursor FROM plugins WHERE id = 'prueba'`).Scan(&cursor)
	if cursor != 1 {
		t.Fatalf("cursor guardado: %d", cursor)
	}
	if rec := pluginCall(h, "GET", "/api/plugin/v1/events?after=-3", token, local, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("after negativo: %d", rec.Code)
	}
}

func TestPluginSetAvailability(t *testing.T) {
	_, h := setupCashDB(t)
	token, _ := devPlugin(t, h, []string{plugins.PermAvailability})
	rec := pluginCall(h, "POST", "/api/plugin/v1/products/2/availability", token, "127.0.0.1:1", `{"available": false, "reason": "Sin leche"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("agotar: %d %s", rec.Code, rec.Body)
	}
	var available bool
	h.DB.QueryRow(`SELECT is_available FROM products WHERE id = 2`).Scan(&available)
	action, details := lastAudit(t, h.DB)
	if available || action != "PRODUCT_AVAILABILITY" || details["plugin"] != "prueba" || details["reason"] != "Sin leche" {
		t.Fatalf("disponible=%v, bitácora %s %+v", available, action, details)
	}
	if rec := pluginCall(h, "POST", "/api/plugin/v1/products/999/availability", token, "127.0.0.1:1", `{"available": true}`); rec.Code != http.StatusNotFound {
		t.Fatalf("producto inexistente: %d", rec.Code)
	}
}

func TestPluginProxy(t *testing.T) {
	_, h := setupCashDB(t)
	token, socket := devPlugin(t, h, nil)
	t.Chdir("../..") // plantillas

	// El plugin "en desarrollo": responde lo que recibió.
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		info := fmt.Sprintf("%s %s user=%s role=%s base=%s cookie=%q spoof=%q body=%s",
			r.Method, r.URL.Path, r.Header.Get("X-Opos-User-Name"), r.Header.Get("X-Opos-User-Role"),
			r.Header.Get("X-Opos-Base-Path"), r.Header.Get("Cookie"), r.Header.Get("X-Opos-Spoof"), body)
		if r.URL.Path == "/pagina" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("X-Opos-Layout", "admin")
			w.Header().Set("X-Opos-Title", "Página de prueba")
			w.Header().Set("Set-Cookie", "robada=1")
			fmt.Fprintf(w, "<p id=plugin>%s</p>", info)
			return
		}
		w.Write([]byte(info))
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	cookie := adminLogin(t, h, "0001")
	mux := pluginMux(h)
	do := func(method, path, body string, withCookie bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("X-Opos-Spoof", "admin")
		if withCookie {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	if rec := do("GET", "/admin/plugins/prueba/algo", "", false); rec.Code != http.StatusSeeOther {
		t.Fatalf("sin sesión debe pedir login: %d", rec.Code)
	}
	rec := do("POST", "/admin/plugins/prueba/api/guardar?x=1", "dato=1", true)
	got := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.HasPrefix(got, "POST /api/guardar user=Usuario general role=ADMIN base=/admin/plugins/prueba") ||
		!strings.Contains(got, `cookie=""`) || !strings.Contains(got, `spoof=""`) || !strings.HasSuffix(got, "body=dato=1") {
		t.Fatalf("proxy: %d %s", rec.Code, got)
	}

	// Una página con X-Opos-Layout sale dentro del panel.
	rec = do("GET", "/admin/plugins/prueba/pagina", "", true)
	page := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(page, "<title>Página de prueba · Admin</title>") ||
		!strings.Contains(page, "Administración") || !strings.Contains(page, "<p id=plugin>GET /pagina") ||
		!strings.Contains(page, "🧪 Prueba") || rec.Header().Get("Set-Cookie") != "" || rec.Header().Get("X-Opos-Layout") != "" {
		t.Fatalf("página: %d %v\n%s", rec.Code, rec.Header(), page[:min(len(page), 600)])
	}

	if rec := do("GET", "/admin/plugins/prueba/_opos/health", "", true); rec.Code != http.StatusNotFound {
		t.Fatalf("las rutas internas no se exponen: %d", rec.Code)
	}
	if rec := do("GET", "/admin/plugins/otro/", "", true); rec.Code != http.StatusNotFound {
		t.Fatalf("plugin inexistente: %d", rec.Code)
	}

	// Plugin caído: error claro, no se cuelga.
	srv.Close()
	if rec := do("GET", "/admin/plugins/prueba/", "", true); rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "no responde") {
		t.Fatalf("plugin caído: %d %s", rec.Code, rec.Body)
	}
}

func TestAdminNavIncludesPlugins(t *testing.T) {
	_, h := setupCashDB(t)
	devPlugin(t, h, nil)
	cookie := adminLogin(t, h, "0001")
	t.Chdir("../..")
	req := httptest.NewRequest("GET", "/admin/plugins", nil)
	req.SetPathValue("section", "plugins")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.RequireAdmin(NewUIHandler(h).ServeAdminPage)(rec, req)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `href="/admin/plugins/prueba"`) || !strings.Contains(body, "🧩 Plugins") {
		t.Fatalf("nav: %d", rec.Code)
	}
	rec = call(h.RequireAdmin(h.AdminListPlugins), "GET", "/", "", cookie, 0)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"state":"esperando (desarrollo)"`) {
		t.Fatalf("lista: %d %s", rec.Code, rec.Body)
	}
}
