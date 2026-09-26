package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func TestMigrationKeepsPrinterRouting(t *testing.T) {
	db, _ := setupCashDB(t)
	// Semilla: Cafetería y Postres salían en Barra (1); Cocina Caliente en Cocina (2).
	for product, want := range map[int]int64{1: 1, 2: 1, 3: 2, 4: 1} {
		var area int64
		db.QueryRow(`SELECT area_id FROM products WHERE id = ?`, product).Scan(&area)
		if area != want {
			t.Errorf("producto %d: área %d, se esperaba %d", product, area, want)
		}
	}
	var name string
	db.QueryRow(`SELECT name FROM production_areas WHERE id = 1`).Scan(&name)
	if name != "Barra" {
		t.Errorf("el área 1 debe llamarse como su impresora, quedó %q", name)
	}
}

func TestSubcategoryRules(t *testing.T) {
	db, h := setupCashDB(t)
	cookie := adminLogin(t, h, "0001")
	create := h.RequireAdmin(h.AdminCreateCategory)

	rec := call(create, "POST", "/", `{"name": "Con café", "color_hex": "#2a78d6", "parent_id": 1, "is_active": true}`, cookie, 0)
	if rec.Code != http.StatusOK {
		t.Fatalf("crear subcategoría: %d %s", rec.Code, rec.Body)
	}
	var sub int64
	db.QueryRow(`SELECT id FROM categories WHERE name = 'Con café'`).Scan(&sub)

	if rec := call(create, "POST", "/", `{"name": "Nieta", "color_hex": "#2a78d6", "parent_id": `+strconv.FormatInt(sub, 10)+`, "is_active": true}`, cookie, 0); rec.Code != http.StatusConflict {
		t.Fatalf("solo hay un nivel de subcategorías, dio %d", rec.Code)
	}
	if rec := call(h.RequireAdmin(h.AdminUpdateCategory), "PUT", "/", `{"name": "Cafetería", "color_hex": "#D97706", "parent_id": 2, "is_active": true}`, cookie, 1); rec.Code != http.StatusConflict {
		t.Fatalf("una categoría con subcategorías no puede volverse subcategoría, dio %d", rec.Code)
	}

	// En el POS la subcategoría aparece dentro de su categoría.
	exec(t, db, `UPDATE products SET category_id = ? WHERE id = 2`, sub)
	var tree struct{ Categories []*menuCategory }
	json.Unmarshal(call(h.GetMenuTree, "GET", "/", "", nil, 0).Body.Bytes(), &tree)
	var cafe *menuCategory
	for _, c := range tree.Categories {
		if c.ID == 1 {
			cafe = c
		}
	}
	if cafe == nil || len(cafe.Children) != 1 || cafe.Children[0].Name != "Con café" ||
		len(cafe.Children[0].Products) != 1 || cafe.Children[0].Products[0].Name != "Latte 12oz" {
		t.Fatalf("la subcategoría y su producto deben ir dentro de Cafetería: %+v", cafe)
	}
	// Las ramas sin productos no se muestran.
	exec(t, db, `INSERT INTO categories (name, parent_id, is_active) VALUES ('Vacía', 1, 1)`)
	json.Unmarshal(call(h.GetMenuTree, "GET", "/", "", nil, 0).Body.Bytes(), &tree)
	for _, c := range tree.Categories {
		for _, ch := range c.Children {
			if ch.Name == "Vacía" {
				t.Fatal("una subcategoría sin productos no debe aparecer en el POS")
			}
		}
	}
}

