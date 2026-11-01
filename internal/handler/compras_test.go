package handler

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/calebpyn/open-pos/internal/ocr"
)

// comprasEnv arma las rutas de Compras como en cmd/pos.
type comprasEnv struct {
	t      *testing.T
	db     *sql.DB
	h      *POSHandler
	mux    *http.ServeMux
	cookie *http.Cookie // sesión de admin
}

func newComprasEnv(t *testing.T) *comprasEnv {
	t.Helper()
	db, h := setupCashDB(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(http.ResponseWriter, *http.Request) {})
	mux.HandleFunc("GET /admin/{section}", h.RequireAdmin(NewUIHandler(h).ServeAdminPage))
	h.RegisterComprasRoutes(mux)
	t.Chdir("../..") // plantillas
	return &comprasEnv{t: t, db: db, h: h, mux: mux, cookie: adminLogin(t, h, "0001")}
}

var central = Terminal{Kind: TerminalFull, Central: true, Name: "Compu central"}

func waiter(id int64, name, role string) Terminal {
	return Terminal{ID: 50 + id, Kind: TerminalWaiter, UserID: id, UserName: name, UserRole: role}
}

func (e *comprasEnv) do(term Terminal, method, path string, form url.Values) *httptest.ResponseRecorder {
	e.t.Helper()
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.AddCookie(e.cookie)
	req = req.WithContext(context.WithValue(req.Context(), ctxTerminal, term))
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

// ok hace la petición y exige 200 (o 303 para formularios normales).
func (e *comprasEnv) ok(term Terminal, method, path string, form url.Values) string {
	e.t.Helper()
	rec := e.do(term, method, path, form)
	if rec.Code != http.StatusOK && rec.Code != http.StatusSeeOther {
		e.t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body.String()[:min(rec.Body.Len(), 500)])
	}
	return rec.Body.String()
}

