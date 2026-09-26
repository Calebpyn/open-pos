package handler

// Servicio de impresión: decide a qué impresoras va cada documento, lo arma
// con su formato (print_docs.go, print_formats.go) y reporta el resultado de
// la misma forma para todos. El envío pasa por la cola de cada impresora
// (printer.Send / printer.OpenDrawer), que los saca uno por uno y en orden.
//
//	Ticket de compra  -> impresoras con "Tickets de cobro"
//	Corte de caja     -> impresoras con "Tickets de cobro"
//	Comandas          -> impresoras de cada área de producción (una por área)
//	Cajón             -> impresoras con "Tiene cajón de dinero conectado"

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/calebpyn/open-pos/internal/printer"
)

var (
	errNoReceiptPrinter = errors.New("Ninguna impresora activa tiene habilitados los tickets de cobro. Configúralo en Dispositivos.")
	errNoDrawer         = errors.New("Ninguna impresora tiene un cajón de dinero conectado. Actívalo en Dispositivos.")
)

// PrintOutcome es el resultado de mandar un documento a una o varias impresoras.
type PrintOutcome struct {
	Printed []string `json:"printed"` // impresoras donde salió (sin repetir)
	Failed  []string `json:"failed"`  // "Impresora: motivo"
}

func newOutcome() PrintOutcome { return PrintOutcome{Printed: []string{}, Failed: []string{}} }

func (o *PrintOutcome) record(p *PrinterView, err error) {
	if err != nil {
		o.Failed = append(o.Failed, p.Name+": "+err.Error())
		return
	}
	for _, n := range o.Printed {
		if n == p.Name {
			return // una impresora con varias áreas imprime varias comandas
		}
	}
	o.Printed = append(o.Printed, p.Name)
}

func (o PrintOutcome) attempted() bool { return len(o.Printed)+len(o.Failed) > 0 }

// respond contesta en texto plano: 200 si salió todo, 502 si algo falló.
func (o PrintOutcome) respond(w http.ResponseWriter, done string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if len(o.Failed) > 0 {
		w.WriteHeader(http.StatusBadGateway)
		msg := "No se pudo imprimir en " + strings.Join(o.Failed, "; ")
		if len(o.Printed) > 0 {
			msg += "\nSí se imprimió en: " + strings.Join(o.Printed, ", ")
		}
		w.Write([]byte(msg))
		return
	}
	w.Write([]byte(done + strings.Join(o.Printed, ", ")))
}

func targetOf(p *PrinterView) printer.Target {
	return printer.Target{
		ConnectionType: p.ConnectionType,
		SystemName:     p.SystemName,
		IPAddress:      p.IPAddress,
		Port:           p.Port,
	}
}

// printRouting son las impresoras activas y qué recibe cada una.
type printRouting struct{ printers []PrinterView }

func loadRouting(ctx context.Context, q queryer) (*printRouting, error) {
	all, err := loadPrinters(ctx, q)
	if err != nil {
		return nil, err
	}
	r := &printRouting{}
	for _, p := range all {
		if p.IsActive {
			r.printers = append(r.printers, p)
		}
	}
	return r, nil
}

func (r *printRouting) filter(keep func(*PrinterView) bool) []*PrinterView {
	var out []*PrinterView
	for i := range r.printers {
		if keep(&r.printers[i]) {
			out = append(out, &r.printers[i])
		}
	}
	return out
}

func (r *printRouting) receipts() []*PrinterView {
	return r.filter(func(p *PrinterView) bool { return p.PrintsReceipts })
}

func (r *printRouting) drawers() []*PrinterView {
	return r.filter(func(p *PrinterView) bool { return p.HasDrawer })
}

func (r *printRouting) forArea(areaID int64) []*PrinterView {
	return r.filter(func(p *PrinterView) bool {
		for _, id := range p.AreaIDs {
			if id == areaID {
				return true
			}
		}
		return false
	})
}

// sendDoc arma el documento al ancho de papel de la impresora y lo imprime.
func sendDoc(ctx context.Context, p *PrinterView, doc func(paperWidth int) *printer.Ticket) error {
	_, err := printer.Send(ctx, targetOf(p), doc(p.PaperWidth).Bytes())
	return err
}

// printToReceiptPrinters manda un documento a todas las impresoras de tickets.
func printToReceiptPrinters(ctx context.Context, db queryer, doc func(paperWidth int) *printer.Ticket) (PrintOutcome, error) {
	out := newOutcome()
	r, err := loadRouting(ctx, db)
	if err != nil {
		return out, err
	}
	targets := r.receipts()
	if len(targets) == 0 {
		return out, errNoReceiptPrinter
	}
	for _, p := range targets {
		out.record(p, sendDoc(ctx, p, doc))
	}
	return out, nil
}

