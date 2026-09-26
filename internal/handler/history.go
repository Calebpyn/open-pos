package handler

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// --- Historial (admin) ---
//
// Todo sale de lo que el POS ya guarda: órdenes, productos, cuentas, pagos,
// turnos y bitácora. No hay tablas propias.

const historyPageSize = 50

type HistoryOrder struct {
	ID          int64      `json:"id"`
	Type        string     `json:"type"`
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"created_at"`
	ClosedAt    *time.Time `json:"closed_at"`
	Table       string     `json:"table"`
	Customer    string     `json:"customer"`
	Items       int        `json:"items"`         // piezas no anuladas
	Amount      float64    `json:"amount"`        // importe de lo pedido (sin anulados)
	Paid        float64    `json:"paid"`          // cobrado (total + propina de sus cuentas)
	Bills       int        `json:"bills"`         // cuentas cobradas
	Voids       int        `json:"voids"`         // renglones anulados
	CancelNote  string     `json:"cancel_reason"` // motivo si se canceló
	DurationMin *int       `json:"duration_min"`  // de abierta a cerrada
}

// GET /api/admin/history/orders?from&to&type&status&q&page
func (h *POSHandler) AdminHistoryOrders(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rg, err := parseRange(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	qs := r.URL.Query()
	where := []string{"o.created_at >= ?1 AND o.created_at < ?2"}
	args := rg.args()
	switch qs.Get("type") {
	case "DINE_IN", "TAKEAWAY", "FLASH":
		args = append(args, qs.Get("type"))
		where = append(where, "o.order_type = ?"+strconv.Itoa(len(args)))
	}
	switch qs.Get("status") {
	case "ACTIVE", "PAID", "CANCELLED":
		args = append(args, qs.Get("status"))
		where = append(where, "o.status = ?"+strconv.Itoa(len(args)))
	}
	if q := strings.TrimSpace(strings.TrimPrefix(qs.Get("q"), "#")); q != "" {
		if id, err := strconv.ParseInt(q, 10, 64); err == nil {
			args = append(args, id)
			where = append(where, "o.id = ?"+strconv.Itoa(len(args)))
		} else {
			args = append(args, "%"+q+"%")
			n := strconv.Itoa(len(args))
			where = append(where, "(dt.name LIKE ?"+n+" OR o.customer_name LIKE ?"+n+
				" OR EXISTS (SELECT 1 FROM bills b JOIN customers c ON c.id = b.customer_id WHERE b.order_id = o.id AND c.name LIKE ?"+n+"))")
		}
	}
	page, _ := strconv.Atoi(qs.Get("page"))
	if page < 1 {
		page = 1
	}
	from := `FROM orders o
		LEFT JOIN table_sessions ts ON ts.id = o.session_id
		LEFT JOIN dining_tables dt ON dt.id = ts.table_id
		WHERE ` + strings.Join(where, " AND ")

	var total int
	var totalPaid float64
	if err := h.DB.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM((SELECT SUM(b.total + COALESCE(b.tip, 0)) FROM bills b
		                               WHERE b.order_id = o.id AND b.status = 'PAID')), 0) `+from, args...).
		Scan(&total, &totalPaid); err != nil {
		serverError(w, "Error consultando el historial", err)
		return
	}

	out := []HistoryOrder{}
	err = eachRow(ctx, h.DB, `
		SELECT o.id, o.order_type, o.status, o.created_at, o.closed_at,
		       COALESCE(dt.name, ''),
		       COALESCE((SELECT c.name FROM bills b JOIN customers c ON c.id = b.customer_id
		                 WHERE b.order_id = o.id ORDER BY b.id DESC LIMIT 1), o.customer_name, ''),
		       COALESCE((SELECT SUM(quantity) FROM order_items WHERE order_id = o.id AND status != 'VOID'), 0),
		       COALESCE((SELECT SUM(unit_price * quantity) FROM order_items WHERE order_id = o.id AND status != 'VOID'), 0),
		       COALESCE((SELECT SUM(b.total + COALESCE(b.tip, 0)) FROM bills b WHERE b.order_id = o.id AND b.status = 'PAID'), 0),
		       (SELECT COUNT(*) FROM bills b WHERE b.order_id = o.id AND b.status = 'PAID'),
		       (SELECT COUNT(*) FROM order_items WHERE order_id = o.id AND status = 'VOID'),
		       COALESCE(o.cancel_reason, '')
		`+from+`
		ORDER BY o.created_at DESC, o.id DESC
		LIMIT `+strconv.Itoa(historyPageSize)+` OFFSET `+strconv.Itoa((page-1)*historyPageSize),
		args, func(scan func(...any) error) error {
			var o HistoryOrder
			var closed sql.NullTime
			if err := scan(&o.ID, &o.Type, &o.Status, &o.CreatedAt, &closed, &o.Table, &o.Customer,
				&o.Items, &o.Amount, &o.Paid, &o.Bills, &o.Voids, &o.CancelNote); err != nil {
				return err
			}
			if closed.Valid {
				o.ClosedAt = &closed.Time
				m := int(closed.Time.Sub(o.CreatedAt).Minutes())
				o.DurationMin = &m
			}
			o.Amount = fromCents(toCents(o.Amount))
			o.Paid = fromCents(toCents(o.Paid))
			out = append(out, o)
			return nil
		})
	if err != nil {
		serverError(w, "Error consultando el historial", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"range": rg, "orders": out, "total": total, "total_paid": fromCents(toCents(totalPaid)),
		"page": page, "page_size": historyPageSize,
	})
}

