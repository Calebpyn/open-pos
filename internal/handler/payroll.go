package handler

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/calebpyn/open-pos/internal/printer"
)

// --- Cargo a nómina ---
//
// Los colaboradores consumen con su descuento y no pagan en caja: el pago
// "Cargo a nómina" registra a quién se carga y qué cajero lo autorizó con su
// PIN. Cada semana (lunes a domingo) el admin marca como descontado lo que se
// aplicó en la nómina.

type Collaborator struct {
	ID          int64    `json:"id"`
	Name        string   `json:"name"`
	IsActive    bool     `json:"is_active"`
	WeeklyLimit *float64 `json:"weekly_limit"`
}

func loadCollaborators(ctx context.Context, q queryer, onlyActive bool) ([]Collaborator, error) {
	query := `SELECT id, name, is_active, weekly_limit FROM collaborators`
	if onlyActive {
		query += ` WHERE is_active = 1`
	}
	out := []Collaborator{}
	err := eachRow(ctx, q, query+` ORDER BY name COLLATE NOCASE`, nil, func(scan func(...any) error) error {
		var c Collaborator
		var limit sql.NullFloat64
		if err := scan(&c.ID, &c.Name, &c.IsActive, &limit); err != nil {
			return err
		}
		if limit.Valid {
			c.WeeklyLimit = &limit.Float64
		}
		out = append(out, c)
		return nil
	})
	return out, err
}

// weekStart es el lunes (a medianoche local) de la semana de t.
func weekStart(t time.Time) time.Time {
	t = t.Local()
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.Local)
	offset := (int(d.Weekday()) + 6) % 7 // lunes = 0
	return d.AddDate(0, 0, -offset)
}

func utcSQL(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05") }

func weekLabel(start time.Time) string {
	end := start.AddDate(0, 0, 6)
	return fmt.Sprintf("%s al %s", start.Format("02/01"), end.Format("02/01/2006"))
}

// checkPayrollCharges valida los cargos a nómina de un cobro: colaborador
// activo y que no rebase su tope semanal. Regresa los nombres por id.
func checkPayrollCharges(ctx context.Context, tx *sql.Tx, payments []validPayment) (map[int64]string, error) {
	perCollab := map[int64]int64{}
	for _, p := range payments {
		if p.Method == "PAYROLL" {
			perCollab[p.Collaborator] += p.Amount
		}
	}
	names := map[int64]string{}
	start := weekStart(time.Now())
	for id, amount := range perCollab {
		var name string
		var active bool
		var limit sql.NullFloat64
		err := tx.QueryRowContext(ctx, `SELECT name, is_active, weekly_limit FROM collaborators WHERE id = ?`, id).
			Scan(&name, &active, &limit)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && !active) {
			return nil, badRequest("El colaborador elegido no existe o está dado de baja")
		}
		if err != nil {
			return nil, err
		}
		names[id] = name
		if !limit.Valid {
			continue
		}
		var used float64
		if err := tx.QueryRowContext(ctx, `
			SELECT COALESCE(SUM(amount), 0) FROM payments
			WHERE payment_method = 'PAYROLL' AND collaborator_id = ? AND created_at >= ?`,
			id, utcSQL(start)).Scan(&used); err != nil {
			return nil, err
		}
		if toCents(used)+amount > toCents(limit.Float64) {
			left := max(0, toCents(limit.Float64)-toCents(used))
			return nil, badRequest(fmt.Sprintf("%s rebasaría su tope semanal de %s (le quedan %s). Cobra el resto con otro método.",
				name, formatMoney(limit.Float64), formatMoney(fromCents(left))))
		}
	}
	return names, nil
}

// cashierByPIN valida el PIN de quien autoriza desde caja (un cargo a
// nómina, editar una orden), con el mismo límite de intentos que la caja.
func (h *POSHandler) cashierByPIN(w http.ResponseWriter, r *http.Request, q queryer, pin string) (User, bool) {
	ip := terminalOf(r)["ip"]
	if d := h.logins.wait(ip); d > 0 {
		http.Error(w, fmt.Sprintf("Demasiados intentos de PIN. Espera %d segundos.", int(d.Seconds())+1), http.StatusTooManyRequests)
		return User{}, false
	}
	u, err := userByPIN(r.Context(), q, pin)
	if errors.Is(err, errBadPIN) {
		h.logins.fail(ip)
		http.Error(w, "PIN de caja incorrecto", http.StatusUnauthorized)
		return User{}, false
	}
	if err != nil {
		serverError(w, "Error validando PIN", err)
		return User{}, false
	}
	h.logins.success(ip)
	if !canOperateCash(u.Role) {
		http.Error(w, "Tu usuario ("+roleLabels[u.Role]+") no puede autorizar esto desde caja", http.StatusForbidden)
		return User{}, false
	}
	return u, true
}

