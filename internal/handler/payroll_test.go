package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestPayrollCharge(t *testing.T) {
	db, h := setupCashDB(t)
	cookie := adminLogin(t, h, "0001")
	fp := newFakePrinter(t)
	exec(t, db, `UPDATE printers SET is_active = 1, prints_receipts = 1, connection_type = 'NETWORK', ip_address = '127.0.0.1', port = ? WHERE id = 1`, fp.port())

	// La migración crea un colaborador por usuario; se agrega uno de cocina con tope.
	var seeded int
	db.QueryRow(`SELECT COUNT(*) FROM collaborators`).Scan(&seeded)
	if seeded == 0 {
		t.Fatal("la migración debe crear colaboradores a partir de los usuarios")
	}
	rec := call(h.RequireAdmin(h.AdminCreateCollaborator), "POST", "/", `{"name": "Ana (cocina)", "weekly_limit": 150}`, cookie, 0)
	var created struct{ Collaborator Collaborator }
	json.Unmarshal(rec.Body.Bytes(), &created)
	ana := strconv.FormatInt(created.Collaborator.ID, 10)

	exec(t, db, `INSERT INTO table_sessions (id, table_id, status) VALUES (600, NULL, 'OPEN')`)
	exec(t, db, `INSERT INTO orders (id, session_id, order_type, status) VALUES (600, 600, 'FLASH', 'ACTIVE')`)
	exec(t, db, `INSERT INTO order_items (id, order_id, product_id, unit_price, quantity) VALUES (61, 600, 2, 100, 1), (62, 600, 1, 100, 1)`)
	post(t, h.OpenCash, 0, `{"pin": "0001", "opening_float": 500}`)
	fp.jobCount(t, 0)

	pay := func(items, payments, pin string) *payResult {
		body := `{"item_ids": ` + items + `, "payroll_pin": "` + pin + `", "payments": ` + payments + `}`
		rec := post(t, h.PayOrder, 600, body)
		return &payResult{rec.Code, rec.Body.String()}
	}
	charge := `[{"method": "PAYROLL", "amount": 100, "collaborator_id": ` + ana + `}]`
	if r := pay(`[61]`, charge, ""); r.code != http.StatusUnauthorized {
		t.Fatalf("sin PIN de caja no se autoriza: %d %s", r.code, r.body)
	}
	if r := pay(`[61]`, `[{"method": "PAYROLL", "amount": 100}]`, "0001"); r.code != http.StatusBadRequest {
		t.Fatalf("sin colaborador debe rechazarse: %d", r.code)
	}
	if r := pay(`[61]`, charge, "0001"); r.code != http.StatusCreated {
		t.Fatalf("cargo a nómina: %d %s", r.code, r.body)
	}
	var collab, authorized int64
	db.QueryRow(`SELECT collaborator_id, authorized_by FROM payments WHERE payment_method = 'PAYROLL'`).Scan(&collab, &authorized)
	if collab != created.Collaborator.ID || authorized == 0 {
		t.Fatalf("el pago debe guardar colaborador (%d) y quién autorizó (%d)", collab, authorized)
	}
	voucher := fp.jobCount(t, 1)[0]
	for _, want := range []string{"VALE DE CONSUMO", "Ana (cocina)", "A DESCONTAR", "$100.00", "Firma del colaborador", "Usuario general"} {
		if !bytes.Contains(voucher, []byte(want)) {
			t.Fatalf("el vale debe incluir %q:\n%s", want, voucher)
		}
	}
	if action, d := lastAudit(t, db); action != "BILL_PAID" && action != "PAYROLL_CHARGE" {
		t.Fatalf("bitácora: %s %v", action, d)
	}

	// Tope semanal de $150: otro cargo de $100 lo rebasa.
	if r := pay(`[62]`, charge, "0001"); r.code != http.StatusConflict {
		t.Fatalf("rebasar el tope debe rechazarse: %d %s", r.code, r.body)
	}
	// Mixto: $50 a nómina (llega a $150) y $50 en efectivo.
	if r := pay(`[62]`, `[{"method": "PAYROLL", "amount": 50, "collaborator_id": `+ana+`}, {"method": "CASH", "amount": 50}]`, "0001"); r.code != http.StatusCreated {
		t.Fatalf("cargo mixto: %d %s", r.code, r.body)
	}

	// Corte: los cargos van aparte y no cambian el efectivo esperado.
	var sid int64
	db.QueryRow(`SELECT id FROM cash_sessions`).Scan(&sid)
	rep, err := loadCashReport(context.Background(), db, sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Payroll) != 1 || rep.Payroll[0].Amount != 150 || rep.Payroll[0].Count != 2 || rep.ExpectedCash != 550 {
		t.Fatalf("corte: nómina %+v, efectivo esperado %v", rep.Payroll, rep.ExpectedCash)
	}

	// Semana: $150 pendientes; al marcar descontado quedan en 0; se puede deshacer.
	week := func() (rows []PayrollRow) {
		var out struct{ Rows []PayrollRow }
		json.Unmarshal(call(h.RequireAdmin(h.AdminPayrollWeek), "GET", "/", "", cookie, 0).Body.Bytes(), &out)
		return out.Rows
	}
	if rows := week(); len(rows) != 1 || rows[0].Total != 150 || rows[0].Pending != 150 || len(rows[0].Entries) != 2 {
		t.Fatalf("semana: %+v", rows)
	}
	monday := weekStart(time.Now()).Format(dateLayout)
	if rec := call(h.RequireAdmin(h.AdminSettlePayroll), "POST", "/", `{"collaborator_id": `+ana+`, "week": "`+monday+`"}`, cookie, 0); rec.Code != http.StatusOK {
		t.Fatalf("descontar: %d %s", rec.Code, rec.Body)
	}
	rows := week()
	if rows[0].Pending != 0 || len(rows[0].SettlementIDs) != 1 {
		t.Fatalf("tras descontar: %+v", rows[0])
	}
	if rec := call(h.RequireAdmin(h.AdminSettlePayroll), "POST", "/", `{"collaborator_id": `+ana+`, "week": "`+monday+`"}`, cookie, 0); rec.Code != http.StatusConflict {
		t.Fatalf("no debe descontarse dos veces: %d", rec.Code)
	}
	if rec := call(h.RequireAdmin(h.AdminUndoSettlement), "DELETE", "/", "", cookie, rows[0].SettlementIDs[0]); rec.Code != http.StatusOK {
		t.Fatalf("deshacer: %d", rec.Code)
	}
	if rows := week(); rows[0].Pending != 150 {
		t.Fatalf("tras deshacer vuelve a pendiente: %+v", rows[0])
	}
}

