package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// call ejecuta un handler como lo haría el servidor, opcionalmente con la
// cookie de administración y un {id} en la ruta.
func call(fn http.HandlerFunc, method, path, body string, cookie *http.Cookie, id int64) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if id > 0 {
		req.SetPathValue("id", strconv.FormatInt(id, 10))
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	fn(rec, req)
	return rec
}

func adminLogin(t *testing.T, h *POSHandler, pin string) *http.Cookie {
	t.Helper()
	rec := call(h.AdminLogin, "POST", "/api/admin/login", `{"pin": "`+pin+`"}`, nil, 0)
	if rec.Code != http.StatusOK {
		t.Fatalf("login %s: %d %s", pin, rec.Code, rec.Body)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == adminCookie {
			return c
		}
	}
	t.Fatal("el login no dejó cookie de sesión")
	return nil
}

func TestAdminLoginAndSession(t *testing.T) {
	_, h := setupCashDB(t)
	protected := h.RequireAdmin(h.AdminListUsers)

	if rec := call(protected, "GET", "/api/admin/users", "", nil, 0); rec.Code != http.StatusUnauthorized {
		t.Fatalf("sin sesión la API debe dar 401, dio %d", rec.Code)
	}
	if rec := call(h.RequireAdmin(func(http.ResponseWriter, *http.Request) {}), "GET", "/admin/menu", "", nil, 0); rec.Code != http.StatusSeeOther ||
		!strings.HasPrefix(rec.Header().Get("Location"), "/admin/login?next=") {
		t.Fatalf("sin sesión una página debe mandar al login: %d %s", rec.Code, rec.Header().Get("Location"))
	}

	cookie := adminLogin(t, h, "0001")
	if rec := call(protected, "GET", "/api/admin/users", "", cookie, 0); rec.Code != http.StatusOK {
		t.Fatalf("con sesión debe responder 200, dio %d", rec.Code)
	}

	// Un token inventado no sirve.
	fake := &http.Cookie{Name: adminCookie, Value: strings.Repeat("a", 64)}
	if rec := call(protected, "GET", "/api/admin/users", "", fake, 0); rec.Code != http.StatusUnauthorized {
		t.Fatalf("token falso debe dar 401, dio %d", rec.Code)
	}

	call(h.AdminLogout, "POST", "/api/admin/logout", "", cookie, 0)
	if rec := call(protected, "GET", "/api/admin/users", "", cookie, 0); rec.Code != http.StatusUnauthorized {
		t.Fatalf("después de salir la sesión no debe servir, dio %d", rec.Code)
	}
}

func TestAdminSessionIdleTimeout(t *testing.T) {
	db, h := setupCashDB(t)
	cookie := adminLogin(t, h, "0001")
	exec(t, db, `UPDATE sessions SET last_seen_at = datetime('now', '-31 minutes')`)
	if rec := call(h.RequireAdmin(h.AdminListUsers), "GET", "/api/admin/users", "", cookie, 0); rec.Code != http.StatusUnauthorized {
		t.Fatalf("una sesión inactiva por más de 30 min debe expirar, dio %d", rec.Code)
	}
}

func TestAdminLoginRateLimit(t *testing.T) {
	_, h := setupCashDB(t)
	now := time.Now()
	h.logins.now = func() time.Time { return now }

	for i := 0; i < loginMaxFails; i++ {
		if rec := call(h.AdminLogin, "POST", "/", `{"pin": "9999"}`, nil, 0); rec.Code != http.StatusUnauthorized {
			t.Fatalf("intento %d: se esperaba 401, dio %d", i+1, rec.Code)
		}
	}
	// Bloqueado: ni el PIN correcto entra.
	if rec := call(h.AdminLogin, "POST", "/", `{"pin": "0001"}`, nil, 0); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("tras %d fallos debe bloquear, dio %d", loginMaxFails, rec.Code)
	}
	now = now.Add(loginLockout + time.Second)
	adminLogin(t, h, "0001")
}