// printReceipt imprime el ticket de una cuenta pagada.
func printReceipt(ctx context.Context, db queryer, billID int64) (PrintOutcome, error) {
	t, err := loadTicket(ctx, db, billID)
	if err != nil {
		return newOutcome(), err
	}
	f, err := loadReceiptFormat(ctx, db)
	if err != nil {
		return newOutcome(), err
	}
	logo := loadLogo(ctx, db)
	return printToReceiptPrinters(ctx, db, func(width int) *printer.Ticket { return receiptDoc(t, width, f, logo) })
}

// printCashReport imprime el corte de un turno cerrado.
func printCashReport(ctx context.Context, db queryer, rep *CashReport) (PrintOutcome, error) {
	return printToReceiptPrinters(ctx, db, func(width int) *printer.Ticket { return cashReportDoc(rep, width) })
}

// openDrawer abre el cajón en las impresoras que lo tienen conectado.
func openDrawer(ctx context.Context, db queryer) (PrintOutcome, error) {
	out := newOutcome()
	r, err := loadRouting(ctx, db)
	if err != nil {
		return out, err
	}
	targets := r.drawers()
	if len(targets) == 0 {
		return out, errNoDrawer
	}
	for _, p := range targets {
		out.record(p, printer.OpenDrawer(ctx, targetOf(p)))
	}
	return out, nil
}