type HistoryItem struct {
	ID           int64      `json:"id"`
	Product      string     `json:"product"`
	Area         string     `json:"area"`
	Quantity     int        `json:"quantity"`
	UnitPrice    float64    `json:"unit_price"`
	Modifiers    string     `json:"modifiers"`
	Notes        string     `json:"notes"`
	Status       string     `json:"status"`
	CreatedAt    time.Time  `json:"created_at"`
	DeliveredAt  *time.Time `json:"delivered_at"`
	DeliveredQty int        `json:"delivered_qty"`
	ServeMin     *float64   `json:"serve_min"` // de pedido a entregado
	BillNumber   *int       `json:"bill_number"`
	Guest        string     `json:"guest"`        // cuentas por persona
	AddedBy      string     `json:"added_by"`     // quién lo tomó (terminal de mesero)
	DeliveredBy  string     `json:"delivered_by"` // quién lo marcó entregado
}

type HistoryPayment struct {
	ID           int64   `json:"id"`
	MethodKey    string  `json:"method_key"`
	Collaborator string  `json:"collaborator"` // cargo a nómina
	Method       string  `json:"method"`
	Amount       float64 `json:"amount"`
	Received     float64 `json:"received"`
	Change       float64 `json:"change"`
	Reference    string  `json:"reference"`
}

type HistoryBill struct {
	ID             int64            `json:"id"`
	Number         int              `json:"number"`
	PaidAt         *time.Time       `json:"paid_at"`
	Subtotal       float64          `json:"subtotal"`
	Discount       float64          `json:"discount"`
	DiscountReason string           `json:"discount_reason"`
	Tax            float64          `json:"tax"`
	Total          float64          `json:"total"`
	Tip            float64          `json:"tip"`
	Customer       string           `json:"customer"`
	Shift          *int64           `json:"shift"`   // turno de caja
	Cashier        string           `json:"cashier"` // quien abrió ese turno
	Payments       []HistoryPayment `json:"payments"`
}

type HistoryEvent struct {
	At      time.Time      `json:"at"`
	Action  string         `json:"action"`
	Details map[string]any `json:"details"`
}