func TestAdminUserRules(t *testing.T) {
	db, h := setupCashDB(t)
	cookie := adminLogin(t, h, "0001")
	create := h.RequireAdmin(h.AdminCreateUser)
	update := h.RequireAdmin(h.AdminUpdateUser)

	if rec := call(create, "POST", "/", `{"name": "Ana", "role": "WAITER", "pin": "1234"}`, cookie, 0); rec.Code != http.StatusCreated {
		t.Fatalf("crear mesero: %d %s", rec.Code, rec.Body)
	}
	if rec := call(create, "POST", "/", `{"name": "Beto", "role": "CASHIER", "pin": "1234"}`, cookie, 0); rec.Code != http.StatusConflict {
		t.Fatalf("un PIN repetido debe rechazarse, dio %d", rec.Code)
	}
	if rec := call(create, "POST", "/", `{"name": "Beto", "role": "JEFE", "pin": "5678"}`, cookie, 0); rec.Code != http.StatusBadRequest {
		t.Fatalf("un rol inválido debe rechazarse, dio %d", rec.Code)
	}

	// La mesera no entra al panel ni opera la caja.
	if rec := call(h.AdminLogin, "POST", "/", `{"pin": "1234"}`, nil, 0); rec.Code != http.StatusForbidden {
		t.Fatalf("un mesero no debe entrar al panel, dio %d", rec.Code)
	}
	if rec := post(t, h.OpenCash, 0, `{"pin": "1234", "opening_float": 100}`); rec.Code != http.StatusForbidden {
		t.Fatalf("un mesero no debe abrir la caja, dio %d", rec.Code)
	}

	// No se puede quitar el último administrador (ni a uno mismo).
	if rec := call(update, "PUT", "/", `{"name": "Usuario general", "role": "CASHIER", "is_active": true}`, cookie, 1); rec.Code != http.StatusConflict {
		t.Fatalf("no debe poder quitarse a sí mismo el acceso, dio %d", rec.Code)
	}

	// Cambiar el PIN del mesero invalida el anterior.
	var anaID int64
	db.QueryRow(`SELECT id FROM users WHERE name = 'Ana'`).Scan(&anaID)
	if rec := call(update, "PUT", "/", `{"name": "Ana", "role": "WAITER", "is_active": true, "pin": "4321"}`, cookie, anaID); rec.Code != http.StatusOK {
		t.Fatalf("cambiar PIN: %d %s", rec.Code, rec.Body)
	}
	if _, err := userByPIN(t.Context(), db, "1234"); err != errBadPIN {
		t.Fatal("el PIN anterior debe dejar de funcionar")
	}
	if u, err := userByPIN(t.Context(), db, "4321"); err != nil || u.Name != "Ana" {
		t.Fatalf("el PIN nuevo debe funcionar: %v", err)
	}

	action, d := lastAudit(t, db)
	if action != "USER_UPDATED" || d["pin_changed"] != true || strings.Contains(d["after"].(map[string]any)["name"].(string), "4321") {
		t.Fatalf("bitácora: %s %v", action, d)
	}
}

func TestAdminMenuAndPOS(t *testing.T) {
	db, h := setupCashDB(t)
	cookie := adminLogin(t, h, "0001")

	rec := call(h.RequireAdmin(h.AdminCreateProduct), "POST", "/",
		`{"category_id": 1, "name": "Cold Brew", "price": 70, "is_available": true, "is_active": true}`, cookie, 0)
	if rec.Code != http.StatusOK {
		t.Fatalf("crear producto: %d %s", rec.Code, rec.Body)
	}
	var id int64
	db.QueryRow(`SELECT id FROM products WHERE name = 'Cold Brew'`).Scan(&id)

	rec = call(h.RequireAdmin(h.AdminUpdateProduct), "PUT", "/",
		`{"category_id": 1, "name": "Cold Brew", "price": 75.5, "is_available": true, "is_active": true}`, cookie, id)
	if rec.Code != http.StatusOK {
		t.Fatalf("editar producto: %d %s", rec.Code, rec.Body)
	}
	if action, d := lastAudit(t, db); action != "PRODUCT_UPDATED" || d["price_before"] != 70.0 || d["price_after"] != 75.5 {
		t.Fatalf("el cambio de precio debe quedar en la bitácora: %s %v", action, d)
	}

	// findProduct busca el producto en el árbol del menú del POS.
	findProduct := func() *menuProduct {
		var out struct{ Categories []*menuCategory }
		json.Unmarshal(call(h.GetMenuTree, "GET", "/api/menu/tree", "", nil, 0).Body.Bytes(), &out)
		var walk func([]*menuCategory) *menuProduct
		walk = func(cats []*menuCategory) *menuProduct {
			for _, c := range cats {
				for i := range c.Products {
					if c.Products[i].ID == id {
						return &c.Products[i]
					}
				}
				if p := walk(c.Children); p != nil {
					return p
				}
			}
			return nil
		}
		return walk(out.Categories)
	}
	if p := findProduct(); p == nil || !p.Available {
		t.Fatal("el producto nuevo debe aparecer en el POS")
	}

	// Agotado: se ve en gris y no se puede agregar.
	call(h.RequireAdmin(h.AdminSetAvailability), "POST", "/", `{"available": false}`, cookie, id)
	if p := findProduct(); p == nil || p.Available {
		t.Fatal("un producto agotado debe mostrarse marcado como agotado")
	}
	if rec := post(t, h.AddToOrder, 0, `{"order_type": "FLASH", "items": [{"product_id": `+strconv.FormatInt(id, 10)+`, "quantity": 1}]}`); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("no se debe poder pedir un producto agotado, dio %d", rec.Code)
	}

	// Archivado: desaparece del POS.
	call(h.RequireAdmin(h.AdminUpdateProduct), "PUT", "/",
		`{"category_id": 1, "name": "Cold Brew", "price": 75.5, "is_available": true, "is_active": false}`, cookie, id)
	if findProduct() != nil {
		t.Fatal("un producto archivado no debe aparecer en el POS")
	}
}