// printComandas manda a cada impresora los productos de las áreas de
// producción que tiene asignadas, una comanda por área con su formato.
// Normalmente imprime solo lo pendiente (printed_quantity < quantity) y lo
// marca como impreso; con reprint=true vuelve a imprimir la orden completa
// sin tocar printed_quantity.
//
// Un producto sin área, o cuya área no tiene impresora, se marca como
// enviado: no hay dónde imprimirlo y no debe quedarse pendiente para siempre.
// Si falla la impresora, sus productos quedan pendientes para reintentar.
func printComandas(ctx context.Context, db *sql.DB, orderID int64, reprint bool) (PrintOutcome, error) {
	out := newOutcome()
	var h comandaHeader
	err := db.QueryRowContext(ctx, `
		SELECT o.id, o.order_type, COALESCE(t.name, ''), COALESCE(o.customer_name, ''), COALESCE(o.notes, '')
		FROM orders o
		LEFT JOIN table_sessions ts ON ts.id = o.session_id
		LEFT JOIN dining_tables t ON t.id = ts.table_id
		WHERE o.id = ?`, orderID).Scan(&h.OrderID, &h.OrderType, &h.TableName, &h.Customer, &h.Notes)
	if err != nil {
		return out, err
	}

	pendingFilter := `AND oi.printed_quantity < oi.quantity`
	if reprint {
		pendingFilter = ``
	}
	var lines []comandaLine
	if err := eachRow(ctx, db, `
		SELECT oi.id, COALESCE(p.area_id, 0), p.name, oi.quantity - oi.printed_quantity, oi.quantity,
		       COALESCE(oi.modifiers_text, ''), COALESCE(oi.notes, ''), COALESCE(g.position, 0), COALESCE(g.name, '')
		FROM order_items oi JOIN products p ON p.id = oi.product_id
		LEFT JOIN order_guests g ON g.id = oi.guest_id
		WHERE oi.order_id = ? AND oi.status != 'VOID' `+pendingFilter+`
		-- Por persona (la mesa al final) y en el orden en que se pidió.
		ORDER BY CASE WHEN g.position IS NULL THEN 1 ELSE 0 END, g.position, oi.id`, []any{orderID}, func(scan func(...any) error) error {
		var l comandaLine
		var pending, total int
		var guestName string
		if err := scan(&l.ItemID, &l.AreaID, &l.Name, &pending, &total, &l.Modifiers, &l.Notes, &l.GuestPos, &guestName); err != nil {
			return err
		}
		l.Guest = guestLabel(l.GuestPos, guestName)
		l.Quantity = pending
		if reprint {
			l.Quantity = total
		}
		lines = append(lines, l)
		return nil
	}); err != nil {
		return out, err
	}
	if len(lines) == 0 {
		return out, nil
	}

	kind := comandaReprint
	if !reprint {
		// Si algo de la orden ya se había impreso, esto es un adicional.
		var alreadyPrinted int
		if err := db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM order_items
			WHERE order_id = ? AND status != 'VOID' AND printed_quantity > 0`, orderID).Scan(&alreadyPrinted); err != nil {
			return out, err
		}
		kind = comandaNew
		if alreadyPrinted > 0 {
			kind = comandaAdditional
		}
	}

	routing, err := loadRouting(ctx, db)
	if err != nil {
		return out, err
	}

	// Formato de cada área que aparece en la orden.
	areaNames := map[int64]string{}
	formats := map[int64]ComandaFormat{}
	hasGuests := false
	for _, l := range lines {
		hasGuests = hasGuests || l.GuestPos > 0
		if _, ok := formats[l.AreaID]; ok || l.AreaID == 0 {
			continue
		}
		var name string
		db.QueryRowContext(ctx, `SELECT name FROM production_areas WHERE id = ?`, l.AreaID).Scan(&name)
		areaNames[l.AreaID] = name
		if formats[l.AreaID], err = loadComandaFormat(ctx, db, l.AreaID); err != nil {
			return out, err
		}
	}

	// Una comanda por impresora y área, en el orden de la orden; si el área
	// pide una comanda por persona, también por persona.
	type job struct {
		p     *PrinterView
		area  int64
		guest string
		lines []comandaLine
	}
	var jobs []*job
	index := map[[3]int64]*job{}
	for _, l := range lines {
		for _, p := range routing.forArea(l.AreaID) {
			k := [3]int64{p.ID, l.AreaID, 0}
			guest := ""
			if hasGuests && formats[l.AreaID].GuestTickets {
				k[2], guest = int64(l.GuestPos), l.Guest
			}
			if index[k] == nil {
				index[k] = &job{p: p, area: l.AreaID, guest: guest}
				jobs = append(jobs, index[k])
			}
			index[k].lines = append(index[k].lines, l)
		}
	}

	failedItems := map[int64]bool{}
	for _, j := range jobs {
		hdr := h
		hdr.Guest = j.guest
		err := sendDoc(ctx, j.p, func(width int) *printer.Ticket {
			return comandaDoc(hdr, j.lines, kind, areaNames[j.area], width, formats[j.area])
		})
		out.record(j.p, err)
		if err != nil {
			for _, l := range j.lines {
				failedItems[l.ItemID] = true
			}
		}
	}

	if !reprint {
		// Lo que salió en todas sus impresoras (o no tenía impresora) queda como enviado.
		for _, l := range lines {
			if failedItems[l.ItemID] {
				continue
			}
			if _, err := db.ExecContext(ctx,
				`UPDATE order_items SET printed_quantity = quantity WHERE id = ?`, l.ItemID); err != nil {
				return out, err
			}
		}
	}
	return out, nil
}

// printNewComandas imprime lo pendiente de una orden recién registrada. Los
// errores de impresión no deshacen la orden: se reportan para reintentar
// desde Expo.
func (h *POSHandler) printNewComandas(ctx context.Context, orderID int64) PrintOutcome {
	out, err := printComandas(ctx, h.DB, orderID, false)
	if err != nil {
		log.Printf("Error imprimiendo comandas de la orden %d: %v", orderID, err)
		out.Failed = append(out.Failed, "No se pudo preparar la comanda: "+err.Error())
	}
	return out
}

// --- Handlers ---

// POST /api/orders/{id}/reprint - Reimprime la comanda completa (desde Expo)
func (h *POSHandler) ReprintComanda(w http.ResponseWriter, r *http.Request) {
	orderID, ok := pathID(r)
	if !ok {
		http.Error(w, "ID de orden inválido", http.StatusBadRequest)
		return
	}
	out, err := printComandas(r.Context(), h.DB, orderID, true)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "Orden no encontrada", http.StatusNotFound)
		return
	}
	if err != nil {
		serverError(w, "Error reimprimiendo la comanda", err)
		return
	}
	if !out.attempted() {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("Ninguna impresora tiene asignadas las áreas de producción de esta orden."))
		return
	}
	out.respond(w, "Comanda reimpresa en: ")
}

// POST /api/bills/{id}/print - Imprime el ticket de una cuenta.
func (h *POSHandler) PrintBill(w http.ResponseWriter, r *http.Request) {
	billID, ok := pathID(r)
	if !ok {
		http.Error(w, "ID de cuenta inválido", http.StatusBadRequest)
		return
	}
	out, err := printReceipt(r.Context(), h.DB, billID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		http.Error(w, "Cuenta no encontrada", http.StatusNotFound)
		return
	case errors.Is(err, errNoReceiptPrinter):
		http.Error(w, err.Error(), http.StatusConflict)
		return
	case err != nil:
		serverError(w, "Error imprimiendo el ticket", err)
		return
	}
	if len(out.Printed) == 0 {
		http.Error(w, "No se pudo imprimir en "+strings.Join(out.Failed, "; "), http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"printer": strings.Join(out.Printed, ", "), "failed": out.Failed})
}