// GET /api/admin/history/orders/{id} - Todo sobre una orden.
func (h *POSHandler) AdminHistoryOrder(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := pathID(r)
	if !ok {
		http.Error(w, "ID inválido", http.StatusBadRequest)
		return
	}
	var o HistoryOrder
	var closed, delivered, opened sql.NullTime
	var notes string
	err := h.DB.QueryRowContext(ctx, `
		SELECT o.id, o.order_type, o.status, o.created_at, o.closed_at, o.delivered_at, ts.opened_at,
		       COALESCE(dt.name, ''), COALESCE(o.customer_name, ''), COALESCE(o.cancel_reason, ''), COALESCE(o.notes, '')
		FROM orders o
		LEFT JOIN table_sessions ts ON ts.id = o.session_id
		LEFT JOIN dining_tables dt ON dt.id = ts.table_id
		WHERE o.id = ?`, id).Scan(&o.ID, &o.Type, &o.Status, &o.CreatedAt, &closed, &delivered, &opened,
		&o.Table, &o.Customer, &o.CancelNote, &notes)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "Orden no encontrada", http.StatusNotFound)
		return
	}
	if err != nil {
		serverError(w, "Error consultando la orden", err)
		return
	}
	nt := func(t sql.NullTime) *time.Time {
		if t.Valid {
			return &t.Time
		}
		return nil
	}
	o.ClosedAt = nt(closed)

	items := []HistoryItem{}
	err = eachRow(ctx, h.DB, `
		SELECT oi.id, p.name, COALESCE(pa.name, ''), oi.quantity, oi.unit_price, COALESCE(oi.modifiers_text, ''),
		       COALESCE(oi.notes, ''), oi.status, oi.created_at, oi.delivered_at, oi.delivered_quantity, b.bill_number,
		       COALESCE(g.position, 0), COALESCE(g.name, ''),
		       COALESCE((SELECT name FROM users WHERE id = oi.added_by), ''),
		       COALESCE((SELECT name FROM users WHERE id = oi.delivered_by), '')
		FROM order_items oi
		LEFT JOIN order_guests g ON g.id = oi.guest_id
		JOIN products p ON p.id = oi.product_id
		LEFT JOIN production_areas pa ON pa.id = p.area_id
		LEFT JOIN bills b ON b.id = oi.bill_id
		WHERE oi.order_id = ? ORDER BY oi.id`, []any{id}, func(scan func(...any) error) error {
		var it HistoryItem
		var del sql.NullTime
		var bill sql.NullInt64
		var guestPos int
		var guestName string
		if err := scan(&it.ID, &it.Product, &it.Area, &it.Quantity, &it.UnitPrice, &it.Modifiers, &it.Notes,
			&it.Status, &it.CreatedAt, &del, &it.DeliveredQty, &bill, &guestPos, &guestName, &it.AddedBy, &it.DeliveredBy); err != nil {
			return err
		}
		if guestPos > 0 {
			it.Guest = guestLabel(guestPos, guestName)
		}
		it.DeliveredAt = nt(del)
		if del.Valid {
			m := del.Time.Sub(it.CreatedAt).Minutes()
			it.ServeMin = &m
		}
		if bill.Valid {
			n := int(bill.Int64)
			it.BillNumber = &n
		}
		items = append(items, it)
		return nil
	})
	if err != nil {
		serverError(w, "Error consultando productos", err)
		return
	}

	bills := []HistoryBill{}
	byID := map[int64]int{}
	err = eachRow(ctx, h.DB, `
		SELECT b.id, b.bill_number, b.paid_at, b.subtotal, b.discount, COALESCE(b.discount_reason, ''),
		       b.tax, b.total, COALESCE(b.tip, 0), COALESCE(c.name, ''), b.cash_session_id, COALESCE(u.name, '')
		FROM bills b
		LEFT JOIN customers c ON c.id = b.customer_id
		LEFT JOIN cash_sessions cs ON cs.id = b.cash_session_id
		LEFT JOIN users u ON u.id = cs.opened_by
		WHERE b.order_id = ? AND b.status = 'PAID' ORDER BY b.id`, []any{id}, func(scan func(...any) error) error {
		var b HistoryBill
		var paid sql.NullTime
		var shift sql.NullInt64
		if err := scan(&b.ID, &b.Number, &paid, &b.Subtotal, &b.Discount, &b.DiscountReason, &b.Tax, &b.Total,
			&b.Tip, &b.Customer, &shift, &b.Cashier); err != nil {
			return err
		}
		b.PaidAt = nt(paid)
		if shift.Valid {
			b.Shift = &shift.Int64
		}
		b.Payments = []HistoryPayment{}
		byID[b.ID] = len(bills)
		bills = append(bills, b)
		return nil
	})
	if err != nil {
		serverError(w, "Error consultando cuentas", err)
		return
	}
	err = eachRow(ctx, h.DB, `
		SELECT p.id, p.bill_id, p.payment_method, p.amount, COALESCE(p.amount_received, 0), COALESCE(p.change_given, 0),
		       COALESCE(p.reference_code, ''), COALESCE(co.name, '')
		FROM payments p JOIN bills b ON b.id = p.bill_id
		LEFT JOIN collaborators co ON co.id = p.collaborator_id
		WHERE b.order_id = ? ORDER BY p.id`, []any{id}, func(scan func(...any) error) error {
		var billID int64
		var p HistoryPayment
		if err := scan(&p.ID, &billID, &p.Method, &p.Amount, &p.Received, &p.Change, &p.Reference, &p.Collaborator); err != nil {
			return err
		}
		p.MethodKey = p.Method
		if label, ok := paymentMethods[p.Method]; ok {
			p.Method = label
		}
		if i, ok := byID[billID]; ok {
			bills[i].Payments = append(bills[i].Payments, p)
		}
		return nil
	})
	if err != nil {
		serverError(w, "Error consultando pagos", err)
		return
	}

	// Bitácora de la orden: cancelaciones, anulaciones, cobros, cierres.
	events := []HistoryEvent{}
	err = eachRow(ctx, h.DB, `
		SELECT created_at, action, details FROM audit_logs
		WHERE created_at >= datetime(?, '-1 minute')
		  AND json_valid(details) AND json_extract(details, '$.order_id') = ?
		ORDER BY id`, []any{o.CreatedAt.UTC().Format("2006-01-02 15:04:05"), id}, func(scan func(...any) error) error {
		var e HistoryEvent
		var raw string
		if err := scan(&e.At, &e.Action, &raw); err != nil {
			return err
		}
		json.Unmarshal([]byte(raw), &e.Details)
		events = append(events, e)
		return nil
	})
	if err != nil {
		serverError(w, "Error consultando la bitácora", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"order": o, "notes": notes, "opened_at": nt(opened), "delivered_at": nt(delivered),
		"items": items, "bills": bills, "events": events,
	})
}

