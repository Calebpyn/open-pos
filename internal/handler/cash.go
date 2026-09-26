package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Denomination es un tipo de billete o moneda que se cuenta en el arqueo.
type Denomination struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Cents int64  `json:"cents"`
	Coin  bool   `json:"coin"`
}

var denominations = []Denomination{
	{"B1000", "$1,000", 100000, false},
	{"B500", "$500", 50000, false},
	{"B200", "$200", 20000, false},
	{"B100", "$100", 10000, false},
	{"B50", "$50", 5000, false},
	{"B20", "$20", 2000, false},
	{"M20", "$20", 2000, true},
	{"M10", "$10", 1000, true},
	{"M5", "$5", 500, true},
	{"M2", "$2", 200, true},
	{"M1", "$1", 100, true},
	{"M050", "50¢", 50, true},
}

// MovementKind es un tipo de entrada o salida de efectivo que no es venta.
type MovementKind struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	In    bool   `json:"in"`
}

var movementKinds = []MovementKind{
	{"EXPENSE", "Gasto / pago a proveedor", false},
	{"WITHDRAWAL", "Retiro a caja fuerte", false},
	{"TIP_PAYOUT", "Pago de propinas", false},
	{"OTHER_OUT", "Otra salida", false},
	{"DEPOSIT", "Entrada de cambio", true},
	{"OTHER_IN", "Otra entrada", true},
}

func movementKind(key string) (MovementKind, bool) {
	for _, k := range movementKinds {
		if k.Key == key {
			return k, true
		}
	}
	return MovementKind{}, false
}

// Métodos en el orden en que aparecen en el corte.
var methodOrder = []string{"CASH", "CARD_DEBIT", "CARD_CREDIT", "TRANSFER", "PAYROLL", "OTHER"}

type CashSession struct {
	ID               int64          `json:"id"`
	Status           string         `json:"status"`
	OpenedAt         time.Time      `json:"opened_at"`
	OpenedBy         string         `json:"opened_by"`
	OpeningFloat     float64        `json:"opening_float"`
	OpeningNotes     string         `json:"opening_notes"`
	ClosedAt         *time.Time     `json:"closed_at"`
	ClosedBy         string         `json:"closed_by"`
	CountedCash      float64        `json:"-"`
	CountDetail      map[string]int `json:"-"`
	ExpectedCash     float64        `json:"-"`
	ReportedCard     float64        `json:"-"`
	ReportedCardTips float64        `json:"-"`
	ExpectedCard     float64        `json:"-"`
	ReportedTransfer *float64       `json:"-"`
	ExpectedTransfer float64        `json:"-"`
	CloseNotes       string         `json:"-"`
}

func (s CashSession) Closed() bool { return s.Status == "CLOSED" }

func (s CashSession) CashDiff() float64 {
	return fromCents(toCents(s.CountedCash) - toCents(s.ExpectedCash))
}

var errNoCashSession = errors.New("No hay un turno de caja abierto. Ábrelo en 💵 Caja antes de cobrar.")

// openCashSessionID regresa el turno abierto o errNoCashSession.
func openCashSessionID(ctx context.Context, q queryer) (int64, error) {
	var id int64
	err := q.QueryRowContext(ctx, `SELECT id FROM cash_sessions WHERE status = 'OPEN'`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, errNoCashSession
	}
	return id, err
}

func loadCashSession(ctx context.Context, q queryer, id int64) (*CashSession, error) {
	s := &CashSession{}
	var closedAt sql.NullTime
	var closedBy, openingNotes, countDetail, closeNotes sql.NullString
	var counted, expected, repCard, repTips, expCard, repTransfer, expTransfer sql.NullFloat64
	err := q.QueryRowContext(ctx, `
		SELECT cs.id, cs.status, cs.opened_at, uo.name, cs.opening_float, cs.opening_notes,
		       cs.closed_at, uc.name, cs.counted_cash, cs.count_detail, cs.expected_cash,
		       cs.reported_card, cs.reported_card_tips, cs.expected_card,
		       cs.reported_transfer, cs.expected_transfer, cs.close_notes
		FROM cash_sessions cs
		JOIN users uo ON uo.id = cs.opened_by
		LEFT JOIN users uc ON uc.id = cs.closed_by
		WHERE cs.id = ?`, id).
		Scan(&s.ID, &s.Status, &s.OpenedAt, &s.OpenedBy, &s.OpeningFloat, &openingNotes,
			&closedAt, &closedBy, &counted, &countDetail, &expected,
			&repCard, &repTips, &expCard, &repTransfer, &expTransfer, &closeNotes)
	if err != nil {
		return nil, err
	}
	if closedAt.Valid {
		s.ClosedAt = &closedAt.Time
	}
	s.OpeningNotes, s.ClosedBy, s.CloseNotes = openingNotes.String, closedBy.String, closeNotes.String
	s.CountedCash, s.ExpectedCash = counted.Float64, expected.Float64
	s.ReportedCard, s.ReportedCardTips, s.ExpectedCard = repCard.Float64, repTips.Float64, expCard.Float64
	s.ExpectedTransfer = expTransfer.Float64
	if repTransfer.Valid {
		s.ReportedTransfer = &repTransfer.Float64
	}
	if countDetail.Valid {
		json.Unmarshal([]byte(countDetail.String), &s.CountDetail)
	}
	return s, nil
}

