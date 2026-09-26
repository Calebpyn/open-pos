package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// queryer lo cumplen *sql.DB y *sql.Tx.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

var paymentMethods = map[string]string{
	"CASH":        "Efectivo",
	"CARD_DEBIT":  "Tarjeta de débito",
	"CARD_CREDIT": "Tarjeta de crédito",
	"TRANSFER":    "Transferencia",
	"PAYROLL":     "Cargo a nómina",
	"OTHER":       "Otro",
}

type Settings struct {
	BusinessName     string  `json:"business_name"`
	TaxRate          float64 `json:"tax_rate"`
	PricesIncludeTax bool    `json:"prices_include_tax"`
	TicketFooter     string  `json:"ticket_footer"`
	StaffDiscountPct float64 `json:"staff_discount_pct"` // descuento de colaborador (0 = sin botón)
}

func loadSettings(ctx context.Context, q queryer) (Settings, error) {
	s := Settings{BusinessName: "Open POS", TaxRate: 0.16, PricesIncludeTax: true}
	rows, err := q.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return s, err
	}
	defer rows.Close()

	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return s, err
		}
		switch k {
		case "business_name":
			s.BusinessName = v
		case "tax_rate":
			if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
				s.TaxRate = f
			}
		case "prices_include_tax":
			s.PricesIncludeTax = v == "1" || v == "true"
		case "ticket_footer":
			s.TicketFooter = v
		case "staff_discount_pct":
			if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 && f <= 100 {
				s.StaffDiscountPct = f
			}
		}
	}
	return s, rows.Err()
}

type CheckoutItem struct {
	ID          int64   `json:"id"`
	ProductID   int64   `json:"product_id"`
	ModifierIDs []int64 `json:"modifier_ids"`   // para editar el producto
	GuestPos    int     `json:"guest_position"` // 0 = mesa
	Guest       string  `json:"guest"`
	ProductName string  `json:"product_name"`
	UnitPrice   float64 `json:"unit_price"`
	Quantity    int     `json:"quantity"`
	Modifiers   string  `json:"modifiers"`
	Notes       string  `json:"notes"`
}

type PaidBill struct {
	ID         int64   `json:"id"`
	BillNumber int     `json:"bill_number"`
	Total      float64 `json:"total"`
	Tip        float64 `json:"tip"`
}

type CheckoutData struct {
	OrderID      int64          `json:"order_id"`
	OrderType    string         `json:"order_type"`
	Status       string         `json:"status"`
	TableName    string         `json:"table_name"`
	CustomerName string         `json:"customer_name"`
	Notes        string         `json:"notes"`
	CreatedAt    time.Time      `json:"created_at"`
	Items        []CheckoutItem `json:"items"` // pendientes de cobro
	Bills        []PaidBill     `json:"bills"` // cuentas ya pagadas
	Settings     Settings       `json:"settings"`
	CashOpen     bool           `json:"cash_open"` // hay turno de caja para cobrar
	Guests       []Guest        `json:"guests"`    // cuentas por persona de la orden
}