func (e *comprasEnv) id(q string, args ...any) int64 {
	e.t.Helper()
	var id int64
	if err := e.db.QueryRow(q, args...).Scan(&id); err != nil {
		e.t.Fatalf("%s: %v", q, err)
	}
	return id
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

func has(t *testing.T, body string, parts ...string) {
	t.Helper()
	for _, p := range parts {
		if !strings.Contains(body, p) {
			t.Fatalf("falta %q en:\n%s", p, body[:min(len(body), 3000)])
		}
	}
}

func TestComprasFlow(t *testing.T) {
	e := newComprasEnv(t)
	exec(t, e.db, `INSERT INTO users (id, name, role, pin_salt, pin_hash) VALUES
		(70, 'Ana', 'WAITER', 'x', 'y'), (71, 'Beto', 'WAITER', 'x', 'y'), (72, 'Caro', 'CASHIER', 'x', 'y')`)
	ana, beto, caro := waiter(70, "Ana", RoleWaiter), waiter(71, "Beto", RoleWaiter), waiter(72, "Caro", RoleCashier)

	// Catálogo desde el panel.
	e.ok(central, "POST", "/admin/compras/lugares", url.Values{"name": {"Costco"}, "sort_order": {"2"}})
	e.ok(central, "POST", "/admin/compras/lugares", url.Values{"name": {"Mercado Hidalgo"}, "kind": {"MERCADO"}, "sort_order": {"1"}})
	if rec := e.do(central, "POST", "/admin/compras/lugares", url.Values{"name": {"COSTCO "}}); !strings.Contains(rec.Header().Get("Location"), "error=") {
		t.Fatal("un lugar repetido (sin importar mayúsculas) debe rechazarse")
	}
	costco := itoa(e.id(`SELECT id FROM compras_places WHERE name = 'Costco'`))
	mercado := itoa(e.id(`SELECT id FROM compras_places WHERE name = 'Mercado Hidalgo'`))
	e.ok(central, "POST", "/admin/compras/insumos", url.Values{"name": {"Leche entera"}, "unit": {"1 L"}, "location": {"Refri"},
		"place": {costco, mercado}, "brand": {"Lala", ""}, "price": {"25", "$27.50"}})
	e.ok(central, "POST", "/admin/compras/insumos", url.Values{"name": {"Leche deslactosada"}, "location": {"Refri"}, "place": {costco}, "price": {"29"}})
	e.ok(central, "POST", "/admin/compras/insumos", url.Values{"name": {"Café en grano"}, "unit": {"1 kg"}, "location": {"Alacena"},
		"place": {mercado}, "price": {"380"}})
	leche := itoa(e.id(`SELECT id FROM compras_supplies WHERE name = 'Leche entera'`))
	deslac := itoa(e.id(`SELECT id FROM compras_supplies WHERE name = 'Leche deslactosada'`))
	cafe := itoa(e.id(`SELECT id FROM compras_supplies WHERE name = 'Café en grano'`))
	// Prioridad: primero el mercado (aunque Costco es más barato), con su porqué.
	e.ok(central, "POST", "/admin/compras/insumos/"+leche, url.Values{"name": {"Leche entera"}, "unit": {"1 L"}, "location": {"Refri"},
		"active": {"1"}, "place": {mercado, costco}, "brand": {"", "Lala"}, "price": {"27.50", "25"},
		"source_note": {"Queda junto al café", "Solo si no hay en el mercado"}, "substitute": {deslac}})
	if rank := e.id(`SELECT rank FROM compras_sources WHERE supply_id = ? AND place_id = ?`, leche, mercado); rank != 1 {
		t.Fatalf("el orden del formulario es la prioridad: mercado quedó %d", rank)
	}
	list := e.ok(central, "GET", "/admin/compras", nil)
	has(t, list, "🛒 Compras", `"name":"Leche entera"`, `"places":["Mercado Hidalgo","Costco"]`,
		`"price":2750`, `"cheaper":"Costco ($25.00 · $25.00/L)"`, `"pack":"1 L"`, `"subs":1`)
	has(t, e.ok(central, "GET", "/admin/compras/insumos/"+leche, nil), "Lala", "Leche deslactosada", "$27.50",
		"Solo si no hay en el mercado")
	// Vuelve a quedar Costco primero (como lo usa el resto de la prueba).
	e.ok(central, "POST", "/admin/compras/insumos/"+leche, url.Values{"name": {"Leche entera"}, "unit": {"1 L"}, "location": {"Refri"},
		"active": {"1"}, "place": {costco, mercado}, "brand": {"Lala", ""}, "price": {"25", "27.50"},
		"source_note": {"Conviene por caja", "Solo si no hay en Costco"}, "substitute": {deslac}})
	// La casilla "Activo" llega como el navegador la manda: oculto "0" + "1".
	e.ok(central, "POST", "/admin/compras/insumos/"+cafe, url.Values{"name": {"Café en grano"}, "active": {"0", "1"}})
	e.ok(central, "POST", "/admin/compras/lugares/"+mercado, url.Values{"name": {"Mercado Hidalgo"}, "sort_order": {"1"}, "active": {"0", "1"}})
	if e.id(`SELECT is_active FROM compras_supplies WHERE id = ?`, cafe) != 1 || e.id(`SELECT is_active FROM compras_places WHERE id = ?`, mercado) != 1 {
		t.Fatal("guardar con la casilla marcada no debe archivar")
	}
	e.ok(central, "POST", "/admin/compras/insumos/"+cafe, url.Values{"name": {"Café en grano"}, "active": {"0"}})
	if e.id(`SELECT is_active FROM compras_supplies WHERE id = ?`, cafe) != 0 {
		t.Fatal("desmarcada, se archiva")
	}
	e.ok(central, "POST", "/admin/compras/insumos/"+cafe, url.Values{"name": {"Café en grano"}, "unit": {"1 kg"}, "location": {"Alacena"},
		"active": {"0", "1"}, "place": {mercado}, "price": {"380"}})
	if rec := e.do(central, "POST", "/admin/compras/insumos", url.Values{"name": {"leche ENTERA"}}); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("insumo repetido: %d", rec.Code)
	}

	// Beto puede surtir.
	e.ok(central, "POST", "/admin/compras/surten/71", url.Values{"shopper": {"1"}})
	has(t, e.ok(central, "GET", "/admin/compras?tab=surten", nil), "✓ Surte")

	// Sin lista no se pide; un mesero no la abre.
	has(t, e.ok(ana, "GET", "/compras", nil), "No hay lista abierta", "Refri", "Alacena")
	has(t, e.ok(ana, "POST", "/compras/pedir/"+leche, url.Values{"urgent": {"0"}}), "Todavía no hay lista abierta")
	if rec := e.do(ana, "POST", "/compras/lista/abrir", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("un mesero no abre listas: %d", rec.Code)
	}
	if rec := e.do(ana, "GET", "/compras/comprar", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("un mesero sin permiso no ve las compras: %d", rec.Code)
	}

	// La cajera abre desde su terminal; el equipo pide.
	e.ok(caro, "POST", "/compras/lista/abrir", nil)
	e.ok(ana, "POST", "/compras/pedir/"+leche, url.Values{"urgent": {"0"}})
	row := e.ok(ana, "POST", "/compras/pedir/"+cafe, url.Values{"urgent": {"1"}, "note": {"el de Chiapas"}})
	has(t, row, "urge", "Ana", "el de Chiapas", "Quitar mi pedido")
	e.ok(central, "POST", "/compras/pedir/"+leche, url.Values{"urgent": {"1"}}) // la caja pide lo mismo: se junta
	if n := e.id(`SELECT COUNT(*) FROM compras_items`); n != 2 {
		t.Fatalf("renglones: %d", n)
	}
	if u := e.id(`SELECT urgent FROM compras_items WHERE supply_id = ?`, leche); u != 1 {
		t.Fatal("si alguien dice que urge, el renglón urge")
	}
	has(t, e.ok(ana, "GET", "/compras", nil), "Ana, Caja")
	byCat := e.ok(ana, "GET", "/compras?por=categoria", nil)
	if !strings.Contains(byCat, ">Otros<") || strings.Contains(byCat, ">Refri<") {
		t.Fatal("por categoría se agrupa por categoría (aquí ninguna tiene: Otros), no por ubicación")
	}

	// Beto surte: agrupado por lugar, en el orden de los lugares.
	shop := e.ok(beto, "GET", "/compras/comprar", nil)
	has(t, shop, "Mercado Hidalgo", "Costco", "$405.00", "wa.me", "el de Chiapas",
		"Conviene por caja", "Si no hay, en este orden", "2º Mercado Hidalgo", "Solo si no hay en Costco")
	if strings.Index(shop, "Mercado Hidalgo") > strings.Index(shop, "📍 Costco") {
		t.Fatal("los lugares van en su orden (Mercado primero)")
	}
	item := itoa(e.id(`SELECT id FROM compras_items WHERE supply_id = ?`, leche))
	shop = e.ok(beto, "POST", "/compras/comprar/"+item+"/comprado", url.Values{"quantity": {"2"}, "total": {"$60"}, "place": {costco}})
	has(t, shop, "pagado $60.00", "1 de 2 comprados")
	if p := e.id(`SELECT price_cents FROM compras_sources WHERE supply_id = ? AND place_id = ?`, leche, costco); p != 3000 {
		t.Fatalf("el precio de referencia queda en $30: %d", p)
	}
	has(t, e.ok(beto, "POST", "/compras/comprar/"+item+"/comprado", url.Values{"total": {"abc"}}), "Precio inválido")
	txt := e.ok(beto, "GET", "/compras/lista/texto", nil)
	has(t, txt, "Café en grano", "URGE")
	if strings.Contains(txt, "Leche entera") {
		t.Fatal("lo comprado no va en el texto a enviar")
	}

	// A compras; un pedido tarde todavía entra. Se cierra y lo no comprado pasa.
	e.ok(caro, "POST", "/compras/lista/a-compras", nil)
	e.ok(ana, "POST", "/compras/pedir/"+deslac, url.Values{"urgent": {"0"}})
	has(t, e.ok(beto, "GET", "/compras/comprar", nil), "agregado tarde")
	has(t, e.ok(central, "GET", "/compras/lista", nil), "En compras", "Café en grano")
	e.ok(central, "POST", "/compras/lista/cerrar", nil)
	has(t, e.ok(central, "GET", "/compras/lista", nil), "No hay lista abierta", "Listas anteriores", "$60.00")
	e.ok(central, "POST", "/compras/lista/abrir", nil)
	if n := e.id(`SELECT COUNT(*) FROM compras_items i JOIN compras_lists l ON l.id = i.list_id
		WHERE l.status = 'OPEN' AND i.carried_from IS NOT NULL`); n != 2 {
		t.Fatalf("deben pasar café y deslactosada: %d", n)
	}
	has(t, e.ok(ana, "GET", "/compras", nil), "no se han surtido", "Café en grano")
	if n := e.id(`SELECT COUNT(*) FROM compras_requests r JOIN compras_items i ON i.id = r.item_id
		JOIN compras_lists l ON l.id = i.list_id WHERE l.status = 'OPEN' AND r.note = 'el de Chiapas' AND r.user_id = 70`); n != 1 {
		t.Fatal("quién pidió y su nota viajan con el pendiente")
	}
	if rec := e.do(central, "POST", "/compras/lista/abrir", nil); !strings.Contains(rec.Header().Get("Location"), "error=") {
		t.Fatal("no puede haber dos listas abiertas")
	}
	if action, _ := lastAudit(t, e.db); action != "COMPRAS_LIST_OPENED" {
		t.Fatalf("bitácora: %s", action)
	}
}