type payResult struct {
	code int
	body string
}

func TestReclassifyOtherAsPayroll(t *testing.T) {
	db, h := setupCashDB(t)
	cookie := adminLogin(t, h, "0001")
	exec(t, db, `INSERT INTO table_sessions (id, table_id, status) VALUES (600, NULL, 'OPEN')`)
	exec(t, db, `INSERT INTO orders (id, session_id, order_type, status) VALUES (600, 600, 'FLASH', 'ACTIVE')`)
	exec(t, db, `INSERT INTO order_items (id, order_id, product_id, unit_price, quantity) VALUES (61, 600, 2, 80, 1)`)
	post(t, h.OpenCash, 0, `{"pin": "0001", "opening_float": 0}`)
	post(t, h.PayOrder, 600, `{"item_ids": [61], "payments": [{"method": "OTHER", "amount": 80}]}`)
	var pid, cid int64
	db.QueryRow(`SELECT id FROM payments`).Scan(&pid)
	db.QueryRow(`SELECT MIN(id) FROM collaborators`).Scan(&cid)

	if rec := call(h.RequireAdmin(h.AdminReclassifyPayment), "PUT", "/", `{"collaborator_id": `+strconv.FormatInt(cid, 10)+`}`, cookie, pid); rec.Code != http.StatusOK {
		t.Fatalf("reclasificar: %d %s", rec.Code, rec.Body)
	}
	var method string
	var collab int64
	db.QueryRow(`SELECT payment_method, collaborator_id FROM payments WHERE id = ?`, pid).Scan(&method, &collab)
	if method != "PAYROLL" || collab != cid {
		t.Fatalf("pago: %s %d", method, collab)
	}
	if action, _ := lastAudit(t, db); action != "PAYMENT_RECLASSIFIED" {
		t.Fatalf("debe quedar en la bitácora, quedó %s", action)
	}
	if rec := call(h.RequireAdmin(h.AdminReclassifyPayment), "PUT", "/", `{"collaborator_id": 1}`, cookie, pid); rec.Code != http.StatusConflict {
		t.Fatalf("solo se reclasifican pagos 'Otro', dio %d", rec.Code)
	}
}
