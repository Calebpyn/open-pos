package handler

import (
	"bytes"
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/calebpyn/open-pos/internal/printer"
)

// setupCashDB deja la base sin impresoras activas para que las pruebas no
// manden trabajos (ni abran el cajón) en impresoras reales.
func setupCashDB(t *testing.T) (*sql.DB, *POSHandler) {
	t.Helper()
	db := setupComandaDB(t)
	exec(t, db, `UPDATE printers SET is_active = 0`)
	return db, NewPOSHandler(db)
}

func TestUserByPIN(t *testing.T) {
	db, _ := setupCashDB(t)
	ctx := context.Background()
	u, err := userByPIN(ctx, db, "0001")
	if err != nil || u.Name != "Usuario general" {
		t.Fatalf("el PIN 0001 debe ser el usuario general: %v %+v", err, u)
	}
	for _, pin := range []string{"1234", "", "00a1", "0001; DROP"} {
		if _, err := userByPIN(ctx, db, pin); err != errBadPIN {
			t.Errorf("PIN %q debería rechazarse, dio %v", pin, err)
		}
	}
}

func TestCashShiftReconciliation(t *testing.T) {
	db, h := setupCashDB(t)
	ctx := context.Background()

	exec(t, db, `INSERT INTO table_sessions (id, table_id, status) VALUES (600, 1, 'OPEN')`)
	exec(t, db, `INSERT INTO orders (id, session_id, order_type, status) VALUES (600, 600, 'DINE_IN', 'ACTIVE')`)
	exec(t, db, `INSERT INTO order_items (id, order_id, product_id, unit_price, quantity) VALUES (61, 600, 1, 45, 1), (62, 600, 2, 65, 1)`)
	payBody := `{"item_ids": [61, 62], "tip": 10, "payments": [
		{"method": "CASH", "amount": 60, "received": 100},
		{"method": "CARD_DEBIT", "amount": 60}]}`

	// Sin turno abierto no se cobra.
	if rec := post(t, h.PayOrder, 600, payBody); rec.Code != http.StatusConflict {
		t.Fatalf("sin turno el cobro debe rechazarse, código %d", rec.Code)
	}

	if rec := post(t, h.OpenCash, 0, `{"pin": "9999", "opening_float": 500}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("PIN incorrecto debe dar 401, dio %d", rec.Code)
	}
	if rec := post(t, h.OpenCash, 0, `{"pin": "0001", "opening_float": 500}`); rec.Code != http.StatusCreated {
		t.Fatalf("abrir turno: %d %s", rec.Code, rec.Body)
	}
	if rec := post(t, h.OpenCash, 0, `{"pin": "0001", "opening_float": 100}`); rec.Code != http.StatusConflict {
		t.Fatalf("no puede haber dos turnos abiertos, código %d", rec.Code)
	}

	if rec := post(t, h.PayOrder, 600, payBody); rec.Code != http.StatusCreated {
		t.Fatalf("cobro: %d %s", rec.Code, rec.Body)
	}
	if rec := post(t, h.CreateCashMovement, 0,
		`{"pin": "0001", "kind": "EXPENSE", "amount": 30, "reason": "Hielo"}`); rec.Code != http.StatusCreated {
		t.Fatalf("movimiento: %d %s", rec.Code, rec.Body)
	}
	if rec := post(t, h.CreateCashMovement, 0,
		`{"pin": "0001", "kind": "EXPENSE", "amount": 30, "reason": " "}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("un gasto sin concepto debe rechazarse, código %d", rec.Code)
	}

	// Esperado: fondo 500 + efectivo 60 - gasto 30 = 530. Se cuentan 520.
	rec := post(t, h.CloseCash, 0, `{"pin": "0001",
		"counts": {"B500": 1, "B20": 1},
		"reported_card": 55, "reported_card_tips": 5}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("cierre: %d %s", rec.Code, rec.Body)
	}

	var sessionID int64
	db.QueryRow(`SELECT id FROM cash_sessions`).Scan(&sessionID)
	rep, err := loadCashReport(ctx, db, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	s := rep.Session
	if !s.Closed() || s.ExpectedCash != 530 || s.CountedCash != 520 || s.CashDiff() != -10 {
		t.Fatalf("arqueo de efectivo: esperado %v contado %v dif %v", s.ExpectedCash, s.CountedCash, s.CashDiff())
	}
	// La propina de 10 se reparte 5 y 5 entre efectivo y tarjeta.
	if rep.Bills != 1 || rep.Sales != 110 || rep.Tips != 10 || rep.CashTips != 5 || rep.CardTips != 5 {
		t.Fatalf("ventas: %d cuentas, %v ventas, %v propinas (%v efectivo, %v tarjeta)",
			rep.Bills, rep.Sales, rep.Tips, rep.CashTips, rep.CardTips)
	}
	arqueo := rep.Arqueo()
	if len(arqueo) != 2 || arqueo[0].Verdict() != "Faltante" || arqueo[1].Diff() != 0 {
		t.Fatalf("arqueo: %+v", arqueo)
	}

	// Cerrado el turno, se vuelve a bloquear el cobro.
	exec(t, db, `INSERT INTO table_sessions (id, table_id, status) VALUES (601, 2, 'OPEN')`)
	exec(t, db, `INSERT INTO orders (id, session_id, order_type, status) VALUES (601, 601, 'DINE_IN', 'ACTIVE')`)
	exec(t, db, `INSERT INTO order_items (id, order_id, product_id, unit_price, quantity) VALUES (63, 601, 1, 45, 1)`)
	if rec := post(t, h.PayOrder, 601, `{"item_ids": [63], "payments": [{"method": "CASH", "amount": 45}]}`); rec.Code != http.StatusConflict {
		t.Fatalf("con el turno cerrado el cobro debe rechazarse, código %d", rec.Code)
	}

	// El corte se dibuja sin errores de plantilla (las rutas son relativas a la raíz).
	t.Chdir("../..")
	ui := NewUIHandler(h)
	page := httptest.NewRequest(http.MethodGet, "/", nil)
	page.SetPathValue("id", strconv.FormatInt(sessionID, 10))
	out := httptest.NewRecorder()
	ui.ServeCashReport(out, page)
	body := out.Body.String()
	if out.Code != http.StatusOK || !strings.Contains(body, "Faltante") || !strings.Contains(body, "Hielo") {
		t.Fatalf("corte: %d\n%s", out.Code, body)
	}
	if ticket := cashReportDoc(rep, 80).Bytes(); !bytes.Contains(ticket, []byte("CORTE DE CAJA")) {
		t.Fatal("el corte impreso debe tener encabezado")
	}
}

func TestOpenDrawerIsAudited(t *testing.T) {
	db, h := setupCashDB(t)
	fp := newFakePrinter(t)
	exec(t, db, `UPDATE printers SET is_active = 1, prints_receipts = 1, connection_type = 'NETWORK',
		ip_address = '127.0.0.1', port = ? WHERE id = 1`, fp.port())

	// Sin cajón configurado no se manda ningún pulso a la impresora de tickets.
	if rec := post(t, h.OpenDrawer, 0, `{"source": "cobro"}`); rec.Code != http.StatusConflict {
		t.Fatalf("sin cajón configurado debe dar 409, dio %d", rec.Code)
	}
	exec(t, db, `UPDATE printers SET has_drawer = 1 WHERE id = 1`)
	// Sin PIN: basta el botón.
	if rec := post(t, h.OpenDrawer, 0, `{"source": "cobro"}`); rec.Code != http.StatusOK {
		t.Fatalf("abrir cajón: %d %s", rec.Code, rec.Body)
	}
	if job := fp.jobCount(t, 1)[0]; !bytes.Equal(job, printer.DrawerKick()) {
		t.Fatalf("se esperaba el pulso del cajón, llegó %v", job)
	}
	action, d := lastAudit(t, db)
	if action != "DRAWER_OPENED" || d["source"] != "Cobro" || d["opened"] != true {
		t.Fatalf("bitácora: %s %v", action, d)
	}
	// El diagnóstico muestra el trabajo del cajón.
	diag := call(h.RequireAdmin(h.AdminDiagnostics), "GET", "/", "", adminLogin(t, h, "0001"), 0).Body.String()
	if !strings.Contains(diag, "cajón") || !strings.Contains(diag, "1b 70 00") {
		t.Fatalf("el diagnóstico debe listar el pulso del cajón:\n%s", diag)
	}
}

func TestCashPINRateLimit(t *testing.T) {
	_, h := setupCashDB(t)
	for i := 0; i < loginMaxFails; i++ {
		if rec := post(t, h.OpenCash, 0, `{"pin": "9999", "opening_float": 100}`); rec.Code != http.StatusUnauthorized {
			t.Fatalf("intento %d: %d", i+1, rec.Code)
		}
	}
	// Bloqueado: ni el PIN correcto pasa.
	if rec := post(t, h.OpenCash, 0, `{"pin": "0001", "opening_float": 100}`); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("tras %d fallos las operaciones de caja deben bloquearse, dio %d", loginMaxFails, rec.Code)
	}
}

// El cajón se abre solo con su botón: ni abrir turno, ni cobrar en efectivo
// mandan el pulso, así el ticket nunca espera al cajón.
func TestDrawerIsIndependentOfPayment(t *testing.T) {
	db, h := setupCashDB(t)
	fp := newFakePrinter(t)
	exec(t, db, `UPDATE printers SET is_active = 1, prints_receipts = 1, has_drawer = 1, connection_type = 'NETWORK',
		ip_address = '127.0.0.1', port = ? WHERE id = 1`, fp.port())
	exec(t, db, `INSERT INTO table_sessions (id, table_id, status) VALUES (600, 1, 'OPEN')`)
	exec(t, db, `INSERT INTO orders (id, session_id, order_type, status) VALUES (600, 600, 'DINE_IN', 'ACTIVE')`)
	exec(t, db, `INSERT INTO order_items (id, order_id, product_id, unit_price, quantity) VALUES (61, 600, 1, 45, 1)`)
	post(t, h.OpenCash, 0, `{"pin": "0001", "opening_float": 500}`)
	if rec := post(t, h.PayOrder, 600, `{"item_ids": [61], "payments": [{"method": "CASH", "amount": 45, "received": 100}]}`); rec.Code != http.StatusCreated {
		t.Fatalf("cobro en efectivo: %d %s", rec.Code, rec.Body)
	}
	if rec := post(t, h.PrintBill, 1, ""); rec.Code != http.StatusOK {
		t.Fatalf("ticket: %d %s", rec.Code, rec.Body)
	}
	jobs := fp.jobCount(t, 1)
	if !bytes.Contains(jobs[0], []byte("TOTAL")) {
		t.Fatalf("el único trabajo debe ser el ticket, llegó %q", jobs[0])
	}
}