// loadCheckout regresa sql.ErrNoRows si la orden no existe.
func loadCheckout(ctx context.Context, q queryer, orderID int64) (*CheckoutData, error) {
	d := &CheckoutData{Items: []CheckoutItem{}, Bills: []PaidBill{}, Guests: []Guest{}}
	err := q.QueryRowContext(ctx, `
		SELECT o.id, o.order_type, o.status, COALESCE(t.name, ''),
		       COALESCE(o.customer_name, ''), COALESCE(o.notes, ''), o.created_at
		FROM orders o
		LEFT JOIN table_sessions ts ON ts.id = o.session_id
		LEFT JOIN dining_tables t ON t.id = ts.table_id
		WHERE o.id = ?`, orderID).
		Scan(&d.OrderID, &d.OrderType, &d.Status, &d.TableName, &d.CustomerName, &d.Notes, &d.CreatedAt)
	if err != nil {
		return nil, err
	}

	rows, err := q.QueryContext(ctx, `
		SELECT oi.id, oi.product_id, p.name, oi.unit_price, oi.quantity, COALESCE(oi.modifiers_text, ''), COALESCE(oi.notes, ''),
		       COALESCE((SELECT GROUP_CONCAT(modifier_option_id) FROM order_item_modifiers WHERE order_item_id = oi.id), ''),
		       COALESCE(g.position, 0), COALESCE(g.name, '')
		FROM order_items oi
		JOIN products p ON p.id = oi.product_id
		LEFT JOIN order_guests g ON g.id = oi.guest_id
		WHERE oi.order_id = ? AND oi.bill_id IS NULL AND oi.status != 'VOID'
		ORDER BY CASE WHEN g.position IS NULL THEN 1 ELSE 0 END, g.position, oi.id`, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var it CheckoutItem
		var modIDs, guestName string
		if err := rows.Scan(&it.ID, &it.ProductID, &it.ProductName, &it.UnitPrice, &it.Quantity, &it.Modifiers, &it.Notes, &modIDs,
			&it.GuestPos, &guestName); err != nil {
			return nil, err
		}
		it.Guest = guestLabel(it.GuestPos, guestName)
		it.ModifierIDs = []int64{}
		for _, s := range strings.Split(modIDs, ",") {
			if id, err := strconv.ParseInt(s, 10, 64); err == nil {
				it.ModifierIDs = append(it.ModifierIDs, id)
			}
		}
		d.Items = append(d.Items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if d.Guests, err = loadGuests(ctx, q, orderID); err != nil {
		return nil, err
	}

	bRows, err := q.QueryContext(ctx, `
		SELECT id, bill_number, total, COALESCE(tip, 0)
		FROM bills WHERE order_id = ? AND status = 'PAID'
		ORDER BY bill_number`, orderID)
	if err != nil {
		return nil, err
	}
	defer bRows.Close()
	for bRows.Next() {
		var b PaidBill
		if err := bRows.Scan(&b.ID, &b.BillNumber, &b.Total, &b.Tip); err != nil {
			return nil, err
		}
		d.Bills = append(d.Bills, b)
	}
	if err := bRows.Err(); err != nil {
		return nil, err
	}

	if _, err := openCashSessionID(ctx, q); err == nil {
		d.CashOpen = true
	} else if !errors.Is(err, errNoCashSession) {
		return nil, err
	}

	d.Settings, err = loadSettings(ctx, q)
	return d, err
}

// GET /api/orders/{id}/checkout
func (h *POSHandler) GetCheckout(w http.ResponseWriter, r *http.Request) {
	orderID, ok := pathID(r)
	if !ok {
		http.Error(w, "ID de orden inválido", http.StatusBadRequest)
		return
	}
	data, err := loadCheckout(r.Context(), h.DB, orderID)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "Orden no encontrada", http.StatusNotFound)
		return
	}
	if err != nil {
		serverError(w, "Error cargando la orden", err)
		return
	}
	writeJSON(w, http.StatusOK, data)
}

type PayRequest struct {
	ItemIDs        []int64          `json:"item_ids"`
	Quantities     map[int64]int    `json:"quantities"`    // piezas a cobrar de un renglón (si no son todas)
	DiscountType   string           `json:"discount_type"` // "", PERCENT, AMOUNT
	DiscountValue  float64          `json:"discount_value"`
	DiscountReason string           `json:"discount_reason"`
	Tip            float64          `json:"tip"`
	CustomerID     *int64           `json:"customer_id"`
	CustomerName   string           `json:"customer_name"`
	NewCustomer    *NewCustomerData `json:"new_customer"`
	Payments       []PaymentRequest `json:"payments"`
	PayrollPIN     string           `json:"payroll_pin"` // PIN de caja que autoriza un cargo a nómina
}

type NewCustomerData struct {
	Name  string `json:"name"`
	Phone string `json:"phone"`
	Email string `json:"email"`
	RFC   string `json:"rfc"`
}

type PaymentRequest struct {
	Method    string  `json:"method"`
	Amount    float64 `json:"amount"`
	Received  float64 `json:"received"` // solo efectivo
	Reference string  `json:"reference"`
	// Cargo a nómina: a qué colaborador se le descuenta.
	CollaboratorID int64 `json:"collaborator_id"`
}

type validPayment struct {
	Method       string
	Amount       int64
	Received     int64
	Change       int64
	Reference    string
	Collaborator int64
}