func TestAdminTablesAndSettings(t *testing.T) {
	db, h := setupCashDB(t)
	cookie := adminLogin(t, h, "0001")

	exec(t, db, `INSERT INTO table_sessions (id, table_id, status) VALUES (700, 1, 'OPEN')`)
	if rec := call(h.RequireAdmin(h.AdminUpdateTable), "PUT", "/", `{"name": "Mesa 1", "zone": "Interior", "is_active": false}`, cookie, 1); rec.Code != http.StatusConflict {
		t.Fatalf("una mesa ocupada no se debe archivar, dio %d %s", rec.Code, rec.Body)
	}
	if rec := call(h.RequireAdmin(h.AdminUpdateTable), "PUT", "/", `{"name": "Terraza 1", "zone": "Terraza", "is_active": false}`, cookie, 3); rec.Code != http.StatusOK {
		t.Fatalf("archivar mesa libre: %d %s", rec.Code, rec.Body)
	}
	out := httptest.NewRecorder()
	h.GetFloor(out, httptest.NewRequest("GET", "/", nil))
	if strings.Contains(out.Body.String(), "Terraza 1") {
		t.Fatal("una mesa archivada no debe aparecer en el POS")
	}

	settings := h.RequireAdmin(h.AdminUpdateSettings)
	if rec := call(settings, "PUT", "/", `{"business_name": "Café", "tax_rate": 1.6}`, cookie, 0); rec.Code != http.StatusBadRequest {
		t.Fatalf("un IVA de 160%% debe rechazarse, dio %d", rec.Code)
	}
	if rec := call(settings, "PUT", "/", `{"business_name": "Café Luna", "tax_rate": 0.08, "prices_include_tax": true, "ticket_footer": "Vuelve pronto"}`, cookie, 0); rec.Code != http.StatusOK {
		t.Fatalf("guardar configuración: %d %s", rec.Code, rec.Body)
	}
	if s, _ := loadSettings(t.Context(), db); s.BusinessName != "Café Luna" || s.TaxRate != 0.08 {
		t.Fatalf("configuración: %+v", s)
	}
}

func TestAdminPagesRender(t *testing.T) {
	_, h := setupCashDB(t)
	cookie := adminLogin(t, h, "0001")
	ui := NewUIHandler(h)
	t.Chdir("../..")
	for _, s := range adminSectionList {
		req := httptest.NewRequest("GET", "/admin/"+s.Key, nil)
		req.SetPathValue("section", s.Key)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		h.RequireAdmin(ui.ServeAdminPage)(rec, req)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Administración") {
			t.Errorf("/admin/%s: %d %s", s.Key, rec.Code, rec.Body.String()[:min(300, rec.Body.Len())])
		}
		if !strings.Contains(rec.Body.String(), "PIN genérico") {
			t.Errorf("/admin/%s debe advertir del PIN 0001", s.Key)
		}
	}
}
