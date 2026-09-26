package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// utcAt da el valor que SQLite guardaría con CURRENT_TIMESTAMP para una hora local.
func utcAt(y int, m time.Month, d, h int) string {
	return time.Date(y, m, d, h, 0, 0, 0, time.Local).UTC().Format("2006-01-02 15:04:05")
}

func TestSalesReport(t *testing.T) {
	db, h := setupCashDB(t)
	cookie := adminLogin(t, h, "0001")

	exec(t, db, `INSERT INTO table_sessions (id, table_id, status) VALUES (800, 1, 'CLOSED'), (801, NULL, 'CLOSED')`)
	exec(t, db, `INSERT INTO orders (id, session_id, order_type, status) VALUES (800, 800, 'DINE_IN', 'PAID'), (801, 801, 'FLASH', 'PAID')`)
	// Lunes 21 de sept. a las 13h y martes 23 a las 9h; el 22 no hubo ventas.
	exec(t, db, `INSERT INTO bills (id, order_id, bill_number, subtotal, discount, tax, total, tip, status, paid_at) VALUES
		(80, 800, 1, 110, 0, 15.17, 110, 10, 'PAID', ?),
		(81, 801, 1, 120, 20, 13.79, 100, 0, 'PAID', ?)`, utcAt(2026, 9, 21, 13), utcAt(2026, 9, 23, 9))
	exec(t, db, `INSERT INTO payments (bill_id, payment_method, amount) VALUES (80, 'CASH', 60), (80, 'CARD_DEBIT', 60), (81, 'CASH', 100)`)
	exec(t, db, `INSERT INTO order_items (order_id, bill_id, product_id, unit_price, quantity) VALUES
		(800, 80, 1, 45, 1), (800, 80, 2, 65, 1), (801, 81, 3, 120, 1)`)

	rec := call(h.RequireAdmin(h.AdminSalesReport), "GET", "/api/admin/reports?from=2026-09-21&to=2026-09-23", "", cookie, 0)
	if rec.Code != http.StatusOK {
		t.Fatalf("reporte: %d %s", rec.Code, rec.Body)
	}
	var rep SalesReport
	json.Unmarshal(rec.Body.Bytes(), &rep)

	k := rep.KPIs
	if k.Sales != 210 || k.Bills != 2 || k.Tips != 10 || k.Discounts != 20 || k.AvgTicket != 105 || k.Items != 3 {
		t.Fatalf("indicadores: %+v", k)
	}
	if len(rep.Daily) != 3 || rep.Daily[1].Sales != 0 || rep.Daily[2].Sales != 100 {
		t.Fatalf("los días sin venta deben aparecer en cero: %+v", rep.Daily)
	}
	if rep.Hourly[13].Sales != 110 || rep.Hourly[9].Bills != 1 {
		t.Fatalf("por hora: 13h=%+v 9h=%+v", rep.Hourly[13], rep.Hourly[9])
	}
	if len(rep.Methods) != 2 || rep.Methods[0].Key != "CASH" || rep.Methods[0].Amount != 160 || rep.Methods[0].Tips != 5 {
		t.Fatalf("métodos: %+v", rep.Methods)
	}
	// Semana del lunes 21: 5 de propina en efectivo y 5 con tarjeta.
	if len(rep.TipWeeks) != 1 || rep.TipWeeks[0].WeekStart != "2026-09-21" || rep.TipWeeks[0].Cash != 5 || rep.TipWeeks[0].Other != 5 {
		t.Fatalf("propinas por semana: %+v", rep.TipWeeks)
	}
	if rep.Control.DiscountedBills != 1 || rep.Previous.From != "2026-09-18" || rep.Previous.To != "2026-09-20" {
		t.Fatalf("control/periodo anterior: %+v %+v", rep.Control, rep.Previous)
	}

	csvRec := call(h.RequireAdmin(h.AdminBillsCSV), "GET", "/api/admin/reports/cuentas.csv?from=2026-09-21&to=2026-09-23", "", cookie, 0)
	lines := strings.Split(strings.TrimSpace(csvRec.Body.String()), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "\uFEFFFecha") || !strings.Contains(lines[1], "60.00,60.00,0.00") {
		t.Fatalf("CSV:\n%s", csvRec.Body.String())
	}

	if rec := call(h.RequireAdmin(h.AdminSalesReport), "GET", "/api/admin/reports?from=2026-09-23&to=2026-09-01", "", cookie, 0); rec.Code != http.StatusBadRequest {
		t.Fatalf("un rango invertido debe rechazarse, dio %d", rec.Code)
	}
}

func TestAdminZones(t *testing.T) {
	db, h := setupCashDB(t)
	cookie := adminLogin(t, h, "0001")
	rename := h.RequireAdmin(h.AdminRenameZone)
	archive := h.RequireAdmin(h.AdminArchiveZone)

	// Semilla: Mesa 1 y 2 en Interior, Mesa 3 en Terraza, Barra 1 en Barra.
	if rec := call(rename, "POST", "/", `{"zone": "Interior", "to": "Salón"}`, cookie, 0); rec.Code != http.StatusOK {
		t.Fatalf("renombrar: %d %s", rec.Code, rec.Body)
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM dining_tables WHERE zone = 'Salón'`).Scan(&n)
	if n != 2 {
		t.Fatalf("las 2 mesas de Interior deben pasar a Salón, quedaron %d", n)
	}

	exec(t, db, `INSERT INTO table_sessions (table_id, status) VALUES (4, 'OPEN')`)
	if rec := call(archive, "POST", "/", `{"zone": "Barra"}`, cookie, 0); rec.Code != http.StatusConflict {
		t.Fatalf("no se archiva una zona con mesas ocupadas, dio %d", rec.Code)
	}
	exec(t, db, `UPDATE table_sessions SET status = 'CLOSED' WHERE table_id = 4`)
	if rec := call(archive, "POST", "/", `{"zone": "Barra"}`, cookie, 0); rec.Code != http.StatusOK {
		t.Fatalf("archivar zona: %d %s", rec.Code, rec.Body)
	}
	var active bool
	db.QueryRow(`SELECT is_active FROM dining_tables WHERE id = 4`).Scan(&active)
	if active {
		t.Fatal("Barra 1 debe quedar archivada")
	}
}

func TestCSVTextNeutralizesFormulas(t *testing.T) {
	cases := map[string]string{
		"=HYPERLINK(\"http://x\")": "'=HYPERLINK(\"http://x\")",
		"+52 555":                  "'+52 555",
		"-5%":                      "'-5%",
		"@SUM(A1)":                 "'@SUM(A1)",
		"Mesa 4":                   "Mesa 4",
		"":                         "",
	}
	for in, want := range cases {
		if got := csvText(in); got != want {
			t.Errorf("csvText(%q) = %q, quería %q", in, got, want)
		}
	}
}