func validatePayments(reqs []PaymentRequest, grand int64) ([]validPayment, int64, error) {
	var out []validPayment
	var sum, change int64
	for _, p := range reqs {
		if _, ok := paymentMethods[p.Method]; !ok {
			return nil, 0, fmt.Errorf("Método de pago inválido: %q", p.Method)
		}
		vp := validPayment{Method: p.Method, Amount: toCents(p.Amount), Reference: strings.TrimSpace(p.Reference)}
		if p.Method == "PAYROLL" {
			if p.CollaboratorID <= 0 {
				return nil, 0, errors.New("Elige a qué colaborador se le carga a nómina")
			}
			vp.Collaborator = p.CollaboratorID
		}
		if vp.Amount <= 0 {
			return nil, 0, errors.New("Cada pago debe ser mayor a cero")
		}
		if p.Method == "CASH" {
			vp.Received = toCents(p.Received)
			if vp.Received == 0 {
				vp.Received = vp.Amount
			}
			if vp.Received < vp.Amount {
				return nil, 0, errors.New("El efectivo recibido no cubre el pago")
			}
			vp.Change = vp.Received - vp.Amount
		}
		sum += vp.Amount
		change += vp.Change
		out = append(out, vp)
	}
	if sum != grand {
		return nil, 0, fmt.Errorf("Los pagos suman %s pero el total a cobrar es %s",
			formatMoney(fromCents(sum)), formatMoney(fromCents(grand)))
	}
	return out, change, nil
}