func TestComprasWithdraw(t *testing.T) {
	e := newComprasEnv(t)
	exec(t, e.db, `INSERT INTO users (id, name, role, pin_salt, pin_hash) VALUES (70, 'Ana', 'WAITER', 'x', 'y')`)
	ana := waiter(70, "Ana", RoleWaiter)
	e.ok(central, "POST", "/admin/compras/insumos", url.Values{"name": {"Hielo"}})
	hielo := itoa(e.id(`SELECT id FROM compras_supplies`))
	e.ok(central, "POST", "/compras/lista/abrir", nil)
	e.ok(ana, "POST", "/compras/pedir/"+hielo, url.Values{"urgent": {"1"}})
	e.ok(central, "POST", "/compras/pedir/"+hielo, url.Values{"urgent": {"0"}})
	e.ok(ana, "POST", "/compras/pedir/"+hielo+"/quitar", nil)
	if u := e.id(`SELECT urgent FROM compras_items`); u != 0 {
		t.Fatal("sin el pedido de Ana ya no urge")
	}
	e.ok(central, "POST", "/compras/pedir/"+hielo+"/quitar", nil)
	if n := e.id(`SELECT COUNT(*) FROM compras_items`); n != 0 {
		t.Fatal("si nadie lo pide, el renglón se quita")
	}
}