type CashMovement struct {
	ID        int64     `json:"id"`
	Kind      string    `json:"kind"`
	Label     string    `json:"label"`
	In        bool      `json:"in"`
	Amount    float64   `json:"amount"`
	Reason    string    `json:"reason"`
	User      string    `json:"user"`
	CreatedAt time.Time `json:"created_at"`
}

func loadMovements(ctx context.Context, q queryer, sessionID int64) ([]CashMovement, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT m.id, m.kind, m.amount, m.reason, u.name, m.created_at
		FROM cash_movements m JOIN users u ON u.id = m.user_id
		WHERE m.cash_session_id = ? ORDER BY m.id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CashMovement{}
	for rows.Next() {
		var m CashMovement
		if err := rows.Scan(&m.ID, &m.Kind, &m.Amount, &m.Reason, &m.User, &m.CreatedAt); err != nil {
			return nil, err
		}
		k, _ := movementKind(m.Kind)
		m.Label, m.In = k.Label, k.In
		out = append(out, m)
	}
	return out, rows.Err()
}

// --- Corte de caja ---

type MethodTotal struct {
	Method string
	Label  string
	Count  int
	Amount float64 // lo cobrado con este método, propinas incluidas
	Tips   float64 // parte de las propinas que se pagó con este método
}

type ControlEvent struct {
	At     time.Time
	Title  string
	Detail string
	Reason string
	Amount float64
}

type ProductLine struct {
	Name     string
	Quantity int
	Amount   float64
}

type SessionNote struct {
	User      string
	Note      string
	CreatedAt time.Time
}

// Reconciliation compara lo que reportó el cajero contra lo que esperaba el sistema.
type Reconciliation struct {
	Label    string
	Expected float64
	Reported float64
	Note     string
}

func (r Reconciliation) Diff() float64 { return fromCents(toCents(r.Reported) - toCents(r.Expected)) }

func (r Reconciliation) DiffText() string { return signedMoney(r.Diff()) }

func (r Reconciliation) Verdict() string {
	switch d := toCents(r.Diff()); {
	case d == 0:
		return "Cuadra"
	case d < 0:
		return "Faltante"
	default:
		return "Sobrante"
	}
}

func (r Reconciliation) Tone() string {
	switch d := toCents(r.Diff()); {
	case d == 0:
		return "bg-emerald-50 border-emerald-200 text-emerald-800"
	case d < 0:
		return "bg-rose-50 border-rose-200 text-rose-800"
	default:
		return "bg-amber-50 border-amber-200 text-amber-800"
	}
}

type CashReport struct {
	Settings
	Session *CashSession

	Bills     int
	Subtotal  float64
	Discounts float64
	Tax       float64
	Sales     float64 // total de las cuentas, sin propina
	Tips      float64
	Collected float64 // ventas + propinas
	AvgTicket float64

	Methods      []MethodTotal
	CashSales    float64
	CashTips     float64
	CardExpected float64
	CardTips     float64
	Transfers    float64
	Payroll      []PayrollLine // cargos a nómina del turno, por colaborador

	Movements    []CashMovement
	MovementsIn  float64
	MovementsOut float64
	ExpectedCash float64

	Discounted    []ControlEvent
	Cancellations []ControlEvent
	Voids         []ControlEvent
	DrawerOpens   []ControlEvent
	TopProducts   []ProductLine
	Notes         []SessionNote
	Denominations []Denomination

	OpenOrders        int
	OpenOrdersPending float64
}

type PayrollLine struct {
	Name   string
	Count  int
	Amount float64
}

// Arqueo solo existe en turnos cerrados: antes de cerrar es ciego.
func (r *CashReport) Arqueo() []Reconciliation {
	s := r.Session
	if !s.Closed() {
		return nil
	}
	out := []Reconciliation{
		{Label: "Efectivo en caja", Expected: s.ExpectedCash, Reported: s.CountedCash,
			Note: fmt.Sprintf("Incluye %s de propinas en efectivo", formatMoney(r.CashTips))},
		{Label: "Terminal Mercado Pago (tarjetas)", Expected: s.ExpectedCard,
			Reported: fromCents(toCents(s.ReportedCard) + toCents(s.ReportedCardTips)),
			Note: fmt.Sprintf("Ventas %s + propinas %s según la terminal · propinas en el POS: %s",
				formatMoney(s.ReportedCard), formatMoney(s.ReportedCardTips), formatMoney(r.CardTips))},
	}
	if s.ReportedTransfer != nil {
		out = append(out, Reconciliation{Label: "Transferencias", Expected: s.ExpectedTransfer, Reported: *s.ReportedTransfer})
	}
	return out
}