type PaymentRow struct {
	PaidAt    time.Time `json:"paid_at"`
	OrderID   int64     `json:"order_id"`
	BillID    int64     `json:"bill_id"`
	Bill      int       `json:"bill_number"`
	Where     string    `json:"where"`
	Method    string    `json:"method"`
	MethodKey string    `json:"method_key"`
	Amount    float64   `json:"amount"`
	Tip       float64   `json:"tip"`
	Reference string    `json:"reference"`
	Shift     *int64    `json:"shift"`
	// Cargo a nómina: a quién.
	Collaborator string `json:"collaborator"`
}

// GET /api/admin/history/payments?from&to&method - Cada pago recibido, con
// totales por método.
func (h *POSHandler) AdminHistoryPayments(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rg, err := parseRange(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	args := rg.args()
	filter := ""
	if m := r.URL.Query().Get("method"); m != "" {
		if _, ok := paymentMethods[m]; ok {
			args = append(args, m)
			filter = " AND p.payment_method = ?3"
		}
	}
	rows := []PaymentRow{}
	byMethod := map[string]float64{}
	var total float64
	err = eachRow(ctx, h.DB, `
		SELECT b.paid_at, b.order_id, b.id, b.bill_number,
		       COALESCE(dt.name, NULLIF(o.customer_name, ''), CASE o.order_type WHEN 'FLASH' THEN 'Mostrador' ELSE 'Para llevar' END),
		       p.payment_method, p.amount, COALESCE(p.tip_amount, 0), COALESCE(p.reference_code, ''), b.cash_session_id,
		       COALESCE((SELECT name FROM collaborators WHERE id = p.collaborator_id), '')
		FROM payments p
		JOIN bills b ON b.id = p.bill_id
		JOIN orders o ON o.id = b.order_id
		LEFT JOIN table_sessions ts ON ts.id = o.session_id
		LEFT JOIN dining_tables dt ON dt.id = ts.table_id
		WHERE `+billsInRange+filter+`
		ORDER BY b.paid_at DESC, p.id DESC
		LIMIT 1000`, args, func(scan func(...any) error) error {
		var p PaymentRow
		var shift sql.NullInt64
		if err := scan(&p.PaidAt, &p.OrderID, &p.BillID, &p.Bill, &p.Where, &p.MethodKey, &p.Amount, &p.Tip,
			&p.Reference, &shift, &p.Collaborator); err != nil {
			return err
		}
		p.Method = paymentMethods[p.MethodKey]
		if shift.Valid {
			p.Shift = &shift.Int64
		}
		byMethod[p.Method] += p.Amount
		total += p.Amount
		rows = append(rows, p)
		return nil
	})
	if err != nil {
		serverError(w, "Error consultando pagos", err)
		return
	}
	type methodTotal struct {
		Method string  `json:"method"`
		Amount float64 `json:"amount"`
	}
	sums := []methodTotal{}
	for m, a := range byMethod {
		sums = append(sums, methodTotal{m, fromCents(toCents(a))})
	}
	sort.Slice(sums, func(i, j int) bool { return sums[i].Amount > sums[j].Amount })
	writeJSON(w, http.StatusOK, map[string]any{
		"range": rg, "payments": rows, "by_method": sums, "total": fromCents(toCents(total)),
	})
}