func TestComprasImport(t *testing.T) {
	e := newComprasEnv(t)
	e.ok(central, "POST", "/admin/compras/insumos", url.Values{"name": {"Leche"}, "notes": {"la de siempre"}})
	csvData := "\ufeffInsumo;Categoría;Presentación;Ubicación;Dónde se compra;Precio;Lugar 2;Precio 2\n" +
		"leche;Lácteos;1 L;Refri;Costco;$25.50;Oxxo;31\n" +
		"Canela;Especias;Bolsa;Alacena;Mercado;;;\n" +
		";;;;;;;\n" +
		"Vasos 12oz;Desechables;Paquete 50;Bodega;Costco;abc;;\n"
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "lista.csv")
	fw.Write([]byte(csvData))
	mw.Close()
	req := httptest.NewRequest("POST", "/admin/compras/importar", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.AddCookie(e.cookie)
	req = req.WithContext(context.WithValue(req.Context(), ctxTerminal, central))
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	has(t, rec.Body.String(), "2 nuevos, 1 actualizados, 3 lugares creados", "Precio inválido")

	var notes, loc string
	e.db.QueryRow(`SELECT notes, location FROM compras_supplies WHERE name = 'Leche'`).Scan(&notes, &loc)
	if notes != "la de siempre" || loc != "Refri" {
		t.Fatalf("actualizar conserva lo que no trae el archivo: %q %q", notes, loc)
	}
	if n := e.id(`SELECT COUNT(*) FROM compras_sources x JOIN compras_supplies s ON s.id = x.supply_id WHERE s.name = 'Leche'`); n != 2 {
		t.Fatalf("leche con 2 lugares: %d", n)
	}
}