func (r *CashReport) CountLines() []struct {
	Denomination
	Pieces int
	Amount float64
} {
	var out []struct {
		Denomination
		Pieces int
		Amount float64
	}
	for _, d := range denominations {
		if n := r.Session.CountDetail[d.Key]; n > 0 {
			out = append(out, struct {
				Denomination
				Pieces int
				Amount float64
			}{d, n, fromCents(d.Cents * int64(n))})
		}
	}
	return out
}

// loadCashReport arma el corte de un turno. Los montos de control salen de
// la bitácora dentro de la ventana de tiempo del turno.
func loadCashReport(ctx context.Context, q queryer, sessionID int64) (*CashReport, error) {
	s, err := loadCashSession(ctx, q, sessionID)
	if err != nil {
		return nil, err
	}
	r := &CashReport{Session: s, Denominations: denominations}
	if r.Settings, err = loadSettings(ctx, q); err != nil {
		return nil, err
	}

	// Cuentas del turno.
	var subtotal, discounts, tax, sales, tips int64
	rows, err := q.QueryContext(ctx, `
		SELECT id, order_id, bill_number, subtotal, discount, tax, total, COALESCE(tip, 0),
		       COALESCE(discount_type, ''), COALESCE(discount_value, 0), COALESCE(discount_reason, ''), paid_at
		FROM bills WHERE cash_session_id = ? AND status = 'PAID' ORDER BY id`, sessionID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, orderID int64
		var number int
		var sub, disc, tx, total, tip, discValue float64
		var discType, discReason string
		var paidAt sql.NullTime
		if err := rows.Scan(&id, &orderID, &number, &sub, &disc, &tx, &total, &tip,
			&discType, &discValue, &discReason, &paidAt); err != nil {
			rows.Close()
			return nil, err
		}
		r.Bills++
		subtotal += toCents(sub)
		discounts += toCents(disc)
		tax += toCents(tx)
		sales += toCents(total)
		tips += toCents(tip)
		if disc > 0 {
			detail := "Monto fijo"
			if discType == "PERCENT" {
				detail = fmt.Sprintf("%g%%", discValue)
			}
			if total == 0 {
				detail = "Cortesía (100%)"
			}
			r.Discounted = append(r.Discounted, ControlEvent{At: paidAt.Time,
				Title: fmt.Sprintf("Orden #%d · Cuenta %d", orderID, number), Detail: detail,
				Reason: discReason, Amount: disc})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	r.Subtotal, r.Discounts, r.Tax, r.Sales, r.Tips = fromCents(subtotal), fromCents(discounts), fromCents(tax), fromCents(sales), fromCents(tips)
	r.Collected = fromCents(sales + tips)
	if r.Bills > 0 {
		r.AvgTicket = fromCents(sales / int64(r.Bills))
	}

	// Pagos por método. La propina de una cuenta se reparte entre sus pagos
	// en proporción al monto, para saber cuánta quedó en efectivo y cuánta
	// en la terminal.
	type acc struct {
		count        int
		amount, tips int64
	}
	byMethod := map[string]*acc{}
	pRows, err := q.QueryContext(ctx, `
		SELECT p.payment_method, p.amount, b.total, COALESCE(b.tip, 0)
		FROM payments p JOIN bills b ON b.id = p.bill_id
		WHERE b.cash_session_id = ?`, sessionID)
	if err != nil {
		return nil, err
	}
	for pRows.Next() {
		var method string
		var amount, total, tip float64
		if err := pRows.Scan(&method, &amount, &total, &tip); err != nil {
			pRows.Close()
			return nil, err
		}
		a := byMethod[method]
		if a == nil {
			a = &acc{}
			byMethod[method] = a
		}
		a.count++
		a.amount += toCents(amount)
		if grand := toCents(total) + toCents(tip); grand > 0 {
			a.tips += toCents(tip) * toCents(amount) / grand
		}
	}
	pRows.Close()
	if err := pRows.Err(); err != nil {
		return nil, err
	}
	var cardC, cardTipsC int64
	for _, m := range methodOrder {
		a := byMethod[m]
		if a == nil {
			continue
		}
		r.Methods = append(r.Methods, MethodTotal{Method: m, Label: paymentMethods[m],
			Count: a.count, Amount: fromCents(a.amount), Tips: fromCents(a.tips)})
		switch m {
		case "CASH":
			r.CashSales, r.CashTips = fromCents(a.amount), fromCents(a.tips)
		case "CARD_DEBIT", "CARD_CREDIT":
			cardC += a.amount
			cardTipsC += a.tips
		case "TRANSFER":
			r.Transfers = fromCents(a.amount)
		}
	}
	r.CardExpected, r.CardTips = fromCents(cardC), fromCents(cardTipsC)

	// Cargos a nómina: no entran a caja; se listan por colaborador.
	if err := eachRow(ctx, q, `
		SELECT COALESCE(co.name, '(sin colaborador)'), COUNT(*), SUM(p.amount)
		FROM payments p JOIN bills b ON b.id = p.bill_id
		LEFT JOIN collaborators co ON co.id = p.collaborator_id
		WHERE b.cash_session_id = ? AND p.payment_method = 'PAYROLL'
		GROUP BY p.collaborator_id ORDER BY 1`, []any{sessionID}, func(scan func(...any) error) error {
		var l PayrollLine
		if err := scan(&l.Name, &l.Count, &l.Amount); err != nil {
			return err
		}
		l.Amount = fromCents(toCents(l.Amount))
		r.Payroll = append(r.Payroll, l)
		return nil
	}); err != nil {
		return nil, err
	}

	// Movimientos y efectivo esperado.
	if r.Movements, err = loadMovements(ctx, q, sessionID); err != nil {
		return nil, err
	}
	var inC, outC int64
	for _, m := range r.Movements {
		if m.In {
			inC += toCents(m.Amount)
		} else {
			outC += toCents(m.Amount)
		}
	}
	r.MovementsIn, r.MovementsOut = fromCents(inC), fromCents(outC)
	r.ExpectedCash = fromCents(toCents(s.OpeningFloat) + toCents(r.CashSales) + inC - outC)

	if err := r.loadControlEvents(ctx, q); err != nil {
		return nil, err
	}

	tRows, err := q.QueryContext(ctx, `
		SELECT p.name, SUM(oi.quantity), SUM(oi.quantity * oi.unit_price)
		FROM order_items oi
		JOIN bills b ON b.id = oi.bill_id
		JOIN products p ON p.id = oi.product_id
		WHERE b.cash_session_id = ? AND oi.status != 'VOID'
		GROUP BY p.id ORDER BY 2 DESC, 3 DESC LIMIT 10`, sessionID)
	if err != nil {
		return nil, err
	}
	for tRows.Next() {
		var p ProductLine
		if err := tRows.Scan(&p.Name, &p.Quantity, &p.Amount); err != nil {
			tRows.Close()
			return nil, err
		}
		r.TopProducts = append(r.TopProducts, p)
	}
	tRows.Close()
	if err := tRows.Err(); err != nil {
		return nil, err
	}

	nRows, err := q.QueryContext(ctx, `
		SELECT u.name, n.note, n.created_at FROM cash_session_notes n JOIN users u ON u.id = n.user_id
		WHERE n.cash_session_id = ? ORDER BY n.id`, sessionID)
	if err != nil {
		return nil, err
	}
	for nRows.Next() {
		var n SessionNote
		if err := nRows.Scan(&n.User, &n.Note, &n.CreatedAt); err != nil {
			nRows.Close()
			return nil, err
		}
		r.Notes = append(r.Notes, n)
	}
	nRows.Close()
	if err := nRows.Err(); err != nil {
		return nil, err
	}

	if !s.Closed() {
		var pending float64
		if err := q.QueryRowContext(ctx, `
			SELECT COUNT(DISTINCT o.id), COALESCE(SUM(oi.unit_price * oi.quantity), 0)
			FROM orders o
			LEFT JOIN order_items oi ON oi.order_id = o.id AND oi.bill_id IS NULL AND oi.status != 'VOID'
			WHERE o.status = 'ACTIVE'`).Scan(&r.OpenOrders, &pending); err != nil {
			return nil, err
		}
		r.OpenOrdersPending = pending
	}
	return r, nil
}

// loadControlEvents junta de la bitácora lo que ocurrió durante el turno:
// órdenes cerradas sin pago, productos anulados y aperturas manuales del cajón.
func (r *CashReport) loadControlEvents(ctx context.Context, q queryer) error {
	rows, err := q.QueryContext(ctx, `
		SELECT action, details, created_at FROM audit_logs
		WHERE action IN ('ORDER_CANCELLED', 'ORDER_CLOSED_WITH_BALANCE', 'ITEM_VOID', 'DRAWER_OPENED')
		  AND created_at >= (SELECT opened_at FROM cash_sessions WHERE id = ?1)
		  AND ((SELECT closed_at FROM cash_sessions WHERE id = ?1) IS NULL
		       OR created_at <= (SELECT closed_at FROM cash_sessions WHERE id = ?1))
		ORDER BY id`, r.Session.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var action, raw string
		var at time.Time
		if err := rows.Scan(&action, &raw, &at); err != nil {
			return err
		}
		var d map[string]any
		json.Unmarshal([]byte(raw), &d)
		str := func(k string) string { s, _ := d[k].(string); return s }
		num := func(k string) float64 { f, _ := d[k].(float64); return f }

		switch action {
		case "ORDER_CANCELLED", "ORDER_CLOSED_WITH_BALANCE":
			title := fmt.Sprintf("Orden #%.0f", num("order_id"))
			if t := str("title"); t != "" {
				title += " · " + t
			}
			var detail []string
			if action == "ORDER_CLOSED_WITH_BALANCE" {
				detail = append(detail, "Tenía cobros; se anuló el saldo")
			}
			if sent, _ := d["comanda_sent"].(bool); sent {
				detail = append(detail, "Comanda enviada")
			}
			if n := num("items_delivered"); n > 0 {
				detail = append(detail, fmt.Sprintf("%.0f producto(s) entregados", n))
			}
			r.Cancellations = append(r.Cancellations, ControlEvent{At: at, Title: title,
				Detail: strings.Join(detail, " · "), Reason: str("reason"), Amount: num("unpaid_amount")})
		case "ITEM_VOID":
			qty := num("quantity")
			r.Voids = append(r.Voids, ControlEvent{At: at,
				Title:  fmt.Sprintf("%.0f× %s", qty, str("product")),
				Detail: fmt.Sprintf("Orden #%.0f", num("order_id")),
				Reason: str("reason"), Amount: fromCents(toCents(num("unit_price")) * int64(qty))})
		case "DRAWER_OPENED":
			// Antes se abría con PIN (queda el nombre); ahora con el botón de Caja o de Cobro.
			title := str("user")
			if title == "" {
				title = "Botón en " + str("source")
				if str("source") == "" {
					title = "Botón"
				}
			}
			r.DrawerOpens = append(r.DrawerOpens, ControlEvent{At: at, Title: title, Reason: str("reason")})
		}
	}
	return rows.Err()
}

// auditBy registra una acción firmada con el PIN de un usuario.
func auditBy(ctx context.Context, tx *sql.Tx, u User, r *http.Request, action string, details map[string]any) error {
	details["user"] = u.Name
	details["terminal"] = terminalOf(r)
	b, err := json.Marshal(details)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO audit_logs (user_id, action, details) VALUES (?, ?, ?)`, u.ID, action, string(b))
	return err
}

// --- Handlers ---

type pinRequest struct {
	PIN string `json:"pin"`
}

// beginWithUser decodifica el cuerpo, abre una transacción y valida el PIN.
// Si algo falla ya respondió y regresa ok=false.
func (h *POSHandler) beginWithUser(w http.ResponseWriter, r *http.Request, req any, pin func() string) (*sql.Tx, User, bool) {
	if err := json.NewDecoder(r.Body).Decode(req); err != nil {
		http.Error(w, "JSON inválido", http.StatusBadRequest)
		return nil, User{}, false
	}
	// Mismo límite de intentos que el inicio de sesión: sin él, un PIN de 4
	// dígitos se adivina en segundos desde cualquier equipo de la red.
	ip := terminalOf(r)["ip"]
	if d := h.logins.wait(ip); d > 0 {
		http.Error(w, fmt.Sprintf("Demasiados intentos de PIN. Espera %d segundos.", int(d.Seconds())+1), http.StatusTooManyRequests)
		return nil, User{}, false
	}
	tx, err := h.DB.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "Error al iniciar transacción", err)
		return nil, User{}, false
	}
	u, err := userByPIN(r.Context(), tx, pin())
	if errors.Is(err, errBadPIN) {
		tx.Rollback()
		h.logins.fail(ip)
		http.Error(w, "PIN incorrecto", http.StatusUnauthorized)
		return nil, User{}, false
	}
	if err == nil {
		h.logins.success(ip)
	}
	if err != nil {
		tx.Rollback()
		serverError(w, "Error validando PIN", err)
		return nil, User{}, false
	}
	if !canOperateCash(u.Role) {
		tx.Rollback()
		http.Error(w, "Tu usuario ("+roleLabels[u.Role]+") no puede operar la caja", http.StatusForbidden)
		return nil, User{}, false
	}
	return tx, u, true
}

// GET /api/cash/current - Estado del turno abierto. No incluye montos de
// venta: el arqueo es ciego.
func (h *POSHandler) GetCurrentCash(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := openCashSessionID(ctx, h.DB)
	if errors.Is(err, errNoCashSession) {
		writeJSON(w, http.StatusOK, map[string]any{"session": nil})
		return
	}
	if err != nil {
		serverError(w, "Error consultando la caja", err)
		return
	}
	s, err := loadCashSession(ctx, h.DB, id)
	if err != nil {
		serverError(w, "Error cargando el turno", err)
		return
	}
	movements, err := loadMovements(ctx, h.DB, id)
	if err != nil {
		serverError(w, "Error cargando movimientos", err)
		return
	}
	var bills, openOrders int
	if err := h.DB.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM bills WHERE cash_session_id = ?),
		       (SELECT COUNT(*) FROM orders WHERE status = 'ACTIVE')`, id).Scan(&bills, &openOrders); err != nil {
		serverError(w, "Error consultando el turno", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session": s, "movements": movements, "bills": bills, "open_orders": openOrders,
	})
}

type openCashRequest struct {
	PIN          string  `json:"pin"`
	OpeningFloat float64 `json:"opening_float"`
	Notes        string  `json:"notes"`
}

// POST /api/cash/open - Abre un turno con su fondo inicial.
func (h *POSHandler) OpenCash(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req openCashRequest
	tx, u, ok := h.beginWithUser(w, r, &req, func() string { return req.PIN })
	if !ok {
		return
	}
	defer tx.Rollback()
	if req.OpeningFloat < 0 {
		http.Error(w, "El fondo no puede ser negativo", http.StatusBadRequest)
		return
	}
	if _, err := openCashSessionID(ctx, tx); err == nil {
		http.Error(w, "Ya hay un turno abierto", http.StatusConflict)
		return
	} else if !errors.Is(err, errNoCashSession) {
		serverError(w, "Error consultando la caja", err)
		return
	}

	res, err := tx.ExecContext(ctx,
		`INSERT INTO cash_sessions (opened_by, opening_float, opening_notes) VALUES (?, ?, ?)`,
		u.ID, fromCents(toCents(req.OpeningFloat)), nullIfEmpty(req.Notes))
	if err != nil {
		serverError(w, "Error abriendo el turno", err)
		return
	}
	id, _ := res.LastInsertId()
	if err := auditBy(ctx, tx, u, r, "CASH_SESSION_OPENED", map[string]any{
		"cash_session_id": id, "opening_float": req.OpeningFloat,
	}); err != nil {
		serverError(w, "Error registrando bitácora", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "Error abriendo el turno", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

type movementRequest struct {
	PIN    string  `json:"pin"`
	Kind   string  `json:"kind"`
	Amount float64 `json:"amount"`
	Reason string  `json:"reason"`
}

// POST /api/cash/movements - Entrada o salida de efectivo (gasto, retiro...).
func (h *POSHandler) CreateCashMovement(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req movementRequest
	tx, u, ok := h.beginWithUser(w, r, &req, func() string { return req.PIN })
	if !ok {
		return
	}
	defer tx.Rollback()

	kind, valid := movementKind(req.Kind)
	reason := strings.TrimSpace(req.Reason)
	switch {
	case !valid:
		http.Error(w, "Tipo de movimiento inválido", http.StatusBadRequest)
		return
	case toCents(req.Amount) <= 0:
		http.Error(w, "El monto debe ser mayor a cero", http.StatusBadRequest)
		return
	case reason == "":
		http.Error(w, "Indica el concepto", http.StatusBadRequest)
		return
	}
	sessionID, err := openCashSessionID(ctx, tx)
	if errors.Is(err, errNoCashSession) {
		http.Error(w, "No hay un turno de caja abierto", http.StatusConflict)
		return
	}
	if err != nil {
		serverError(w, "Error consultando la caja", err)
		return
	}

	amount := fromCents(toCents(req.Amount))
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO cash_movements (cash_session_id, kind, amount, reason, user_id) VALUES (?, ?, ?, ?, ?)`,
		sessionID, kind.Key, amount, reason, u.ID); err != nil {
		serverError(w, "Error registrando el movimiento", err)
		return
	}
	if err := auditBy(ctx, tx, u, r, "CASH_MOVEMENT", map[string]any{
		"cash_session_id": sessionID, "kind": kind.Key, "amount": amount, "reason": reason,
	}); err != nil {
		serverError(w, "Error registrando bitácora", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "Error registrando el movimiento", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true})
}

type drawerRequest struct {
	Reason string `json:"reason"`
	Source string `json:"source"` // caja | cobro: qué botón se usó
}

var drawerSources = map[string]string{"caja": "Caja", "cobro": "Cobro"}

const drawerTimeout = 10 * time.Second

// POST /api/cash/drawer - Abre el cajón con el botón (sin PIN, por ahora).
// Cada apertura queda en la bitácora con la terminal y el botón que se usó, y
// aparece en el corte del turno.
func (h *POSHandler) OpenDrawer(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req drawerRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "JSON inválido", http.StatusBadRequest)
			return
		}
	}
	// Si la impresora sigue ocupada con otro trabajo, no se deja al cajero
	// esperando indefinidamente.
	kickCtx, cancel := context.WithTimeout(ctx, drawerTimeout)
	defer cancel()
	out, err := openDrawer(kickCtx, h.DB)
	if errors.Is(err, errNoDrawer) {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if err != nil {
		serverError(w, "Error buscando impresoras", err)
		return
	}
	// Se registra aunque haya fallado: importa saber que se intentó.
	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		serverError(w, "Error registrando bitácora", err)
		return
	}
	defer tx.Rollback()
	if err := audit(ctx, tx, "DRAWER_OPENED", map[string]any{
		"reason": strings.TrimSpace(req.Reason), "source": drawerSources[req.Source],
		"opened": len(out.Printed) > 0, "failed": out.Failed, "terminal": terminalOf(r),
	}); err != nil {
		serverError(w, "Error registrando bitácora", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "Error registrando bitácora", err)
		return
	}
	if len(out.Printed) == 0 {
		msg := strings.Join(out.Failed, "; ")
		if errors.Is(kickCtx.Err(), context.DeadlineExceeded) {
			msg = "la impresora sigue ocupada con otro trabajo; espera a que termine e intenta de nuevo"
		}
		http.Error(w, "No se pudo abrir el cajón: "+msg, http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type closeCashRequest struct {
	PIN              string         `json:"pin"`
	Counts           map[string]int `json:"counts"`
	ReportedCard     float64        `json:"reported_card"`
	ReportedCardTips float64        `json:"reported_card_tips"`
	ReportedTransfer *float64       `json:"reported_transfer"`
	Notes            string         `json:"notes"`
}

// POST /api/cash/close - Cierra el turno con el arqueo ciego. Lo contado se
// guarda tal cual; después del cierre ya no se puede modificar.
func (h *POSHandler) CloseCash(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req closeCashRequest
	tx, u, ok := h.beginWithUser(w, r, &req, func() string { return req.PIN })
	if !ok {
		return
	}
	defer tx.Rollback()

	counts := map[string]int{}
	var countedC int64
	for _, d := range denominations {
		n := req.Counts[d.Key]
		if n < 0 {
			http.Error(w, "Las cantidades no pueden ser negativas", http.StatusBadRequest)
			return
		}
		if n > 0 {
			counts[d.Key] = n
			countedC += d.Cents * int64(n)
		}
	}
	for k := range req.Counts {
		if _, known := counts[k]; !known && req.Counts[k] != 0 {
			http.Error(w, "Denominación desconocida: "+k, http.StatusBadRequest)
			return
		}
	}
	if req.ReportedCard < 0 || req.ReportedCardTips < 0 || (req.ReportedTransfer != nil && *req.ReportedTransfer < 0) {
		http.Error(w, "Los montos reportados no pueden ser negativos", http.StatusBadRequest)
		return
	}

	sessionID, err := openCashSessionID(ctx, tx)
	if errors.Is(err, errNoCashSession) {
		http.Error(w, "No hay un turno de caja abierto", http.StatusConflict)
		return
	}
	if err != nil {
		serverError(w, "Error consultando la caja", err)
		return
	}
	rep, err := loadCashReport(ctx, tx, sessionID)
	if err != nil {
		serverError(w, "Error calculando el corte", err)
		return
	}

	detail, _ := json.Marshal(counts)
	var reportedTransfer any
	if req.ReportedTransfer != nil {
		reportedTransfer = fromCents(toCents(*req.ReportedTransfer))
	}
	counted := fromCents(countedC)
	if _, err := tx.ExecContext(ctx, `
		UPDATE cash_sessions SET
			status = 'CLOSED', closed_at = CURRENT_TIMESTAMP, closed_by = ?,
			counted_cash = ?, count_detail = ?, expected_cash = ?,
			reported_card = ?, reported_card_tips = ?, expected_card = ?,
			reported_transfer = ?, expected_transfer = ?, close_notes = ?
		WHERE id = ? AND status = 'OPEN'`,
		u.ID, counted, string(detail), rep.ExpectedCash,
		fromCents(toCents(req.ReportedCard)), fromCents(toCents(req.ReportedCardTips)), rep.CardExpected,
		reportedTransfer, rep.Transfers, nullIfEmpty(req.Notes), sessionID); err != nil {
		serverError(w, "Error cerrando el turno", err)
		return
	}
	if err := auditBy(ctx, tx, u, r, "CASH_SESSION_CLOSED", map[string]any{
		"cash_session_id": sessionID,
		"expected_cash":   rep.ExpectedCash,
		"counted_cash":    counted,
		"cash_diff":       fromCents(countedC - toCents(rep.ExpectedCash)),
		"expected_card":   rep.CardExpected,
		"reported_card":   fromCents(toCents(req.ReportedCard) + toCents(req.ReportedCardTips)),
		"open_orders":     rep.OpenOrders,
	}); err != nil {
		serverError(w, "Error registrando bitácora", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "Error cerrando el turno", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": sessionID})
}

type noteRequest struct {
	PIN  string `json:"pin"`
	Note string `json:"note"`
}

// POST /api/cash/sessions/{id}/notes - Nota posterior al cierre (aclaraciones
// de faltantes, etc.). El turno en sí no se modifica.
func (h *POSHandler) AddCashNote(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sessionID, ok := pathID(r)
	if !ok {
		http.Error(w, "ID de turno inválido", http.StatusBadRequest)
		return
	}
	var req noteRequest
	tx, u, ok := h.beginWithUser(w, r, &req, func() string { return req.PIN })
	if !ok {
		return
	}
	defer tx.Rollback()
	note := strings.TrimSpace(req.Note)
	if note == "" {
		http.Error(w, "Escribe la nota", http.StatusBadRequest)
		return
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO cash_session_notes (cash_session_id, user_id, note)
		SELECT id, ?, ? FROM cash_sessions WHERE id = ?`, u.ID, note, sessionID)
	if err != nil {
		serverError(w, "Error guardando la nota", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		http.Error(w, "Turno no encontrado", http.StatusNotFound)
		return
	}
	if err := auditBy(ctx, tx, u, r, "CASH_SESSION_NOTE", map[string]any{"cash_session_id": sessionID, "note": note}); err != nil {
		serverError(w, "Error registrando bitácora", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "Error guardando la nota", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true})
}

type CashSessionSummary struct {
	ID           int64      `json:"id"`
	Status       string     `json:"status"`
	OpenedAt     time.Time  `json:"opened_at"`
	ClosedAt     *time.Time `json:"closed_at"`
	OpenedBy     string     `json:"opened_by"`
	ClosedBy     string     `json:"closed_by"`
	Bills        int        `json:"bills"`
	CashDiff     *float64   `json:"cash_diff"`
	CardDiff     *float64   `json:"card_diff"`
	OpeningFloat float64    `json:"opening_float"`
}

// GET /api/cash/sessions - Últimos turnos.
func (h *POSHandler) ListCashSessions(w http.ResponseWriter, r *http.Request) {
	rows, err := h.DB.QueryContext(r.Context(), `
		SELECT cs.id, cs.status, cs.opened_at, cs.closed_at, uo.name, COALESCE(uc.name, ''),
		       (SELECT COUNT(*) FROM bills b WHERE b.cash_session_id = cs.id),
		       cs.counted_cash, cs.expected_cash,
		       cs.reported_card, cs.reported_card_tips, cs.expected_card, cs.opening_float
		FROM cash_sessions cs
		JOIN users uo ON uo.id = cs.opened_by
		LEFT JOIN users uc ON uc.id = cs.closed_by
		ORDER BY cs.id DESC LIMIT 30`)
	if err != nil {
		serverError(w, "Error consultando turnos", err)
		return
	}
	defer rows.Close()
	out := []CashSessionSummary{}
	for rows.Next() {
		var s CashSessionSummary
		var closedAt sql.NullTime
		var counted, expected, repCard, repTips, expCard sql.NullFloat64
		if err := rows.Scan(&s.ID, &s.Status, &s.OpenedAt, &closedAt, &s.OpenedBy, &s.ClosedBy, &s.Bills,
			&counted, &expected, &repCard, &repTips, &expCard, &s.OpeningFloat); err != nil {
			serverError(w, "Error leyendo turnos", err)
			return
		}
		if closedAt.Valid {
			s.ClosedAt = &closedAt.Time
			cash := fromCents(toCents(counted.Float64) - toCents(expected.Float64))
			card := fromCents(toCents(repCard.Float64) + toCents(repTips.Float64) - toCents(expCard.Float64))
			s.CashDiff, s.CardDiff = &cash, &card
		}
		out = append(out, s)
	}
	writeJSON(w, http.StatusOK, out)
}

// POST /api/cash/sessions/{id}/print - Imprime el corte de un turno cerrado.
func (h *POSHandler) PrintCashReport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sessionID, ok := pathID(r)
	if !ok {
		http.Error(w, "ID de turno inválido", http.StatusBadRequest)
		return
	}
	rep, err := loadCashReport(ctx, h.DB, sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "Turno no encontrado", http.StatusNotFound)
		return
	}
	if err != nil {
		serverError(w, "Error calculando el corte", err)
		return
	}
	if !rep.Session.Closed() {
		http.Error(w, "El corte se imprime al cerrar el turno", http.StatusConflict)
		return
	}
	out, err := printCashReport(ctx, h.DB, rep)
	if errors.Is(err, errNoReceiptPrinter) {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if err != nil {
		serverError(w, "Error imprimiendo el corte", err)
		return
	}
	if len(out.Printed) == 0 {
		http.Error(w, "No se pudo imprimir en "+strings.Join(out.Failed, "; "), http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"printer": strings.Join(out.Printed, ", ")})
}

func signedMoney(v float64) string {
	if toCents(v) > 0 {
		return "+" + formatMoney(v)
	}
	return formatMoney(v)
}
