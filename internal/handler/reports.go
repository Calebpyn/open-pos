package handler

import (
	"context"
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Los reportes agrupan por día y hora LOCALES del servidor (la Mac del
// negocio): SQLite convierte con el modificador 'localtime'. Una cuenta
// cuenta en el día en que se cobró.

const dateLayout = "2006-01-02"

type ReportRange struct {
	From string `json:"from"`
	To   string `json:"to"`
	Days int    `json:"days"`

	// Límites en UTC, como se guardan las fechas (CURRENT_TIMESTAMP): el
	// filtro usa índices y no convierte cada renglón a hora local.
	startUTC, endUTC string
}

const sqliteTime = "2006-01-02 15:04:05"

// withBounds calcula [inicio del día From, inicio del día siguiente a To) en
// hora local del servidor, expresado en UTC.
func (r ReportRange) withBounds() ReportRange {
	f, _ := time.ParseInLocation(dateLayout, r.From, time.Local)
	t, _ := time.ParseInLocation(dateLayout, r.To, time.Local)
	r.startUTC = f.UTC().Format(sqliteTime)
	r.endUTC = t.AddDate(0, 0, 1).UTC().Format(sqliteTime)
	return r
}

func (r ReportRange) args() []any { return []any{r.startUTC, r.endUTC} }

// parseRange lee ?from=&to= (YYYY-MM-DD). Por omisión, hoy.
func parseRange(r *http.Request) (ReportRange, error) {
	today := time.Now().Format(dateLayout)
	from, to := r.URL.Query().Get("from"), r.URL.Query().Get("to")
	if from == "" {
		from = today
	}
	if to == "" {
		to = from
	}
	f, err1 := time.Parse(dateLayout, from)
	t, err2 := time.Parse(dateLayout, to)
	if err1 != nil || err2 != nil {
		return ReportRange{}, fmt.Errorf("Fechas inválidas")
	}
	if t.Before(f) {
		return ReportRange{}, fmt.Errorf("La fecha final es anterior a la inicial")
	}
	days := int(t.Sub(f).Hours()/24) + 1
	if days > 400 {
		return ReportRange{}, fmt.Errorf("El rango máximo es de 400 días")
	}
	return ReportRange{From: from, To: to, Days: days}.withBounds(), nil
}

// previous es el periodo de la misma duración justo antes de r.
func (r ReportRange) previous() ReportRange {
	f, _ := time.Parse(dateLayout, r.From)
	return ReportRange{
		From: f.AddDate(0, 0, -r.Days).Format(dateLayout),
		To:   f.AddDate(0, 0, -1).Format(dateLayout),
		Days: r.Days,
	}.withBounds()
}

type SalesKPIs struct {
	Sales     float64 `json:"sales"` // total de cuentas, sin propina
	Subtotal  float64 `json:"subtotal"`
	Discounts float64 `json:"discounts"`
	Tax       float64 `json:"tax"`
	Tips      float64 `json:"tips"`
	Bills     int     `json:"bills"`
	Orders    int     `json:"orders"`
	Items     int     `json:"items"`
	AvgTicket float64 `json:"avg_ticket"`
}

type DayPoint struct {
	Date  string  `json:"date"`
	Sales float64 `json:"sales"`
	Bills int     `json:"bills"`
}

type HourPoint struct {
	Hour  int     `json:"hour"`
	Sales float64 `json:"sales"`
	Bills int     `json:"bills"`
}

type LabeledAmount struct {
	Key      string  `json:"key"`
	Label    string  `json:"label"`
	Color    string  `json:"color,omitempty"`
	Count    int     `json:"count"`
	Amount   float64 `json:"amount"`
	Tips     float64 `json:"tips,omitempty"`
	Quantity int     `json:"quantity,omitempty"`
	Category string  `json:"category,omitempty"`
}

type TipWeek struct {
	WeekStart string  `json:"week_start"`
	Cash      float64 `json:"cash"`
	Other     float64 `json:"other"` // tarjeta, transferencia, otro
}

type ControlSummary struct {
	Cancellations      int     `json:"cancellations"`
	CancelledAmount    float64 `json:"cancelled_amount"`
	Voids              int     `json:"voids"`
	VoidedAmount       float64 `json:"voided_amount"`
	DiscountedBills    int     `json:"discounted_bills"`
	Courtesies         int     `json:"courtesies"`
	DrawerOpens        int     `json:"drawer_opens"`
	ClosedShifts       int     `json:"closed_shifts"`
	CashDiffTotal      float64 `json:"cash_diff_total"`
	ShiftsWithCashDiff int     `json:"shifts_with_cash_diff"`
}

type SalesReport struct {
	Range      ReportRange          `json:"range"`
	Previous   ReportRange          `json:"previous"`
	KPIs       SalesKPIs            `json:"kpis"`
	PrevKPIs   SalesKPIs            `json:"prev_kpis"`
	Daily      []DayPoint           `json:"daily"`
	Hourly     []HourPoint          `json:"hourly"`
	Methods    []LabeledAmount      `json:"methods"`
	Categories []LabeledAmount      `json:"categories"`
	Products   []LabeledAmount      `json:"products"`
	OrderTypes []LabeledAmount      `json:"order_types"`
	TipWeeks   []TipWeek            `json:"tip_weeks"`
	Control    ControlSummary       `json:"control"`
	Shifts     []CashSessionSummary `json:"shifts"`
}

// El filtro de cuentas cobradas en el rango; ?1 = inicio, ?2 = fin (UTC,
// fin exclusivo). Usa el índice idx_bills_paid_at.
const billsInRange = `b.status = 'PAID' AND b.paid_at >= ?1 AND b.paid_at < ?2`

func loadKPIs(ctx context.Context, q queryer, rg ReportRange) (SalesKPIs, error) {
	var k SalesKPIs
	err := q.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(b.total), 0), COALESCE(SUM(b.subtotal), 0), COALESCE(SUM(b.discount), 0),
		       COALESCE(SUM(b.tax), 0), COALESCE(SUM(b.tip), 0), COUNT(*), COUNT(DISTINCT b.order_id),
		       COALESCE((SELECT SUM(oi.quantity) FROM order_items oi JOIN bills b ON b.id = oi.bill_id
		                 WHERE oi.status != 'VOID' AND `+billsInRange+`), 0)
		FROM bills b WHERE `+billsInRange, rg.args()...).
		Scan(&k.Sales, &k.Subtotal, &k.Discounts, &k.Tax, &k.Tips, &k.Bills, &k.Orders, &k.Items)
	if err != nil {
		return k, err
	}
	for _, v := range []*float64{&k.Sales, &k.Subtotal, &k.Discounts, &k.Tax, &k.Tips} {
		*v = fromCents(toCents(*v))
	}
	if k.Bills > 0 {
		k.AvgTicket = fromCents(toCents(k.Sales) / int64(k.Bills))
	}
	return k, nil
}

func loadSalesReport(ctx context.Context, q queryer, rg ReportRange) (*SalesReport, error) {
	rep := &SalesReport{Range: rg, Previous: rg.previous()}
	var err error
	if rep.KPIs, err = loadKPIs(ctx, q, rg); err != nil {
		return nil, err
	}
	if rep.PrevKPIs, err = loadKPIs(ctx, q, rep.Previous); err != nil {
		return nil, err
	}

	// Por día, con los días sin ventas en cero para que la gráfica no mienta.
	byDay := map[string]DayPoint{}
	if err := eachRow(ctx, q, `
		SELECT date(b.paid_at, 'localtime') AS d, SUM(b.total), COUNT(*)
		FROM bills b WHERE `+billsInRange+` GROUP BY d`, rg.args(), func(scan func(...any) error) error {
		var p DayPoint
		if err := scan(&p.Date, &p.Sales, &p.Bills); err != nil {
			return err
		}
		byDay[p.Date] = p
		return nil
	}); err != nil {
		return nil, err
	}
	start, _ := time.Parse(dateLayout, rg.From)
	for i := 0; i < rg.Days; i++ {
		d := start.AddDate(0, 0, i).Format(dateLayout)
		p, ok := byDay[d]
		if !ok {
			p = DayPoint{Date: d}
		}
		p.Sales = fromCents(toCents(p.Sales))
		rep.Daily = append(rep.Daily, p)
	}

	// Por hora del día (suma de todo el rango).
	rep.Hourly = make([]HourPoint, 24)
	for h := range rep.Hourly {
		rep.Hourly[h].Hour = h
	}
	if err := eachRow(ctx, q, `
		SELECT CAST(strftime('%H', b.paid_at, 'localtime') AS INTEGER) AS h, SUM(b.total), COUNT(*)
		FROM bills b WHERE `+billsInRange+` GROUP BY h`, rg.args(), func(scan func(...any) error) error {
		var h, n int
		var s float64
		if err := scan(&h, &s, &n); err != nil {
			return err
		}
		if h >= 0 && h < 24 {
			rep.Hourly[h].Sales, rep.Hourly[h].Bills = fromCents(toCents(s)), n
		}
		return nil
	}); err != nil {
		return nil, err
	}

	// Métodos de pago; la propina de cada cuenta se reparte entre sus pagos
	// en proporción al monto (igual que en el corte de caja).
	byMethod := map[string]*LabeledAmount{}
	if err := eachRow(ctx, q, `
		SELECT p.payment_method, COUNT(*), SUM(p.amount),
		       SUM(CASE WHEN b.total + COALESCE(b.tip, 0) > 0
		                THEN COALESCE(b.tip, 0) * p.amount / (b.total + COALESCE(b.tip, 0)) ELSE 0 END)
		FROM payments p JOIN bills b ON b.id = p.bill_id
		WHERE `+billsInRange+` GROUP BY p.payment_method`, rg.args(), func(scan func(...any) error) error {
		var m LabeledAmount
		if err := scan(&m.Key, &m.Count, &m.Amount, &m.Tips); err != nil {
			return err
		}
		m.Label = paymentMethods[m.Key]
		m.Amount, m.Tips = fromCents(toCents(m.Amount)), fromCents(toCents(m.Tips))
		byMethod[m.Key] = &m
		return nil
	}); err != nil {
		return nil, err
	}
	for _, k := range methodOrder {
		if m := byMethod[k]; m != nil {
			rep.Methods = append(rep.Methods, *m)
		}
	}

	// Categorías y productos: importe bruto (antes de descuentos de la cuenta).
	if err := eachRow(ctx, q, `
		SELECT c.id, c.name, COALESCE(c.color_hex, ''), SUM(oi.quantity), SUM(oi.quantity * oi.unit_price)
		FROM order_items oi
		JOIN bills b ON b.id = oi.bill_id
		JOIN products p ON p.id = oi.product_id
		JOIN categories c ON c.id = p.category_id
		WHERE oi.status != 'VOID' AND `+billsInRange+`
		GROUP BY c.id ORDER BY 5 DESC`, rg.args(), func(scan func(...any) error) error {
		var c LabeledAmount
		if err := scan(&c.Key, &c.Label, &c.Color, &c.Quantity, &c.Amount); err != nil {
			return err
		}
		c.Amount = fromCents(toCents(c.Amount))
		rep.Categories = append(rep.Categories, c)
		return nil
	}); err != nil {
		return nil, err
	}
	if err := eachRow(ctx, q, `
		SELECT p.id, p.name, c.name, SUM(oi.quantity), SUM(oi.quantity * oi.unit_price)
		FROM order_items oi
		JOIN bills b ON b.id = oi.bill_id
		JOIN products p ON p.id = oi.product_id
		JOIN categories c ON c.id = p.category_id
		WHERE oi.status != 'VOID' AND `+billsInRange+`
		GROUP BY p.id ORDER BY 5 DESC LIMIT 20`, rg.args(), func(scan func(...any) error) error {
		var p LabeledAmount
		if err := scan(&p.Key, &p.Label, &p.Category, &p.Quantity, &p.Amount); err != nil {
			return err
		}
		p.Amount = fromCents(toCents(p.Amount))
		rep.Products = append(rep.Products, p)
		return nil
	}); err != nil {
		return nil, err
	}

	// Tipo de venta.
	typeLabels := map[string]string{"DINE_IN": "Mesa", "TAKEAWAY": "Para llevar", "FLASH": "Mostrador"}
	if err := eachRow(ctx, q, `
		SELECT o.order_type, COUNT(*), SUM(b.total)
		FROM bills b JOIN orders o ON o.id = b.order_id
		WHERE `+billsInRange+` GROUP BY o.order_type ORDER BY 3 DESC`, rg.args(), func(scan func(...any) error) error {
		var t LabeledAmount
		if err := scan(&t.Key, &t.Count, &t.Amount); err != nil {
			return err
		}
		t.Label = typeLabels[t.Key]
		t.Amount = fromCents(toCents(t.Amount))
		rep.OrderTypes = append(rep.OrderTypes, t)
		return nil
	}); err != nil {
		return nil, err
	}

	// Propinas por semana (lunes a domingo), separando efectivo del resto:
	// son las que se reparten con la nómina.
	byWeek := map[string]*TipWeek{}
	var weeks []string
	if err := eachRow(ctx, q, `
		SELECT date(b.paid_at, 'localtime', 'weekday 0', '-6 days') AS w, p.payment_method = 'CASH',
		       SUM(COALESCE(b.tip, 0) * p.amount / (b.total + COALESCE(b.tip, 0)))
		FROM payments p JOIN bills b ON b.id = p.bill_id
		WHERE `+billsInRange+` AND COALESCE(b.tip, 0) > 0
		GROUP BY w, 2 ORDER BY w`, rg.args(), func(scan func(...any) error) error {
		var w string
		var cash bool
		var amount float64
		if err := scan(&w, &cash, &amount); err != nil {
			return err
		}
		tw := byWeek[w]
		if tw == nil {
			tw = &TipWeek{WeekStart: w}
			byWeek[w] = tw
			weeks = append(weeks, w)
		}
		if cash {
			tw.Cash = fromCents(toCents(amount))
		} else {
			tw.Other = fromCents(toCents(tw.Other) + toCents(amount))
		}
		return nil
	}); err != nil {
		return nil, err
	}
	for _, w := range weeks {
		rep.TipWeeks = append(rep.TipWeeks, *byWeek[w])
	}

	if err := loadControlSummary(ctx, q, rg, &rep.Control); err != nil {
		return nil, err
	}
	if rep.Shifts, err = loadShiftsInRange(ctx, q, rg); err != nil {
		return nil, err
	}
	rep.Control.ClosedShifts = len(rep.Shifts)
	var diffC int64
	for _, s := range rep.Shifts {
		if d := toCents(*s.CashDiff); d != 0 {
			rep.Control.ShiftsWithCashDiff++
			diffC += d
		}
	}
	rep.Control.CashDiffTotal = fromCents(diffC)
	return rep, nil
}

func loadControlSummary(ctx context.Context, q queryer, rg ReportRange, c *ControlSummary) error {
	const inRange = `created_at >= ?1 AND created_at < ?2`
	if err := q.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(action IN ('ORDER_CANCELLED', 'ORDER_CLOSED_WITH_BALANCE')), 0),
			COALESCE(SUM(CASE WHEN action IN ('ORDER_CANCELLED', 'ORDER_CLOSED_WITH_BALANCE')
			                  THEN json_extract(details, '$.unpaid_amount') END), 0),
			COALESCE(SUM(action = 'ITEM_VOID'), 0),
			COALESCE(SUM(CASE WHEN action = 'ITEM_VOID'
			                  THEN json_extract(details, '$.quantity') * json_extract(details, '$.unit_price') END), 0),
			COALESCE(SUM(action = 'DRAWER_OPENED'), 0)
		FROM audit_logs WHERE `+inRange, rg.args()...).
		Scan(&c.Cancellations, &c.CancelledAmount, &c.Voids, &c.VoidedAmount, &c.DrawerOpens); err != nil {
		return err
	}
	c.CancelledAmount, c.VoidedAmount = fromCents(toCents(c.CancelledAmount)), fromCents(toCents(c.VoidedAmount))
	return q.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(b.discount > 0), 0), COALESCE(SUM(b.discount > 0 AND b.total = 0), 0)
		FROM bills b WHERE `+billsInRange, rg.args()...).Scan(&c.DiscountedBills, &c.Courtesies)
}

func loadShiftsInRange(ctx context.Context, q queryer, rg ReportRange) ([]CashSessionSummary, error) {
	out := []CashSessionSummary{}
	err := eachRow(ctx, q, `
		SELECT cs.id, cs.closed_at, uc.name,
		       (SELECT COUNT(*) FROM bills b WHERE b.cash_session_id = cs.id),
		       COALESCE(cs.counted_cash, 0) - COALESCE(cs.expected_cash, 0),
		       COALESCE(cs.reported_card, 0) + COALESCE(cs.reported_card_tips, 0) - COALESCE(cs.expected_card, 0)
		FROM cash_sessions cs LEFT JOIN users uc ON uc.id = cs.closed_by
		WHERE cs.status = 'CLOSED' AND cs.closed_at >= ?1 AND cs.closed_at < ?2
		ORDER BY cs.id`, rg.args(), func(scan func(...any) error) error {
		var s CashSessionSummary
		var closedAt time.Time
		var cash, card float64
		if err := scan(&s.ID, &closedAt, &s.ClosedBy, &s.Bills, &cash, &card); err != nil {
			return err
		}
		s.Status, s.ClosedAt = "CLOSED", &closedAt
		cash, card = fromCents(toCents(cash)), fromCents(toCents(card))
		s.CashDiff, s.CardDiff = &cash, &card
		out = append(out, s)
		return nil
	})
	return out, err
}

// eachRow corre una consulta y llama fn por renglón.
func eachRow(ctx context.Context, q queryer, query string, args []any, fn func(scan func(...any) error) error) error {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows.Scan); err != nil {
			return err
		}
	}
	return rows.Err()
}

// GET /api/admin/reports?from=&to=
func (h *POSHandler) AdminSalesReport(w http.ResponseWriter, r *http.Request) {
	rg, err := parseRange(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rep, err := loadSalesReport(r.Context(), h.DB, rg)
	if err != nil {
		serverError(w, "Error generando el reporte", err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// GET /api/admin/reports/cuentas.csv?from=&to= - Detalle de cuentas cobradas
// para el contador. Con BOM para que Excel respete los acentos.
func (h *POSHandler) AdminBillsCSV(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rg, err := parseRange(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rows, err := h.DB.QueryContext(ctx, `
		SELECT datetime(b.paid_at, 'localtime'), b.order_id, b.bill_number, o.order_type,
		       COALESCE(dt.name, o.customer_name, ''),
		       b.subtotal, b.discount, COALESCE(b.discount_reason, ''), b.tax, b.total, COALESCE(b.tip, 0),
		       COALESCE(b.cash_session_id, 0),
		       COALESCE((SELECT SUM(amount) FROM payments WHERE bill_id = b.id AND payment_method = 'CASH'), 0),
		       COALESCE((SELECT SUM(amount) FROM payments WHERE bill_id = b.id AND payment_method = 'CARD_DEBIT'), 0),
		       COALESCE((SELECT SUM(amount) FROM payments WHERE bill_id = b.id AND payment_method = 'CARD_CREDIT'), 0),
		       COALESCE((SELECT SUM(amount) FROM payments WHERE bill_id = b.id AND payment_method = 'TRANSFER'), 0),
		       COALESCE((SELECT SUM(amount) FROM payments WHERE bill_id = b.id AND payment_method = 'OTHER'), 0),
		       COALESCE((SELECT SUM(amount) FROM payments WHERE bill_id = b.id AND payment_method = 'PAYROLL'), 0),
		       COALESCE((SELECT GROUP_CONCAT(co.name, ', ') FROM payments p JOIN collaborators co ON co.id = p.collaborator_id
		                 WHERE p.bill_id = b.id AND p.payment_method = 'PAYROLL'), '')
		FROM bills b
		JOIN orders o ON o.id = b.order_id
		LEFT JOIN table_sessions ts ON ts.id = o.session_id
		LEFT JOIN dining_tables dt ON dt.id = ts.table_id
		WHERE `+billsInRange+` ORDER BY b.paid_at, b.id`, rg.args()...)
	if err != nil {
		serverError(w, "Error generando el CSV", err)
		return
	}
	defer rows.Close()

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="cuentas_%s_a_%s.csv"`, rg.From, rg.To))
	w.Write([]byte{0xEF, 0xBB, 0xBF})
	cw := csv.NewWriter(w)
	cw.Write([]string{"Fecha", "Orden", "Cuenta", "Tipo", "Mesa / Cliente", "Subtotal", "Descuento", "Motivo descuento",
		"IVA", "Total", "Propina", "Total cobrado", "Efectivo", "Tarjeta débito", "Tarjeta crédito", "Transferencia", "Otro", "Cargo a nómina", "Colaborador", "Turno"})
	typeLabels := map[string]string{"DINE_IN": "Mesa", "TAKEAWAY": "Para llevar", "FLASH": "Mostrador"}
	money := func(v float64) string { return strconv.FormatFloat(fromCents(toCents(v)), 'f', 2, 64) }
	for rows.Next() {
		var paidAt, orderType, title, reason string
		var orderID, session int64
		var number int
		var sub, disc, tax, total, tip, cash, debit, credit, transfer, other, payroll float64
		var collaborator string
		if err := rows.Scan(&paidAt, &orderID, &number, &orderType, &title, &sub, &disc, &reason, &tax, &total, &tip,
			&session, &cash, &debit, &credit, &transfer, &other, &payroll, &collaborator); err != nil {
			return // la respuesta ya empezó; se corta el archivo
		}
		turno := ""
		if session > 0 {
			turno = strconv.FormatInt(session, 10)
		}
		cw.Write([]string{paidAt, strconv.FormatInt(orderID, 10), strconv.Itoa(number), typeLabels[orderType], csvText(title),
			money(sub), money(disc), csvText(reason), money(tax), money(total), money(tip), money(total + tip),
			money(cash), money(debit), money(credit), money(transfer), money(other), money(payroll), csvText(collaborator), turno})
	}
	cw.Flush()
}

// csvText neutraliza un texto capturado por el personal (nombre de cliente,
// motivo de descuento) que empiece como fórmula: Excel ejecutaría
// "=HYPERLINK(...)" al abrir el archivo. El apóstrofo lo deja como texto.
func csvText(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}