func TestComprasWaiterRoutes(t *testing.T) {
	for _, c := range []struct {
		method, path string
		ok           bool
	}{
		{"GET", "/compras", true},
		{"GET", "/compras/comprar", true},
		{"POST", "/compras/pedir/12", true},
		{"POST", "/compras/pedir/12/quitar", true},
		{"POST", "/compras/comprar/3/comprado", true},
		{"POST", "/compras/lista/cerrar", true},
		{"POST", "/admin/compras/insumos", false},
		{"POST", "/compras/pedir/x", false},
	} {
		req := httptest.NewRequest(c.method, c.path, nil)
		if got := matches(waiterAllowed, req); got != c.ok {
			t.Errorf("%s %s: permitido=%v", c.method, c.path, got)
		}
	}
}

func TestComprasMoney(t *testing.T) {
	for in, want := range map[int64]string{0: "$0.00", 2550: "$25.50", 123456789: "$1,234,567.89", -500: "-$5.00"} {
		if got := formatCents(in); got != want {
			t.Errorf("formatCents(%d) = %s", in, got)
		}
	}
	for in, want := range map[string]int64{"25": 2500, "$1,234.5": 123450, " 0.07 ": 7} {
		if got, ok, err := parseCents(in); err != nil || !ok || got != want {
			t.Errorf("parseCents(%q) = %d %v %v", in, got, ok, err)
		}
	}
	if comprasKey("  Azúcar   MASCABADO ") != "azucar mascabado" {
		t.Error("comprasKey")
	}
}

func TestComprasPresentations(t *testing.T) {
	e := newComprasEnv(t)
	e.ok(central, "POST", "/admin/compras/lugares", url.Values{"name": {"Costco"}})
	e.ok(central, "POST", "/admin/compras/lugares", url.Values{"name": {"Fresh Market"}})
	e.ok(central, "POST", "/admin/compras/lugares", url.Values{"name": {"Mercado"}})
	costco := itoa(e.id(`SELECT id FROM compras_places WHERE name = 'Costco'`))
	fresh := itoa(e.id(`SELECT id FROM compras_places WHERE name = 'Fresh Market'`))
	mercado := itoa(e.id(`SELECT id FROM compras_places WHERE name = 'Mercado'`))

	// Fresas: en Fresh (el de siempre) burbuja de 200 g a $45; en Costco de
	// 400 g a $70. El contenido se lee del texto de la presentación.
	e.ok(central, "POST", "/admin/compras/insumos", url.Values{"name": {"Fresas"}, "unit": {"Burbuja"},
		"place": {fresh, costco}, "brand": {"", ""}, "price": {"45", "70"},
		"pack_label": {"Burbuja 200 g", "Burbuja 400 gr"}, "content_qty": {"", ""}, "content_unit": {"g", "g"},
		"by_weight": {"0", "0"}, "source_note": {"", ""}})
	var qty float64
	var unit string
	e.db.QueryRow(`SELECT content_qty, content_unit FROM compras_sources s JOIN compras_places p ON p.id = s.place_id
		WHERE p.name = 'Costco'`).Scan(&qty, &unit)
	if qty != 400 || unit != "g" {
		t.Fatalf("contenido leído de \"Burbuja 400 gr\": %v %s", qty, unit)
	}
	has(t, e.ok(central, "GET", "/admin/compras", nil), `"cheaper":"Costco ($70.00 · $175.00/kg)"`,
		`"unit_price":"$225.00/kg"`, `"pack":"Burbuja 200 g"`)

	// Aguacate a granel: el precio es por kg y se piden kilos con decimales.
	e.ok(central, "POST", "/admin/compras/insumos", url.Values{"name": {"Aguacate"}, "unit": {"kg"},
		"place": {mercado}, "brand": {""}, "price": {"75"}, "pack_label": {""}, "content_qty": {""},
		"content_unit": {"g"}, "by_weight": {"1"}, "source_note": {""}})
	aguacate := itoa(e.id(`SELECT id FROM compras_supplies WHERE name = 'Aguacate'`))
	fresas := itoa(e.id(`SELECT id FROM compras_supplies WHERE name = 'Fresas'`))
	e.ok(central, "POST", "/compras/lista/abrir", nil)
	e.ok(central, "POST", "/compras/pedir/"+aguacate, url.Values{"urgent": {"0"}})
	e.ok(central, "POST", "/compras/pedir/"+fresas, url.Values{"urgent": {"0"}})
	item := itoa(e.id(`SELECT id FROM compras_items WHERE supply_id = ?`, aguacate))
	if rec := e.do(central, "POST", "/compras/comprar/"+item+"/cantidad", url.Values{"quantity": {"0"}}); !strings.Contains(rec.Body.String(), "mayor a cero") {
		t.Fatal("cantidad cero debe rechazarse")
	}
	shop := e.ok(central, "POST", "/compras/comprar/"+item+"/cantidad", url.Values{"quantity": {"1,5"}})
	has(t, shop, "1.5 kg", "~$112.50 ($75.00/kg)", // 1.5 kg × $75
		"2º Costco · Burbuja 400 gr", "$70.00", "($175.00/kg)", "~$45.00 ($225.00/kg)", "💲 sale más barato")

	// Se compran 1.5 kg por $120: el precio de referencia queda en $80/kg.
	e.ok(central, "POST", "/compras/comprar/"+item+"/comprado", url.Values{"quantity": {"1.5"}, "total": {"120"}, "place": {mercado}})
	if p := e.id(`SELECT price_cents FROM compras_sources WHERE supply_id = ?`, aguacate); p != 8000 {
		t.Fatalf("precio por kg después de comprar: %d", p)
	}
	if txt := e.ok(central, "GET", "/compras/lista/texto", nil); !strings.Contains(txt, "Fresas (Burbuja 200 g)") {
		t.Fatalf("texto: %s", txt)
	}
}

