package handler

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func tableRect(t *testing.T, db *sql.DB, id int64) (r rect, zone string) {
	t.Helper()
	if err := db.QueryRow(`SELECT pos_x, pos_y, width, height, zone FROM dining_tables WHERE id = ?`, id).
		Scan(&r.X, &r.Y, &r.W, &r.H, &zone); err != nil {
		t.Fatal(err)
	}
	return
}

func TestFreeSpot(t *testing.T) {
	x, y, ok := freeSpot(nil, 2, 2)
	if !ok || x != 1 || y != 1 {
		t.Fatalf("plano vacío: (%d,%d) %v, quería (1,1) con pasillo", x, y, ok)
	}
	x, y, _ = freeSpot([]rect{{1, 1, 2, 2}}, 2, 2)
	if (rect{x, y, 2, 2}).grow(1).overlaps(rect{1, 1, 2, 2}) {
		t.Fatalf("debe dejar una celda de pasillo, dio (%d,%d)", x, y)
	}
}

func TestFloorLayoutEditing(t *testing.T) {
	db, h := setupCashDB(t)
	cookie := adminLogin(t, h, "0001")
	move := h.RequireAdmin(h.AdminMoveTable)

	// Mesa nueva: aparece en un lugar libre de su zona, sin encimarse.
	rec := call(h.RequireAdmin(h.AdminCreateTable), "POST", "/", `{"name": "Mesa 9", "zone": "Interior", "is_active": true}`, cookie, 0)
	if rec.Code != http.StatusOK {
		t.Fatalf("crear mesa: %d %s", rec.Code, rec.Body)
	}
	var created struct{ Table AdminTable }
	json.Unmarshal(rec.Body.Bytes(), &created)
	nueva, _ := tableRect(t, db, created.Table.ID)
	occ, _ := occupiedCells(t.Context(), db, "Interior", created.Table.ID, 0)
	if collides(nueva, occ) {
		t.Fatalf("la mesa nueva quedó encimada: %+v", nueva)
	}

	// Mover encima de otra mesa se rechaza; a un lugar libre, se guarda.
	m1, _ := tableRect(t, db, 1)
	body := func(r rect, shape string) string {
		b, _ := json.Marshal(map[string]any{"x": r.X, "y": r.Y, "w": r.W, "h": r.H, "shape": shape})
		return string(b)
	}
	if rec := call(move, "PUT", "/", body(m1, "SQUARE"), cookie, created.Table.ID); rec.Code != http.StatusConflict {
		t.Fatalf("encimar mesas debe rechazarse, dio %d", rec.Code)
	}
	if rec := call(move, "PUT", "/", body(rect{58, 0, 4, 2}, "SQUARE"), cookie, created.Table.ID); rec.Code != http.StatusBadRequest {
		t.Fatalf("fuera del plano debe rechazarse, dio %d", rec.Code)
	}
	if rec := call(move, "PUT", "/", body(rect{10, 8, 3, 2}, "ROUND"), cookie, created.Table.ID); rec.Code != http.StatusOK {
		t.Fatalf("mover mesa: %d %s", rec.Code, rec.Body)
	}
	if r, _ := tableRect(t, db, created.Table.ID); r != (rect{10, 8, 3, 2}) {
		t.Fatalf("posición guardada: %+v", r)
	}

	// Elementos fijos: se crean en un lugar libre y tampoco se enciman.
	rec = call(h.RequireAdmin(h.AdminCreateFloorItem), "POST", "/", `{"zone": "Interior", "kind": "COUNTER"}`, cookie, 0)
	var item struct{ Item FloorItem }
	json.Unmarshal(rec.Body.Bytes(), &item)
	if rec.Code != http.StatusOK || item.Item.Label != "Barra" {
		t.Fatalf("crear barra: %d %s", rec.Code, rec.Body)
	}
	it := item.Item
	it.X, it.Y = 10, 8
	b, _ := json.Marshal(it)
	if rec := call(h.RequireAdmin(h.AdminUpdateFloorItem), "PUT", "/", string(b), cookie, it.ID); rec.Code != http.StatusConflict {
		t.Fatalf("encimar la barra en una mesa debe rechazarse, dio %d", rec.Code)
	}
	if rec := call(h.RequireAdmin(h.AdminCreateFloorItem), "POST", "/", `{"zone": "Interior", "kind": "SOFA"}`, cookie, 0); rec.Code != http.StatusBadRequest {
		t.Fatalf("tipo inválido debe rechazarse, dio %d", rec.Code)
	}

	// Unir zonas: todo pasa a la otra zona y lo que choque se reacomoda.
	exec(t, db, `UPDATE dining_tables SET pos_x = ?, pos_y = ? WHERE zone = 'Terraza'`, m1.X, m1.Y)
	if rec := call(h.RequireAdmin(h.AdminRenameZone), "POST", "/", `{"zone": "Terraza", "to": "Interior"}`, cookie, 0); rec.Code != http.StatusOK {
		t.Fatalf("unir zonas: %d %s", rec.Code, rec.Body)
	}
	var rects []rect
	rows, _ := db.Query(`SELECT pos_x, pos_y, width, height FROM dining_tables WHERE zone = 'Interior' AND is_active = 1
		UNION ALL SELECT pos_x, pos_y, width, height FROM floor_items WHERE zone = 'Interior'`)
	for rows.Next() {
		var r rect
		rows.Scan(&r.X, &r.Y, &r.W, &r.H)
		for _, o := range rects {
			if r.overlaps(o) {
				t.Fatalf("al unir zonas quedaron encimados %+v y %+v", r, o)
			}
		}
		rects = append(rects, r)
	}
	rows.Close()

	if rec := call(h.RequireAdmin(h.AdminDeleteFloorItem), "DELETE", "/", "", cookie, it.ID); rec.Code != http.StatusOK {
		t.Fatalf("borrar elemento: %d", rec.Code)
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM audit_logs WHERE action = ''`).Scan(&n)
	if n != 0 {
		t.Fatal("acomodar el plano no debe llenar la bitácora")
	}
}

func TestFloorShowsOccupiedTables(t *testing.T) {
	db, h := setupCashDB(t)
	exec(t, db, `UPDATE dining_tables SET status = 'OCCUPIED' WHERE id = 1`)
	exec(t, db, `INSERT INTO table_sessions (id, table_id, status, opened_at) VALUES (800, 1, 'OPEN', datetime('now', '-25 minutes'))`)
	exec(t, db, `INSERT INTO orders (id, session_id, order_type, status) VALUES (800, 800, 'DINE_IN', 'ACTIVE')`)
	exec(t, db, `INSERT INTO order_items (order_id, product_id, unit_price, quantity) VALUES (800, 2, 70, 2), (800, 1, 45, 1)`)
	exec(t, db, `INSERT INTO order_items (order_id, product_id, unit_price, quantity, status) VALUES (800, 1, 45, 1, 'VOID')`)
	exec(t, db, `INSERT INTO floor_items (zone, kind, label, pos_x, pos_y, width, height) VALUES ('Interior', 'DOOR', 'Entrada', 0, 9, 2, 1)`)

	rec := httptest.NewRecorder()
	h.GetFloor(rec, httptest.NewRequest("GET", "/api/tables/floor", nil))
	var out struct{ Zones []FloorZone }
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err, rec.Body)
	}
	var found bool
	for _, z := range out.Zones {
		for _, tb := range z.Tables {
			if tb.ID == 1 {
				found = true
				if tb.Total != 185 || tb.Minutes < 24 || tb.Minutes > 26 {
					t.Fatalf("mesa ocupada: consumo %v (quería 185), %d min (quería 25)", tb.Total, tb.Minutes)
				}
				if z.Name != "Interior" || len(z.Items) != 1 || z.Items[0].Kind != "DOOR" {
					t.Fatalf("zona %q con elementos %+v", z.Name, z.Items)
				}
			}
		}
	}
	if !found {
		t.Fatal("la mesa 1 no apareció en el plano")
	}
}

func TestAppStatus(t *testing.T) {
	db, h := setupCashDB(t)
	exec(t, db, `UPDATE dining_tables SET status = 'OCCUPIED' WHERE id = 1`)
	srv := TrackTerminals(http.HandlerFunc(h.AppStatus))

	// Una terminal de la red (iPad) no puede consultarlo, pero queda registrada.
	req := httptest.NewRequest("GET", "/api/app/status", nil)
	req.RemoteAddr = "192.168.1.50:51000"
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("desde la red debe negarse, dio %d", rec.Code)
	}

	req = httptest.NewRequest("GET", "/api/app/status", nil)
	req.RemoteAddr = "127.0.0.1:52000"
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	var out struct {
		OpenTables int      `json:"open_tables"`
		Terminals  []string `json:"terminals"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusOK || out.OpenTables != 1 || len(out.Terminals) != 1 || out.Terminals[0] != "192.168.1.50" {
		t.Fatalf("estado: %d %+v", rec.Code, out)
	}
}