func TestModifiersEndToEnd(t *testing.T) {
	db, h := setupCashDB(t)
	cookie := adminLogin(t, h, "0001")
	fp := newFakePrinter(t)
	exec(t, db, `UPDATE printers SET is_active = 1, connection_type = 'NETWORK', ip_address = '127.0.0.1', port = ? WHERE id = 1`, fp.port())

	group := func(body string) int64 {
		rec := call(h.RequireAdmin(h.AdminCreateModifierGroup), "POST", "/", body, cookie, 0)
		if rec.Code != http.StatusOK {
			t.Fatalf("crear grupo: %d %s", rec.Code, rec.Body)
		}
		var out struct{ Group ModifierGroup }
		json.Unmarshal(rec.Body.Bytes(), &out)
		return out.Group.ID
	}
	milk := group(`{"name": "Tipo de leche", "min": 1, "max": 1, "is_active": true, "options": [
		{"name": "Entera", "price_extra": 0, "is_available": true},
		{"name": "Avena", "price_extra": 12, "is_available": true}]}`)
	syrup := group(`{"name": "Jarabes", "min": 0, "max": 2, "is_active": true, "options": [
		{"name": "Vainilla", "price_extra": 8, "is_available": true},
		{"name": "Caramelo", "price_extra": 8, "is_available": true},
		{"name": "Avellana", "price_extra": 8, "is_available": false}]}`)
	if rec := call(h.RequireAdmin(h.AdminCreateModifierGroup), "POST", "/", `{"name": "Mal", "min": 3, "max": 1, "options": [{"name": "x"}]}`, cookie, 0); rec.Code != http.StatusBadRequest {
		t.Fatalf("mínimo mayor que máximo debe rechazarse, dio %d", rec.Code)
	}

	// Latte (id 2, $65) con leche y jarabes.
	rec := call(h.RequireAdmin(h.AdminUpdateProduct), "PUT", "/", `{"category_id": 1, "area_id": 1, "name": "Latte 12oz", "price": 65,
		"is_available": true, "is_active": true, "modifier_group_ids": [`+strconv.FormatInt(milk, 10)+`, `+strconv.FormatInt(syrup, 10)+`]}`, cookie, 2)
	if rec.Code != http.StatusOK {
		t.Fatalf("asignar grupos: %d %s", rec.Code, rec.Body)
	}
	opt := map[string]int64{}
	rows, _ := db.Query(`SELECT name, id FROM modifier_options`)
	for rows.Next() {
		var n string
		var id int64
		rows.Scan(&n, &id)
		opt[n] = id
	}
	rows.Close()
	ids := func(names ...string) string {
		var s []string
		for _, n := range names {
			s = append(s, strconv.FormatInt(opt[n], 10))
		}
		return "[" + strings.Join(s, ",") + "]"
	}
	order := func(mods string) *httpResult {
		rec := post(t, h.AddToOrder, 0, `{"order_type": "FLASH", "items": [{"product_id": 2, "quantity": 2, "modifier_ids": `+mods+`}]}`)
		return &httpResult{rec.Code, rec.Body.String()}
	}

	for _, bad := range []struct{ mods, want string }{
		{ids(), "Elige al menos 1"},                                    // falta la leche
		{ids("Entera", "Avena"), "Máximo 1"},                           // dos leches
		{ids("Avena", "Avellana"), "agotado"},                          // opción no disponible
		{ids("Entera", "Vainilla", "Caramelo", "Avellana"), "agotado"}, // cualquiera de los dos errores
	} {
		if r := order(bad.mods); r.code != http.StatusUnprocessableEntity || !strings.Contains(r.body, bad.want) {
			t.Errorf("mods %s: %d %q (se esperaba %q)", bad.mods, r.code, r.body, bad.want)
		}
	}

	if r := order(ids("Vainilla", "Avena")); r.code != http.StatusCreated {
		t.Fatalf("orden válida: %d %s", r.code, r.body)
	}
	var price float64
	var text string
	db.QueryRow(`SELECT unit_price, modifiers_text FROM order_items ORDER BY id DESC LIMIT 1`).Scan(&price, &text)
	// 65 + 12 (avena) + 8 (vainilla); el texto va en el orden de los grupos.
	if price != 85 || text != "Avena · Vainilla" {
		t.Fatalf("línea: $%v %q", price, text)
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM order_item_modifiers`).Scan(&n)
	if n != 2 {
		t.Fatalf("se esperaban 2 modificadores guardados, hay %d", n)
	}
	if job := fp.jobCount(t, 1)[0]; !bytes.Contains(job, []byte("+ Avena")) || !bytes.Contains(job, []byte("+ Vainilla")) {
		t.Fatalf("la comanda debe llevar los modificadores:\n%q", job)
	}
}

type httpResult struct {
	code int
	body string
}