func TestParseContent(t *testing.T) {
	for in, want := range map[string]string{
		"Burbuja 400 g": "400 g", "Bolsa 1 kg": "1 kg", "Caja 12 × 1 L": "12 L", "6x355 ml": "2130 ml",
		"Paquete 50 piezas": "50 pz", "Galón 3,8 litros": "3.8 L", "Burbuja": "", "12 oz": "",
	} {
		q, u, ok := parseContent(in)
		got := ""
		if ok {
			got = formatQty(q) + " " + u
		}
		if got != want {
			t.Errorf("parseContent(%q) = %q, quiero %q", in, got, want)
		}
	}
}

// fakeOCR responde siempre las mismas cajas (salida real de open-pos-ocr).
func fakeOCR(t *testing.T, file string) func(context.Context, string) ([]ocr.Box, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var out struct{ Lines []ocr.Box }
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return func(_ context.Context, path string) ([]ocr.Box, error) {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("el OCR debe recibir la foto guardada: %v", err)
		}
		return out.Lines, nil
	}
}

func (e *comprasEnv) upload(term Terminal, name string, data []byte) *httptest.ResponseRecorder {
	e.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("photo", name)
	fw.Write(data)
	mw.Close()
	req := httptest.NewRequest("POST", "/compras/tickets", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req = req.WithContext(context.WithValue(req.Context(), ctxTerminal, term))
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

var jpegBytes = []byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00foto")

func TestComprasTickets(t *testing.T) {
	e := newComprasEnv(t)
	e.h.DataDir = t.TempDir()
	e.h.OCR = fakeOCR(t, "internal/ocr/testdata/costco.json")
	exec(t, e.db, `INSERT INTO users (id, name, role, pin_salt, pin_hash) VALUES (71, 'Beto', 'WAITER', 'x', 'y')`)
	exec(t, e.db, `INSERT INTO compras_shoppers (user_id) VALUES (71)`)
	beto := waiter(71, "Beto", RoleWaiter)
	ana := waiter(72, "Ana", RoleWaiter)

	e.ok(central, "POST", "/admin/compras/lugares", url.Values{"name": {"Costco"}})
	costco := itoa(e.id(`SELECT id FROM compras_places`))
	for _, s := range []url.Values{
		{"name": {"Leche entera"}, "unit": {"Caja 12 × 1 L"}, "place": {costco}, "price": {"280"}, "brand": {"Lala"}},
		{"name": {"Fresas"}, "place": {costco}, "pack_label": {"Burbuja 400 g"}, "price": {"70"}},
		{"name": {"Vasos 12 oz"}, "place": {costco}, "price": {"90"}},
		{"name": {"Aguacate"}, "place": {costco}, "by_weight": {"1"}, "price": {"70"}},
		{"name": {"Servilletas"}, "place": {costco}, "price": {"95"}},
	} {
		e.ok(central, "POST", "/admin/compras/insumos", s)
	}
	sid := func(name string) string { return itoa(e.id(`SELECT id FROM compras_supplies WHERE name = ?`, name)) }
	e.ok(central, "POST", "/compras/lista/abrir", nil)
	e.ok(central, "POST", "/compras/pedir/"+sid("Leche entera"), url.Values{"urgent": {"1"}})
	e.ok(central, "POST", "/compras/pedir/"+sid("Fresas"), url.Values{"urgent": {"0"}})

	// Quien no surte no sube tickets.
	if rec := e.upload(ana, "t.jpg", jpegBytes); rec.Code != http.StatusForbidden {
		t.Fatalf("un mesero sin permiso no sube tickets: %d", rec.Code)
	}
	if rec := e.upload(beto, "notas.txt", []byte("hola")); !strings.Contains(rec.Header().Get("Location"), "error=") {
		t.Fatal("un archivo que no es foto se rechaza")
	}

	rec := e.upload(beto, "IMG_1234.JPG", jpegBytes)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/compras/tickets/1" {
		t.Fatalf("subir: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	var place, total int64
	var date, image string
	e.db.QueryRow(`SELECT place_id, purchased_on, total_cents, image FROM compras_receipts WHERE id = 1`).Scan(&place, &date, &total, &image)
	if itoa(place) != costco || date != "2026-10-06" || total != 79265 {
		t.Fatalf("tienda %d fecha %q total %d", place, date, total)
	}
	if _, err := os.Stat(filepath.Join(e.h.DataDir, "compras-tickets", image)); err != nil {
		t.Fatalf("la foto debe guardarse: %v", err)
	}
	suggested := map[string]string{}
	rows, _ := e.db.Query(`SELECT l.description, COALESCE(s.name, '') FROM compras_receipt_lines l
		LEFT JOIN compras_supplies s ON s.id = l.supply_id WHERE l.receipt_id = 1`)
	for rows.Next() {
		var d, s string
		rows.Scan(&d, &s)
		suggested[d] = s
	}
	rows.Close()
	want := map[string]string{"FRESA 400G": "Fresas", "LECHE ENT LALA 12PK": "Leche entera",
		"VASOS TERMICOS 120Z": "Vasos 12 oz", "AGUACATE HASS": "Aguacate"}
	for d, s := range want {
		if suggested[d] != s {
			t.Errorf("%q debió sugerirse como %q, quedó %q (%v)", d, s, suggested[d], suggested)
		}
	}
	page := e.ok(beto, "GET", "/compras/tickets/1", nil)
	has(t, page, "Registrar compra", `"desc":"FRESA 400G"`, `"inList":true`, `'792.65'`)
	if rec := e.do(beto, "GET", "/compras/tickets/1/imagen", nil); rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), jpegBytes) {
		t.Fatalf("imagen: %d", rec.Code)
	}

	// Se registra; un renglón extra se relaciona a mano (y se aprende).
	form := url.Values{"place": {costco}, "date": {"2026-10-06"}, "total": {"887.65"},
		"desc":      {"FRESA 400G", "LECHE ENT LALA 12PK", "VASOS TERMICOS 120Z", "AGUACATE HASS", "PETALO 500H", "BOLSA"},
		"qty":       {"1", "1", "2", "0.850", "1", "1"},
		"by_weight": {"0", "0", "0", "1", "0", "0"},
		"amount":    {"69.90", "289.00", "185.00", "63.75", "95.00", "0"},
		"supply":    {sid("Fresas"), sid("Leche entera"), sid("Vasos 12 oz"), sid("Aguacate"), sid("Servilletas"), ""}}
	if rec := e.do(beto, "POST", "/compras/tickets/1/confirmar", form); !strings.Contains(rec.Header().Get("Location"), "error=") {
		t.Fatal("un importe en cero debe rechazarse")
	}
	form["amount"][5] = "3.00"
	e.ok(beto, "POST", "/compras/tickets/1/confirmar", form)

	var status string
	var price int64
	e.db.QueryRow(`SELECT status, bought_price_cents FROM compras_items WHERE supply_id = ?`, sid("Leche entera")).Scan(&status, &price)
	if status != "BOUGHT" || price != 28900 {
		t.Fatalf("la leche de la lista se tacha con su precio: %s %d", status, price)
	}
	if p := e.id(`SELECT price_cents FROM compras_sources WHERE supply_id = ?`, sid("Vasos 12 oz")); p != 9250 {
		t.Fatalf("precio de los vasos (no estaban en la lista): %d", p)
	}
	if p := e.id(`SELECT price_cents FROM compras_sources WHERE supply_id = ?`, sid("Aguacate")); p != 7500 {
		t.Fatalf("precio por kg del aguacate: %d", p)
	}
	if n := e.id(`SELECT COUNT(*) FROM compras_receipt_lines WHERE receipt_id = 1 AND item_id IS NOT NULL`); n != 2 {
		t.Fatalf("renglones tachados en la lista: %d", n)
	}
	if rec := e.do(beto, "POST", "/compras/tickets/1/confirmar", form); !strings.Contains(rec.Header().Get("Location"), "error=") {
		t.Fatal("un ticket no se registra dos veces")
	}
	has(t, e.ok(beto, "GET", "/compras/tickets/1", nil), "Registrado", "$887.65", "tachado en la lista")
	has(t, e.ok(beto, "GET", "/compras/tickets", nil), "Registrado en", "Costco", "$887.65")
	if action, _ := lastAudit(t, e.db); action != "COMPRAS_TICKET_CONFIRMED" {
		t.Fatalf("bitácora: %s", action)
	}

	// Lo relacionado a mano se recuerda en el siguiente ticket.
	e.h.OCR = func(context.Context, string) ([]ocr.Box, error) {
		return []ocr.Box{{Text: "PETALO 500H", X: 0.05, Y: 0.30, W: 0.3, H: 0.02}, {Text: "95.00", X: 0.8, Y: 0.30, W: 0.1, H: 0.02}}, nil
	}
	e.upload(beto, "otro.png", []byte("\x89PNG\r\n\x1a\n...."))
	if s := e.id(`SELECT supply_id FROM compras_receipt_lines WHERE receipt_id = 2`); itoa(s) != sid("Servilletas") {
		t.Fatalf("debió recordar PETALO 500H → Servilletas: %d", s)
	}
	// Descartar borra el borrador y su foto.
	var image2 string
	e.db.QueryRow(`SELECT image FROM compras_receipts WHERE id = 2`).Scan(&image2)
	e.ok(beto, "POST", "/compras/tickets/2/descartar", nil)
	if _, err := os.Stat(filepath.Join(e.h.DataDir, "compras-tickets", image2)); !os.IsNotExist(err) {
		t.Fatal("la foto del ticket descartado se borra")
	}
	if rec := e.do(beto, "POST", "/compras/tickets/1/descartar", nil); !strings.Contains(rec.Header().Get("Location"), "error=") {
		t.Fatal("un ticket registrado no se descarta")
	}

	// Sin lector: el ticket queda para capturarse a mano.
	e.h.OCR = func(context.Context, string) ([]ocr.Box, error) { return nil, ocr.ErrUnavailable }
	e.upload(beto, "t.jpg", jpegBytes)
	has(t, e.ok(beto, "GET", "/compras/tickets/3", nil), "captura los productos a mano")
}

func TestBackupTicketImages(t *testing.T) {
	data, backup := t.TempDir(), t.TempDir()
	os.MkdirAll(filepath.Join(data, "compras-tickets"), 0o755)
	os.WriteFile(filepath.Join(data, "compras-tickets", "a.jpg"), []byte("uno"), 0o644)
	BackupTicketImages(data, backup)
	os.WriteFile(filepath.Join(data, "compras-tickets", "b.jpg"), []byte("dos"), 0o644)
	BackupTicketImages(data, backup)
	for name, want := range map[string]string{"a.jpg": "uno", "b.jpg": "dos"} {
		if b, err := os.ReadFile(filepath.Join(backup, "compras-tickets", name)); err != nil || string(b) != want {
			t.Errorf("%s: %q %v", name, b, err)
		}
	}
	BackupTicketImages(t.TempDir(), backup) // sin tickets no falla
}

// Compras viene apagado: no responde ni aparece en el POS o el panel.