// POST /api/orders/{id}/pay - Cobra los productos seleccionados de una orden.
// Si ya no quedan productos pendientes, cierra la orden y libera la mesa.
func (h *POSHandler) PayOrder(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orderID, ok := pathID(r)
	if !ok {
		http.Error(w, "ID de orden inválido", http.StatusBadRequest)
		return
	}

	var req PayRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "JSON inválido", http.StatusBadRequest)
		return
	}
	if len(req.ItemIDs) == 0 {
		http.Error(w, "Selecciona al menos un producto para cobrar", http.StatusBadRequest)
		return
	}

	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		serverError(w, "Error al iniciar transacción", err)
		return
	}
	defer tx.Rollback()

	var status, orderType string
	var sessionID int64
	err = tx.QueryRowContext(ctx, `SELECT status, order_type, session_id FROM orders WHERE id = ?`, orderID).
		Scan(&status, &orderType, &sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "Orden no encontrada", http.StatusNotFound)
		return
	}
	if err != nil {
		serverError(w, "Error consultando la orden", err)
		return
	}
	if status != "ACTIVE" {
		http.Error(w, "La orden ya no está activa", http.StatusConflict)
		return
	}

	// Toda cuenta cobrada pertenece al turno de caja abierto.
	cashSessionID, err := openCashSessionID(ctx, tx)
	if errors.Is(err, errNoCashSession) {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if err != nil {
		serverError(w, "Error consultando la caja", err)
		return
	}

	settings, err := loadSettings(ctx, tx)
	if err != nil {
		serverError(w, "Error cargando configuración", err)
		return
	}

	// Precio y piezas de cada producto pendiente, con el precio guardado en la orden.
	type pendingItem struct{ unit, qty int64 }
	pending := map[int64]pendingItem{}
	rows, err := tx.QueryContext(ctx, `
		SELECT id, unit_price, quantity FROM order_items
		WHERE order_id = ? AND bill_id IS NULL AND status != 'VOID'`, orderID)
	if err != nil {
		serverError(w, "Error consultando productos", err)
		return
	}
	for rows.Next() {
		var id int64
		var price float64
		var qty int64
		if err := rows.Scan(&id, &price, &qty); err != nil {
			rows.Close()
			serverError(w, "Error leyendo productos", err)
			return
		}
		pending[id] = pendingItem{toCents(price), qty}
	}
	rows.Close()

	var itemIDs []int64
	var itemsCents int64
	seen := map[int64]bool{}
	for _, id := range req.ItemIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		it, ok := pending[id]
		if !ok {
			http.Error(w, "Algún producto seleccionado ya fue cobrado o anulado. Recarga la pantalla.", http.StatusConflict)
			return
		}
		take, ok := partialQty(req.Quantities, id, it.qty)
		if !ok {
			http.Error(w, "La cantidad a cobrar de un producto no es válida. Recarga la pantalla.", http.StatusBadRequest)
			return
		}
		itemIDs = append(itemIDs, id)
		itemsCents += it.unit * take
	}

	totals, err := computeTotals(itemsCents, req.DiscountType, req.DiscountValue, toCents(req.Tip), settings)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	payments, change, err := validatePayments(req.Payments, totals.Grand)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Cargo a nómina: lo autoriza quien está en caja con su PIN (queda como
	// responsable de haber registrado bien al colaborador).
	var authorizer User
	var collabNames map[int64]string
	hasPayroll := false
	for _, p := range payments {
		hasPayroll = hasPayroll || p.Method == "PAYROLL"
	}
	if hasPayroll {
		var ok bool
		if authorizer, ok = h.cashierByPIN(w, r, tx, req.PayrollPIN); !ok {
			return
		}
		collabNames, err = checkPayrollCharges(ctx, tx, payments)
		var bad badRequest
		if errors.As(err, &bad) {
			http.Error(w, bad.Error(), http.StatusConflict)
			return
		}
		if err != nil {
			serverError(w, "Error validando el cargo a nómina", err)
			return
		}
	}

	// Cliente: uno existente, uno nuevo, o solo el nombre en la orden.
	customerName := strings.TrimSpace(req.CustomerName)
	var customerID any
	if req.CustomerID != nil {
		if err := tx.QueryRowContext(ctx, `SELECT name FROM customers WHERE id = ?`, *req.CustomerID).Scan(&customerName); err != nil {
			http.Error(w, "Cliente no encontrado", http.StatusBadRequest)
			return
		}
		customerID = *req.CustomerID
	} else if req.NewCustomer != nil && strings.TrimSpace(req.NewCustomer.Name) != "" {
		nc := req.NewCustomer
		res, err := tx.ExecContext(ctx,
			`INSERT INTO customers (name, phone, email, rfc) VALUES (?, ?, ?, ?)`,
			strings.TrimSpace(nc.Name), nullIfEmpty(nc.Phone), nullIfEmpty(nc.Email),
			nullIfEmpty(strings.ToUpper(nc.RFC)))
		if err != nil {
			serverError(w, "Error guardando cliente", err)
			return
		}
		id, _ := res.LastInsertId()
		customerID = id
		customerName = strings.TrimSpace(nc.Name)
	}

	var billNumber int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(bill_number), 0) + 1 FROM bills WHERE order_id = ?`, orderID).Scan(&billNumber); err != nil {
		serverError(w, "Error numerando la cuenta", err)
		return
	}

	var discountType any
	if totals.Discount > 0 {
		discountType = req.DiscountType
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO bills (order_id, bill_number, subtotal, tax, discount, total, status,
		                   customer_id, discount_type, discount_value, discount_reason, tip,
		                   cash_session_id, created_at, paid_at)
		VALUES (?, ?, ?, ?, ?, ?, 'PAID', ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		orderID, billNumber, fromCents(totals.Items), fromCents(totals.Tax), fromCents(totals.Discount),
		fromCents(totals.Total), customerID, discountType, req.DiscountValue,
		nullIfEmpty(req.DiscountReason), fromCents(totals.Tip), cashSessionID)
	if err != nil {
		serverError(w, "Error creando la cuenta", err)
		return
	}
	billID, _ := res.LastInsertId()

	var payrollPayments []int64
	for _, p := range payments {
		var received, collaborator, authorizedBy any
		if p.Method == "CASH" {
			received = fromCents(p.Received)
		}
		if p.Method == "PAYROLL" {
			collaborator, authorizedBy = p.Collaborator, authorizer.ID
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO payments (bill_id, payment_method, amount, reference_code, amount_received, change_given,
			                      collaborator_id, authorized_by)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			billID, p.Method, fromCents(p.Amount), nullIfEmpty(p.Reference), received, fromCents(p.Change),
			collaborator, authorizedBy)
		if err != nil {
			serverError(w, "Error registrando pago", err)
			return
		}
		if p.Method == "PAYROLL" {
			id, _ := res.LastInsertId()
			payrollPayments = append(payrollPayments, id)
			if err := auditBy(ctx, tx, authorizer, r, "PAYROLL_CHARGE", map[string]any{
				"order_id": orderID, "bill_id": billID, "payment_id": id,
				"collaborator_id": p.Collaborator, "collaborator": collabNames[p.Collaborator],
				"amount": fromCents(p.Amount),
			}); err != nil {
				serverError(w, "Error registrando bitácora", err)
				return
			}
		}
	}

	for _, id := range itemIDs {
		// Si se cobra solo una parte de las piezas ("1 de 2× Pepperoni"), el
		// renglón se parte y a la cuenta va la parte cobrada.
		if take, _ := partialQty(req.Quantities, id, pending[id].qty); take < pending[id].qty {
			if id, err = splitOrderItem(ctx, tx, id, take); err != nil {
				serverError(w, "Error separando piezas del producto", err)
				return
			}
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE order_items SET bill_id = ? WHERE id = ? AND bill_id IS NULL`, billID, id)
		if err != nil {
			serverError(w, "Error asignando productos a la cuenta", err)
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			http.Error(w, "Algún producto ya fue cobrado. Recarga la pantalla.", http.StatusConflict)
			return
		}
	}

	if customerName != "" {
		if _, err := tx.ExecContext(ctx,
			`UPDATE orders SET customer_name = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, customerName, orderID); err != nil {
			serverError(w, "Error actualizando cliente de la orden", err)
			return
		}
	}

	remaining, err := countPendingItems(ctx, tx, orderID)
	if err != nil {
		serverError(w, "Error consultando productos pendientes", err)
		return
	}
	orderClosed := remaining == 0
	if orderClosed {
		if err := finishOrder(ctx, tx, orderID, sessionID, "PAID", ""); err != nil {
			serverError(w, "Error cerrando la orden", err)
			return
		}
	}

	if err := audit(ctx, tx, "BILL_PAID", map[string]any{
		"order_id": orderID, "bill_id": billID, "totals": totals,
		"discount_reason": req.DiscountReason, "payments": len(payments),
	}); err != nil {
		serverError(w, "Error registrando bitácora", err)
		return
	}

	if err := tx.Commit(); err != nil {
		serverError(w, "Error al confirmar el cobro", err)
		return
	}
	// El cajón no se abre solo al cobrar: se abre con su botón en la pantalla
	// de cobro, independiente del ticket.

	// Vale de consumo para que firme el colaborador.
	voucher := map[string]any{"payment_ids": payrollPayments}
	for _, id := range payrollPayments {
		out, err := printPayrollVoucher(ctx, h.DB, id)
		if err != nil {
			out.Failed = append(out.Failed, err.Error())
		}
		voucher["printed"], voucher["failed"] = out.Printed, out.Failed
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"bill_id":         billID,
		"bill_number":     billNumber,
		"total":           fromCents(totals.Grand),
		"change":          fromCents(change),
		"order_closed":    orderClosed,
		"remaining_items": remaining,
		"payroll_voucher": voucher,
	})
}

type reasonRequest struct {
	Reason string `json:"reason"`
}

func decodeReason(w http.ResponseWriter, r *http.Request) (string, bool) {
	var req reasonRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "JSON inválido", http.StatusBadRequest)
		return "", false
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		http.Error(w, "Indica el motivo", http.StatusBadRequest)
		return "", false
	}
	return reason, true
}

// closedItem es la foto de un producto al cerrar una orden sin pago; queda en
// la bitácora para poder revisar después qué se preparó y qué se entregó.
type closedItem struct {
	Name         string  `json:"name"`
	Quantity     int     `json:"quantity"`
	UnitPrice    float64 `json:"unit_price"`
	Sent         bool    `json:"sent"`               // salió en comanda
	Delivered    bool    `json:"delivered"`          // se entregó al menos una pieza
	DeliveredQty int     `json:"delivered_quantity"` // piezas marcadas en Expo
	Paid         bool    `json:"paid"`
}

// POST /api/orders/{id}/cancel - Cierra una orden sin cobrar lo pendiente.
//
// Sin cobros previos la orden queda CANCELLED; si ya tenía cuentas cobradas,
// lo pendiente se anula y la orden queda PAID por lo que sí se cobró. En ambos
// casos la bitácora guarda el motivo, la terminal y qué productos se habían
// enviado a cocina o entregado, para detectar cierres sospechosos.
func (h *POSHandler) CancelOrder(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orderID, ok := pathID(r)
	if !ok {
		http.Error(w, "ID de orden inválido", http.StatusBadRequest)
		return
	}
	reason, ok := decodeReason(w, r)
	if !ok {
		return
	}

	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		serverError(w, "Error al iniciar transacción", err)
		return
	}
	defer tx.Rollback()

	var status, orderType, title string
	var sessionID int64
	var bills int
	err = tx.QueryRowContext(ctx, `
		SELECT o.status, o.order_type, o.session_id, COALESCE(t.name, o.customer_name, ''),
		       (SELECT COUNT(*) FROM bills b WHERE b.order_id = o.id)
		FROM orders o
		LEFT JOIN table_sessions ts ON ts.id = o.session_id
		LEFT JOIN dining_tables t ON t.id = ts.table_id
		WHERE o.id = ?`, orderID).Scan(&status, &orderType, &sessionID, &title, &bills)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "Orden no encontrada", http.StatusNotFound)
		return
	}
	if err != nil {
		serverError(w, "Error consultando la orden", err)
		return
	}
	if status != "ACTIVE" {
		http.Error(w, "La orden ya no está activa", http.StatusConflict)
		return
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT p.name, oi.quantity, oi.unit_price, oi.printed_quantity > 0,
		       oi.delivered_quantity, oi.bill_id IS NOT NULL
		FROM order_items oi JOIN products p ON p.id = oi.product_id
		WHERE oi.order_id = ? AND oi.status != 'VOID'
		ORDER BY oi.id`, orderID)
	if err != nil {
		serverError(w, "Error consultando productos", err)
		return
	}
	items := []closedItem{}
	var unpaidCents int64
	var sent, delivered int
	for rows.Next() {
		var it closedItem
		if err := rows.Scan(&it.Name, &it.Quantity, &it.UnitPrice, &it.Sent, &it.DeliveredQty, &it.Paid); err != nil {
			rows.Close()
			serverError(w, "Error leyendo productos", err)
			return
		}
		if !it.Paid {
			unpaidCents += toCents(it.UnitPrice) * int64(it.Quantity)
		}
		if it.Sent {
			sent += it.Quantity
		}
		it.Delivered = it.DeliveredQty > 0
		delivered += it.DeliveredQty
		items = append(items, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		serverError(w, "Error leyendo productos", err)
		return
	}

	newStatus, action := "CANCELLED", "ORDER_CANCELLED"
	if bills > 0 {
		newStatus, action = "PAID", "ORDER_CLOSED_WITH_BALANCE"
		if _, err := tx.ExecContext(ctx, `
			UPDATE order_items SET status = 'VOID'
			WHERE order_id = ? AND bill_id IS NULL AND status != 'VOID'`, orderID); err != nil {
			serverError(w, "Error anulando lo pendiente", err)
			return
		}
	}
	if err := finishOrder(ctx, tx, orderID, sessionID, newStatus, reason); err != nil {
		serverError(w, "Error cerrando la orden", err)
		return
	}
	if err := audit(ctx, tx, action, map[string]any{
		"order_id":        orderID,
		"order_type":      orderType,
		"title":           title,
		"reason":          reason,
		"unpaid_amount":   fromCents(unpaidCents),
		"paid_bills":      bills,
		"comanda_sent":    sent > 0,
		"items_sent":      sent,
		"items_delivered": delivered,
		"items":           items,
		"terminal":        terminalOf(r),
	}); err != nil {
		serverError(w, "Error registrando bitácora", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "Error al confirmar el cierre", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"order_status": newStatus, "unpaid_amount": fromCents(unpaidCents)})
}

// terminalOf identifica desde qué equipo se hizo una operación mientras no
// haya usuarios con PIN.
func terminalOf(r *http.Request) map[string]string {
	ip := r.RemoteAddr
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}
	return map[string]string{"ip": ip, "user_agent": r.UserAgent()}
}

