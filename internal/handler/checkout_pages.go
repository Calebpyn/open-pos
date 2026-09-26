package handler

import (
	"context"
	"database/sql"
	"errors"
	"html/template"
	"net/http"
	"strconv"
	"time"
)

var pageFuncs = template.FuncMap{
	"money": formatMoney,
	"localTime": func(t time.Time) string {
		return t.Local().Format("02/01/2006 15:04")
	},
}

// GET /cobrar/{id} - Pantalla de cobro de una orden
func (h *UIHandler) ServeCheckout(w http.ResponseWriter, r *http.Request) {
	orderID, ok := pathID(r)
	if !ok {
		http.Error(w, "ID de orden inválido", http.StatusBadRequest)
		return
	}
	data, err := loadCheckout(r.Context(), h.POS.DB, orderID)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "Orden no encontrada", http.StatusNotFound)
		return
	}
	if err != nil {
		serverError(w, "Error cargando la orden", err)
		return
	}

	renderPage(w, map[string]any{"Data": data, "Methods": paymentMethods}, "checkout.html")
}

// GET /cobrar/mesa/{id} - Redirige al cobro de la orden activa de una mesa
func (h *UIHandler) ServeTableCheckout(w http.ResponseWriter, r *http.Request) {
	tableID, ok := pathID(r)
	if !ok {
		http.Error(w, "ID de mesa inválido", http.StatusBadRequest)
		return
	}
	var orderID int64
	err := h.POS.DB.QueryRowContext(r.Context(), `
		SELECT o.id FROM orders o
		JOIN table_sessions ts ON ts.id = o.session_id
		WHERE ts.table_id = ? AND ts.status = 'OPEN' AND o.status = 'ACTIVE'
		ORDER BY o.id LIMIT 1`, tableID).Scan(&orderID)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "La mesa no tiene una orden activa", http.StatusNotFound)
		return
	}
	if err != nil {
		serverError(w, "Error consultando la mesa", err)
		return
	}

	target := "/cobrar/" + strconv.FormatInt(orderID, 10)
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

type TicketItem struct {
	Name      string
	Quantity  int
	Amount    float64
	Modifiers string
	Notes     string
}

type TicketPayment struct {
	Method    string
	Amount    float64
	Received  float64
	Change    float64
	Reference string
}

type TicketData struct {
	Settings
	BillID         int64
	BillNumber     int
	OrderID        int64
	OrderType      string
	TableName      string
	CustomerName   string
	PaidAt         time.Time
	Items          []TicketItem
	Subtotal       float64
	Discount       float64
	DiscountType   string
	DiscountValue  float64
	DiscountReason string
	Tax            float64
	Total          float64
	Tip            float64
	Grand          float64
	Payments       []TicketPayment
	Change         float64
	AutoPrint      bool
	HeaderLines    []string // dirección, teléfono... (del editor de impresiones)
	PreBill        bool     // pre-cuenta: se lleva a la mesa antes de cobrar
	GuestLabel     string   // cuentas por persona: "Juan" si todo es de él
}

// GET /tickets/{id} - Ticket imprimible de una cuenta pagada (?print=1 imprime al abrir)
func (h *UIHandler) ServeTicket(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	billID, ok := pathID(r)
	if !ok {
		http.Error(w, "ID de cuenta inválido", http.StatusBadRequest)
		return
	}

	t, err := loadTicket(ctx, h.POS.DB, billID)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "Cuenta no encontrada", http.StatusNotFound)
		return
	}
	if err != nil {
		serverError(w, "Error cargando la cuenta", err)
		return
	}
	t.AutoPrint = r.URL.Query().Get("print") == "1"
	if f, err := loadReceiptFormat(ctx, h.POS.DB); err == nil {
		t.TicketFooter, t.HeaderLines = f.FooterLines, nonEmptyLines(f.HeaderLines)
	}

	renderPage(w, t, "ticket.html")
}

// loadTicket reúne todo lo necesario para imprimir una cuenta pagada.
// Regresa sql.ErrNoRows si la cuenta no existe.
func loadTicket(ctx context.Context, q queryer, billID int64) (*TicketData, error) {
	t := &TicketData{BillID: billID}
	// paid_at se lee como NullTime: COALESCE haría perder el tipo DATETIME.
	var paidAt sql.NullTime
	err := q.QueryRowContext(ctx, `
		SELECT b.bill_number, b.order_id, o.order_type, COALESCE(dt.name, ''),
		       COALESCE(c.name, o.customer_name, ''), b.paid_at,
		       b.subtotal, b.discount, COALESCE(b.discount_type, ''), COALESCE(b.discount_value, 0),
		       COALESCE(b.discount_reason, ''), b.tax, b.total, COALESCE(b.tip, 0)
		FROM bills b
		JOIN orders o ON o.id = b.order_id
		LEFT JOIN table_sessions ts ON ts.id = o.session_id
		LEFT JOIN dining_tables dt ON dt.id = ts.table_id
		LEFT JOIN customers c ON c.id = b.customer_id
		WHERE b.id = ?`, billID).
		Scan(&t.BillNumber, &t.OrderID, &t.OrderType, &t.TableName, &t.CustomerName, &paidAt,
			&t.Subtotal, &t.Discount, &t.DiscountType, &t.DiscountValue, &t.DiscountReason,
			&t.Tax, &t.Total, &t.Tip)
	if err != nil {
		return nil, err
	}
	t.PaidAt = paidAt.Time
	t.Grand = fromCents(toCents(t.Total) + toCents(t.Tip))
	t.GuestLabel = billGuestLabel(ctx, q, `oi.bill_id = ?`, billID)

	if t.Settings, err = loadSettings(ctx, q); err != nil {
		return nil, err
	}

	rows, err := q.QueryContext(ctx, `
		SELECT p.name, oi.quantity, oi.unit_price, COALESCE(oi.modifiers_text, ''), COALESCE(oi.notes, '')
		FROM order_items oi JOIN products p ON p.id = oi.product_id
		WHERE oi.bill_id = ? ORDER BY oi.id`, billID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var it TicketItem
		var price float64
		if err := rows.Scan(&it.Name, &it.Quantity, &price, &it.Modifiers, &it.Notes); err != nil {
			return nil, err
		}
		it.Amount = fromCents(toCents(price) * int64(it.Quantity))
		t.Items = append(t.Items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	pRows, err := q.QueryContext(ctx, `
		SELECT payment_method, amount, COALESCE(amount_received, 0), COALESCE(change_given, 0), COALESCE(reference_code, '')
		FROM payments WHERE bill_id = ? ORDER BY id`, billID)
	if err != nil {
		return nil, err
	}
	defer pRows.Close()
	for pRows.Next() {
		var p TicketPayment
		if err := pRows.Scan(&p.Method, &p.Amount, &p.Received, &p.Change, &p.Reference); err != nil {
			return nil, err
		}
		if label, ok := paymentMethods[p.Method]; ok {
			p.Method = label
		}
		t.Change += p.Change
		t.Payments = append(t.Payments, p)
	}
	if err := pRows.Err(); err != nil {
		return nil, err
	}
	return t, nil
}