// --- Vale de consumo ---

type payrollCharge struct {
	PaymentID    int64
	BillID       int64
	Collaborator string
	Amount       float64
	AuthorizedBy string
	At           time.Time
}

func loadPayrollCharge(ctx context.Context, q queryer, paymentID int64) (*payrollCharge, error) {
	c := &payrollCharge{PaymentID: paymentID}
	err := q.QueryRowContext(ctx, `
		SELECT p.bill_id, COALESCE(co.name, ''), p.amount, COALESCE(u.name, ''), p.created_at
		FROM payments p
		LEFT JOIN collaborators co ON co.id = p.collaborator_id
		LEFT JOIN users u ON u.id = p.authorized_by
		WHERE p.id = ? AND p.payment_method = 'PAYROLL'`, paymentID).
		Scan(&c.BillID, &c.Collaborator, &c.Amount, &c.AuthorizedBy, &c.At)
	return c, err
}

// payrollVoucherDoc es el comprobante que firma el colaborador: qué consumió
// y cuánto se le descontará de la nómina.
func payrollVoucherDoc(t *TicketData, c *payrollCharge, paperWidth int) *printer.Ticket {
	tk := printer.NewTicket(paperWidth)
	m := formatMoney
	tk.Align(printer.AlignCenter).Bold(true).Line(t.BusinessName).Bold(false).
		Bold(true).Size(1, 2).Line("VALE DE CONSUMO").Size(1, 1).Line("CARGO A NÓMINA").Bold(false).
		Line(c.At.Local().Format("02/01/2006 15:04")).
		Align(printer.AlignLeft).Separator().
		Bold(true).Columns("Colaborador", c.Collaborator).Bold(false).
		Columns(fmt.Sprintf("Orden #%d", t.OrderID), fmt.Sprintf("Cuenta %d", t.BillNumber))
	if t.TableName != "" {
		tk.Columns("Mesa", t.TableName)
	}
	tk.Separator()
	for _, it := range t.Items {
		tk.Columns(fmt.Sprintf("%d %s", it.Quantity, it.Name), m(it.Amount))
	}
	tk.Separator().Columns("Subtotal", m(t.Subtotal))
	if t.Discount > 0 {
		label := "Descuento"
		if t.DiscountReason != "" {
			label += " (" + t.DiscountReason + ")"
		}
		tk.Columns(label, "-"+m(t.Discount))
	}
	tk.Columns("Total", m(t.Total))
	if len(t.Payments) > 1 {
		for _, p := range t.Payments {
			tk.Columns("  "+p.Method, m(p.Amount))
		}
	}
	tk.Bold(true).Size(1, 2).Columns("A DESCONTAR", m(c.Amount)).Size(1, 1).Bold(false).
		Line("Semana del "+weekLabel(weekStart(c.At))).
		Columns("Autorizó (caja)", c.AuthorizedBy).
		Feed(1).Line("Acepto que este importe se").Line("descuente de mi nómina semanal.").
		Feed(3).Align(printer.AlignCenter).Line(strings.Repeat("_", min(28, tk.Cols()))).
		Line("Firma del colaborador").Line(c.Collaborator).
		Feed(3).Cut()
	return tk
}

func printPayrollVoucher(ctx context.Context, db queryer, paymentID int64) (PrintOutcome, error) {
	c, err := loadPayrollCharge(ctx, db, paymentID)
	if err != nil {
		return newOutcome(), err
	}
	t, err := loadTicket(ctx, db, c.BillID)
	if err != nil {
		return newOutcome(), err
	}
	return printToReceiptPrinters(ctx, db, func(width int) *printer.Ticket { return payrollVoucherDoc(t, c, width) })
}

// POST /api/payments/{id}/voucher - Reimprime el vale de un cargo a nómina.
func (h *POSHandler) PrintPayrollVoucher(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.Error(w, "ID inválido", http.StatusBadRequest)
		return
	}
	out, err := printPayrollVoucher(r.Context(), h.DB, id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		http.Error(w, "Cargo a nómina no encontrado", http.StatusNotFound)
		return
	case errors.Is(err, errNoReceiptPrinter):
		http.Error(w, err.Error(), http.StatusConflict)
		return
	case err != nil:
		serverError(w, "Error imprimiendo el vale", err)
		return
	}
	if len(out.Printed) == 0 {
		http.Error(w, "No se pudo imprimir en "+strings.Join(out.Failed, "; "), http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"printer": strings.Join(out.Printed, ", ")})
}