// POST /api/order-items/{id}/void - Anula un producto pendiente de cobro.
func (h *POSHandler) VoidItem(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	itemID, ok := pathID(r)
	if !ok {
		http.Error(w, "ID de producto inválido", http.StatusBadRequest)
		return
	}
	reason, ok := decodeReason(w, r)
	if !ok {
		return
	}

	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		serverError(w, "Error al iniciar transacción", err)
		return
	}
	defer tx.Rollback()

	var orderID, sessionID int64
	var billID sql.NullInt64
	var itemStatus, orderStatus, productName string
	var qty int
	var price float64
	err = tx.QueryRowContext(ctx, `
		SELECT oi.order_id, oi.bill_id, oi.status, o.status, o.session_id, p.name, oi.quantity, oi.unit_price
		FROM order_items oi
		JOIN orders o ON o.id = oi.order_id
		JOIN products p ON p.id = oi.product_id
		WHERE oi.id = ?`, itemID).
		Scan(&orderID, &billID, &itemStatus, &orderStatus, &sessionID, &productName, &qty, &price)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "Producto no encontrado", http.StatusNotFound)
		return
	}
	if err != nil {
		serverError(w, "Error consultando el producto", err)
		return
	}
	switch {
	case orderStatus != "ACTIVE":
		http.Error(w, "La orden ya no está activa", http.StatusConflict)
		return
	case billID.Valid:
		http.Error(w, "El producto ya fue cobrado", http.StatusConflict)
		return
	case itemStatus == "VOID":
		http.Error(w, "El producto ya estaba anulado", http.StatusConflict)
		return
	}

	if _, err := tx.ExecContext(ctx, `UPDATE order_items SET status = 'VOID' WHERE id = ?`, itemID); err != nil {
		serverError(w, "Error anulando el producto", err)
		return
	}
	if err := audit(ctx, tx, "ITEM_VOID", map[string]any{
		"order_id": orderID, "order_item_id": itemID, "product": productName,
		"quantity": qty, "unit_price": price, "reason": reason,
	}); err != nil {
		serverError(w, "Error registrando bitácora", err)
		return
	}

	// Si ya no queda nada por cobrar, la orden se cierra sola.
	remaining, err := countPendingItems(ctx, tx, orderID)
	if err != nil {
		serverError(w, "Error consultando productos pendientes", err)
		return
	}
	newStatus := "ACTIVE"
	if remaining == 0 {
		var bills int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM bills WHERE order_id = ?`, orderID).Scan(&bills); err != nil {
			serverError(w, "Error consultando cuentas", err)
			return
		}
		newStatus = "CANCELLED"
		cancelReason := "Todos los productos fueron anulados"
		if bills > 0 {
			newStatus, cancelReason = "PAID", ""
		}
		if err := finishOrder(ctx, tx, orderID, sessionID, newStatus, cancelReason); err != nil {
			serverError(w, "Error cerrando la orden", err)
			return
		}
	}

	if err := tx.Commit(); err != nil {
		serverError(w, "Error al confirmar la anulación", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"order_status": newStatus})
}

// POST /api/orders/{id}/deliver - Marca una orden como entregada al cliente.
func (h *POSHandler) DeliverOrder(w http.ResponseWriter, r *http.Request) {
	orderID, ok := pathID(r)
	if !ok {
		http.Error(w, "ID de orden inválido", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		serverError(w, "Error al iniciar transacción", err)
		return
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `
		UPDATE orders SET delivered_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND delivered_at IS NULL AND status != 'CANCELLED'`, orderID)
	if err != nil {
		serverError(w, "Error marcando la orden como entregada", err)
		return
	}
	// Entregar la orden completa tacha también los productos que faltaban.
	if n, _ := res.RowsAffected(); n > 0 {
		if _, err := tx.ExecContext(ctx, `
			UPDATE order_items SET delivered_quantity = quantity, delivered_at = COALESCE(delivered_at, CURRENT_TIMESTAMP)
			WHERE order_id = ? AND status != 'VOID' AND delivered_quantity < quantity`, orderID); err != nil {
			serverError(w, "Error marcando los productos como entregados", err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "Error al confirmar la entrega", err)
		return
	}
	// Refresca los paneles HTMX que escuchan "reload from:body".
	w.Header().Set("HX-Trigger", "reload")
	w.WriteHeader(http.StatusOK)
}

type CustomerResult struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Phone string `json:"phone"`
	Email string `json:"email"`
	RFC   string `json:"rfc"`
}

// GET /api/customers?q= - Busca clientes por nombre, teléfono o RFC.
func (h *POSHandler) SearchCustomers(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	like := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(q) + "%"

	rows, err := h.DB.QueryContext(r.Context(), `
		SELECT id, name, COALESCE(phone, ''), COALESCE(email, ''), COALESCE(rfc, '')
		FROM customers
		WHERE name LIKE ? ESCAPE '\' OR phone LIKE ? ESCAPE '\' OR rfc LIKE ? ESCAPE '\'
		ORDER BY name LIMIT 10`, like, like, like)
	if err != nil {
		serverError(w, "Error buscando clientes", err)
		return
	}
	defer rows.Close()

	results := []CustomerResult{}
	for rows.Next() {
		var c CustomerResult
		if err := rows.Scan(&c.ID, &c.Name, &c.Phone, &c.Email, &c.RFC); err != nil {
			serverError(w, "Error leyendo clientes", err)
			return
		}
		results = append(results, c)
	}
	writeJSON(w, http.StatusOK, results)
}

func countPendingItems(ctx context.Context, tx *sql.Tx, orderID int64) (int, error) {
	var n int
	err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM order_items
		WHERE order_id = ? AND bill_id IS NULL AND status != 'VOID'`, orderID).Scan(&n)
	return n, err
}