// GET /api/collaborators - Colaboradores activos (para el cobro).
func (h *POSHandler) ListCollaborators(w http.ResponseWriter, r *http.Request) {
	list, err := loadCollaborators(r.Context(), h.DB, true)
	if err != nil {
		serverError(w, "Error consultando colaboradores", err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// --- Admin: colaboradores ---

// GET /api/admin/collaborators
func (h *POSHandler) AdminListCollaborators(w http.ResponseWriter, r *http.Request) {
	list, err := loadCollaborators(r.Context(), h.DB, false)
	if err != nil {
		serverError(w, "Error consultando colaboradores", err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func decodeCollaborator(w http.ResponseWriter, r *http.Request) (*Collaborator, bool) {
	var c Collaborator
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		http.Error(w, "JSON inválido", http.StatusBadRequest)
		return nil, false
	}
	c.Name = strings.TrimSpace(c.Name)
	if c.Name == "" {
		http.Error(w, "Escribe el nombre del colaborador", http.StatusBadRequest)
		return nil, false
	}
	if c.WeeklyLimit != nil && *c.WeeklyLimit <= 0 {
		c.WeeklyLimit = nil // 0 o vacío = sin tope
	}
	return &c, true
}

// POST /api/admin/collaborators
func (h *POSHandler) AdminCreateCollaborator(w http.ResponseWriter, r *http.Request) {
	c, ok := decodeCollaborator(w, r)
	if !ok {
		return
	}
	h.adminWrite(w, r, "COLLABORATOR_CREATED", func(tx *sql.Tx) (map[string]any, error) {
		res, err := tx.ExecContext(r.Context(),
			`INSERT INTO collaborators (name, is_active, weekly_limit) VALUES (?, 1, ?)`, c.Name, c.WeeklyLimit)
		if err != nil {
			return nil, err
		}
		c.ID, _ = res.LastInsertId()
		c.IsActive = true
		return map[string]any{"collaborator": c}, nil
	})
}

// PUT /api/admin/collaborators/{id}
func (h *POSHandler) AdminUpdateCollaborator(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.Error(w, "ID inválido", http.StatusBadRequest)
		return
	}
	c, ok := decodeCollaborator(w, r)
	if !ok {
		return
	}
	c.ID = id
	h.adminWrite(w, r, "COLLABORATOR_UPDATED", func(tx *sql.Tx) (map[string]any, error) {
		res, err := tx.ExecContext(r.Context(),
			`UPDATE collaborators SET name = ?, is_active = ?, weekly_limit = ? WHERE id = ?`,
			c.Name, c.IsActive, c.WeeklyLimit, id)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil, errNotFound
		}
		return map[string]any{"collaborator": c}, nil
	})
}

// --- Admin: nómina semanal ---

type PayrollEntry struct {
	PaymentID    int64     `json:"payment_id"`
	At           time.Time `json:"at"`
	OrderID      int64     `json:"order_id"`
	BillID       int64     `json:"bill_id"`
	BillNumber   int       `json:"bill_number"`
	Amount       float64   `json:"amount"`
	Discount     float64   `json:"discount"`
	Items        string    `json:"items"`
	AuthorizedBy string    `json:"authorized_by"`
	Settled      bool      `json:"settled"`
}

type PayrollRow struct {
	CollaboratorID int64          `json:"collaborator_id"`
	Name           string         `json:"name"`
	Total          float64        `json:"total"`
	Pending        float64        `json:"pending"` // aún no descontado
	SettlementIDs  []int64        `json:"settlement_ids"`
	Entries        []PayrollEntry `json:"entries"`
}

func parseWeek(r *http.Request) (time.Time, error) {
	w := r.URL.Query().Get("week")
	if w == "" {
		return weekStart(time.Now()), nil
	}
	t, err := time.ParseInLocation(dateLayout, w, time.Local)
	if err != nil {
		return time.Time{}, badRequest("Semana inválida")
	}
	return weekStart(t), nil
}

func loadPayrollWeek(ctx context.Context, q queryer, start time.Time) ([]*PayrollRow, error) {
	end := start.AddDate(0, 0, 7)
	rows := map[int64]*PayrollRow{}
	var order []*PayrollRow
	err := eachRow(ctx, q, `
		SELECT p.id, p.created_at, b.order_id, b.id, b.bill_number, p.amount, b.discount,
		       COALESCE((SELECT GROUP_CONCAT(oi.quantity || ' ' || pr.name, ', ')
		                 FROM order_items oi JOIN products pr ON pr.id = oi.product_id WHERE oi.bill_id = b.id), ''),
		       COALESCE(u.name, ''), p.settlement_id, p.collaborator_id, COALESCE(co.name, '(sin colaborador)')
		FROM payments p
		JOIN bills b ON b.id = p.bill_id
		LEFT JOIN users u ON u.id = p.authorized_by
		LEFT JOIN collaborators co ON co.id = p.collaborator_id
		WHERE p.payment_method = 'PAYROLL' AND p.created_at >= ? AND p.created_at < ?
		ORDER BY p.created_at`, []any{utcSQL(start), utcSQL(end)}, func(scan func(...any) error) error {
		var e PayrollEntry
		var settlement, collab sql.NullInt64
		var name string
		if err := scan(&e.PaymentID, &e.At, &e.OrderID, &e.BillID, &e.BillNumber, &e.Amount, &e.Discount,
			&e.Items, &e.AuthorizedBy, &settlement, &collab, &name); err != nil {
			return err
		}
		e.Settled = settlement.Valid
		row := rows[collab.Int64]
		if row == nil {
			row = &PayrollRow{CollaboratorID: collab.Int64, Name: name, SettlementIDs: []int64{}, Entries: []PayrollEntry{}}
			rows[collab.Int64] = row
			order = append(order, row)
		}
		row.Entries = append(row.Entries, e)
		row.Total = fromCents(toCents(row.Total) + toCents(e.Amount))
		if e.Settled {
			found := false
			for _, id := range row.SettlementIDs {
				found = found || id == settlement.Int64
			}
			if !found {
				row.SettlementIDs = append(row.SettlementIDs, settlement.Int64)
			}
		} else {
			row.Pending = fromCents(toCents(row.Pending) + toCents(e.Amount))
		}
		return nil
	})
	sort.Slice(order, func(i, j int) bool { return strings.ToLower(order[i].Name) < strings.ToLower(order[j].Name) })
	return order, err
}

// GET /api/admin/payroll?week=AAAA-MM-DD
func (h *POSHandler) AdminPayrollWeek(w http.ResponseWriter, r *http.Request) {
	start, err := parseWeek(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rows, err := loadPayrollWeek(r.Context(), h.DB, start)
	if err != nil {
		serverError(w, "Error consultando cargos a nómina", err)
		return
	}
	var total, pending int64
	for _, row := range rows {
		total += toCents(row.Total)
		pending += toCents(row.Pending)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"week_start": start.Format(dateLayout), "week_end": start.AddDate(0, 0, 6).Format(dateLayout),
		"label": weekLabel(start), "rows": rows, "total": fromCents(total), "pending": fromCents(pending),
	})
}

// GET /api/admin/payroll.csv?week= - Para quien hace la nómina.
func (h *POSHandler) AdminPayrollCSV(w http.ResponseWriter, r *http.Request) {
	start, err := parseWeek(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rows, err := loadPayrollWeek(r.Context(), h.DB, start)
	if err != nil {
		serverError(w, "Error consultando cargos a nómina", err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="cargos_nomina_`+start.Format(dateLayout)+`.csv"`)
	w.Write([]byte{0xEF, 0xBB, 0xBF})
	money := func(v float64) string { return strconv.FormatFloat(fromCents(toCents(v)), 'f', 2, 64) }
	cw := csv.NewWriter(w)
	cw.Write([]string{"Colaborador", "Fecha", "Orden", "Cuenta", "Consumo", "Descuento", "A descontar", "Autorizó", "Descontado"})
	for _, row := range rows {
		for _, e := range row.Entries {
			settled := "No"
			if e.Settled {
				settled = "Sí"
			}
			cw.Write([]string{csvText(row.Name), e.At.Local().Format("2006-01-02 15:04"), strconv.FormatInt(e.OrderID, 10),
				strconv.Itoa(e.BillNumber), csvText(e.Items), money(e.Discount), money(e.Amount), csvText(e.AuthorizedBy), settled})
		}
		cw.Write([]string{csvText(row.Name), "", "", "", "TOTAL DE LA SEMANA", "", money(row.Total), "", ""})
	}
	cw.Flush()
}

type settleRequest struct {
	CollaboratorID int64  `json:"collaborator_id"`
	Week           string `json:"week"`
}

// POST /api/admin/payroll/settle - Marca como descontados los cargos
// pendientes de un colaborador en una semana.
func (h *POSHandler) AdminSettlePayroll(w http.ResponseWriter, r *http.Request) {
	var req settleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "JSON inválido", http.StatusBadRequest)
		return
	}
	t, err := time.ParseInLocation(dateLayout, req.Week, time.Local)
	if err != nil {
		http.Error(w, "Semana inválida", http.StatusBadRequest)
		return
	}
	start := weekStart(t)
	admin, _ := currentUser(r.Context())
	h.adminWrite(w, r, "PAYROLL_SETTLED", func(tx *sql.Tx) (map[string]any, error) {
		var amount float64
		var n int
		args := []any{req.CollaboratorID, utcSQL(start), utcSQL(start.AddDate(0, 0, 7))}
		const pending = `payment_method = 'PAYROLL' AND collaborator_id = ? AND settlement_id IS NULL
		                 AND created_at >= ? AND created_at < ?`
		if err := tx.QueryRowContext(r.Context(), `SELECT COUNT(*), COALESCE(SUM(amount), 0) FROM payments WHERE `+pending, args...).
			Scan(&n, &amount); err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, badRequest("No hay cargos pendientes de descontar en esa semana")
		}
		res, err := tx.ExecContext(r.Context(), `
			INSERT INTO payroll_settlements (collaborator_id, week_start, amount, settled_by) VALUES (?, ?, ?, ?)`,
			req.CollaboratorID, start.Format(dateLayout), amount, admin.ID)
		if err != nil {
			return nil, err
		}
		id, _ := res.LastInsertId()
		if _, err := tx.ExecContext(r.Context(), `UPDATE payments SET settlement_id = ? WHERE `+pending,
			append([]any{id}, args...)...); err != nil {
			return nil, err
		}
		return map[string]any{"settlement_id": id, "collaborator_id": req.CollaboratorID,
			"week": start.Format(dateLayout), "amount": amount, "charges": n}, nil
	})
}

// DELETE /api/admin/payroll/settlements/{id} - Deshace un "descontado".
func (h *POSHandler) AdminUndoSettlement(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.Error(w, "ID inválido", http.StatusBadRequest)
		return
	}
	h.adminWrite(w, r, "PAYROLL_SETTLEMENT_UNDONE", func(tx *sql.Tx) (map[string]any, error) {
		if _, err := tx.ExecContext(r.Context(), `UPDATE payments SET settlement_id = NULL WHERE settlement_id = ?`, id); err != nil {
			return nil, err
		}
		res, err := tx.ExecContext(r.Context(), `DELETE FROM payroll_settlements WHERE id = ?`, id)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil, errNotFound
		}
		return map[string]any{"settlement_id": id}, nil
	})
}

type reclassifyRequest struct {
	CollaboratorID int64 `json:"collaborator_id"`
}

// PUT /api/admin/payments/{id}/payroll - Convierte un pago registrado como
// "Otro" en cargo a nómina de un colaborador (consumos capturados antes de
// existir el método). Queda en la bitácora.
func (h *POSHandler) AdminReclassifyPayment(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.Error(w, "ID inválido", http.StatusBadRequest)
		return
	}
	var req reclassifyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "JSON inválido", http.StatusBadRequest)
		return
	}
	admin, _ := currentUser(r.Context())
	h.adminWrite(w, r, "PAYMENT_RECLASSIFIED", func(tx *sql.Tx) (map[string]any, error) {
		var method string
		var orderID int64
		var amount float64
		err := tx.QueryRowContext(r.Context(), `
			SELECT p.payment_method, b.order_id, p.amount FROM payments p JOIN bills b ON b.id = p.bill_id WHERE p.id = ?`, id).
			Scan(&method, &orderID, &amount)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNotFound
		}
		if err != nil {
			return nil, err
		}
		if method != "OTHER" {
			return nil, badRequest("Solo los pagos registrados como \"Otro\" se pueden pasar a cargo a nómina")
		}
		var name string
		if err := tx.QueryRowContext(r.Context(), `SELECT name FROM collaborators WHERE id = ?`, req.CollaboratorID).Scan(&name); err != nil {
			return nil, badRequest("Elige un colaborador")
		}
		if _, err := tx.ExecContext(r.Context(), `
			UPDATE payments SET payment_method = 'PAYROLL', collaborator_id = ?, authorized_by = ? WHERE id = ?`,
			req.CollaboratorID, admin.ID, id); err != nil {
			return nil, err
		}
		return map[string]any{"payment_id": id, "order_id": orderID, "amount": amount,
			"collaborator": name, "from": "OTHER", "to": "PAYROLL"}, nil
	})
}