// finishOrder cierra la orden y, si era la última activa de la sesión,
// cierra la sesión y libera la mesa.
func finishOrder(ctx context.Context, tx *sql.Tx, orderID, sessionID int64, status, cancelReason string) error {
	// En comedor la comida ya se entregó al cobrar; las canceladas no deben
	// quedarse esperando entrega en Expo.
	if _, err := tx.ExecContext(ctx, `
		UPDATE orders SET
			status = ?, cancel_reason = ?, closed_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP,
			delivered_at = CASE WHEN order_type = 'DINE_IN' OR ? = 'CANCELLED'
			                    THEN COALESCE(delivered_at, CURRENT_TIMESTAMP) ELSE delivered_at END
		WHERE id = ?`, status, nullIfEmpty(cancelReason), status, orderID); err != nil {
		return err
	}

	var active int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM orders WHERE session_id = ? AND status = 'ACTIVE'`, sessionID).Scan(&active); err != nil {
		return err
	}
	if active > 0 {
		return nil
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE table_sessions SET status = 'CLOSED', closed_at = CURRENT_TIMESTAMP
		WHERE id = ? AND status = 'OPEN'`, sessionID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `
		UPDATE dining_tables SET status = 'FREE'
		WHERE id = (SELECT table_id FROM table_sessions WHERE id = ?)`, sessionID)
	return err
}

func audit(ctx context.Context, tx *sql.Tx, action string, details any) error {
	b, err := json.Marshal(details)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO audit_logs (action, details) VALUES (?, ?)`, action, string(b))
	return err
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

func nullIfEmpty(s string) any {
	if s = strings.TrimSpace(s); s == "" {
		return nil
	}
	return s
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// partialQty regresa cuántas piezas de un renglón se cobran: todas, salvo que
// la petición indique menos. ok=false si la cantidad no es válida.
func partialQty(quantities map[int64]int, id, total int64) (int64, bool) {
	q, ok := quantities[id]
	if !ok {
		return total, true
	}
	if q < 1 || int64(q) > total {
		return 0, false
	}
	return int64(q), true
}

// splitOrderItem separa take piezas de un renglón en un renglón nuevo (que es
// el que se cobra) y deja el resto pendiente en el original. Lo impreso en
// comanda y lo entregado en Expo se reparte entre ambos (primero a la parte
// cobrada), así no se reimprime nada ni se pierden los tiempos de salida.
func splitOrderItem(ctx context.Context, tx *sql.Tx, id, take int64) (int64, error) {
	var qty, printed, delivered int64
	var deliveredAt sql.NullString
	if err := tx.QueryRowContext(ctx, `
		SELECT quantity, printed_quantity, delivered_quantity, delivered_at FROM order_items WHERE id = ?`, id).
		Scan(&qty, &printed, &delivered, &deliveredAt); err != nil {
		return 0, err
	}
	if take >= qty {
		return id, nil
	}
	partPrinted, partDelivered := min(take, printed), min(take, delivered)
	// Una parte con todas sus piezas entregadas conserva la hora de entrega
	// (o toma la actual si el renglón completo aún no estaba entregado).
	doneAt := func(n, d int64) any {
		if d < n {
			return nil
		}
		if deliveredAt.Valid {
			return deliveredAt.String
		}
		return time.Now().UTC().Format("2006-01-02 15:04:05")
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO order_items (order_id, product_id, unit_price, quantity, printed_quantity, notes, status,
		                         created_at, delivered_at, modifiers_text, delivered_quantity, guest_id)
		SELECT order_id, product_id, unit_price, ?, ?, notes, status, created_at, ?, modifiers_text, ?, guest_id
		FROM order_items WHERE id = ?`, take, partPrinted, doneAt(take, partDelivered), partDelivered, id)
	if err != nil {
		return 0, err
	}
	newID, _ := res.LastInsertId()
	rest, restDelivered := qty-take, delivered-partDelivered
	if _, err := tx.ExecContext(ctx, `
		UPDATE order_items SET quantity = ?, printed_quantity = ?, delivered_quantity = ?, delivered_at = ?
		WHERE id = ?`, rest, printed-partPrinted, restDelivered, doneAt(rest, restDelivered), id); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO order_item_modifiers (order_item_id, modifier_option_id, unit_price, name)
		SELECT ?, modifier_option_id, unit_price, name FROM order_item_modifiers WHERE order_item_id = ?`, newID, id); err != nil {
		return 0, err
	}
	return newID, nil
}
