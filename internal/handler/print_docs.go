package handler

// Documentos que imprime el POS. Cada función arma un *printer.Ticket (bytes
// ESC/POS más su vista previa) para un ancho de papel; a quién se manda y
// cómo se reporta lo decide printing.go.

import (
	"fmt"
	"image"
	"strings"
	"time"

	"github.com/calebpyn/open-pos/internal/printer"
)

// --- Comanda ---

type comandaHeader struct {
	OrderID   int64
	OrderType string
	TableName string
	Customer  string
	Notes     string
	Guest     string // comanda de una sola persona ("Mesa 5 · Juan")
}

type comandaLine struct {
	ItemID    int64
	AreaID    int64 // 0 = sin área de producción (no se imprime)
	Name      string
	Quantity  int // cantidad a imprimir
	Modifiers string
	Notes     string
	Before    string // corrección: cómo estaba antes ("1 x Latte + Entera")
	Removed   bool   // corrección: ya no se prepara
	GuestPos  int    // persona (0 = mesa)
	Guest     string // "Juan" / "Persona 2" / "Mesa"
}

// Tipos de comanda
const (
	comandaNew        = "COMANDA"
	comandaAdditional = "ADICIONAL"
	comandaReprint    = "REIMPRESIÓN"
)

// --- Ticket de compra ---

// receiptDoc arma el ticket de cobro en ESC/POS con el formato del
// editor de impresiones; replica web/templates/ticket.html.
func receiptDoc(t *TicketData, paperWidth int, f ReceiptFormat, logo image.Image) *printer.Ticket {
	tk := printer.NewTicket(paperWidth)
	m := formatMoney

	tk.Align(printer.AlignCenter)
	if f.ShowLogo && logo != nil {
		// El logo no pasa de 240 puntos de alto (unos 3 cm) aunque la imagen sea alta.
		paper := printer.DotsFor(paperWidth)
		dots := printer.FitWidth(logo, paper*f.LogoWidthPct/100, 240)
		tk.Image(printer.Dither(logo, dots), dots*100/paper).Feed(1)
	}
	if f.ShowName {
		w, h := sizeOf(f.NameSize)
		tk.Bold(true).Size(w, h).Line(t.BusinessName).Size(1, 1).Bold(false)
	}
	for _, l := range nonEmptyLines(f.HeaderLines) {
		tk.Line(l)
	}
	if f.ShowDate && !t.PaidAt.IsZero() {
		tk.Line(t.PaidAt.Local().Format("02/01/2006 15:04"))
	}
	if t.PreBill {
		tk.Feed(1).Bold(true).Size(2, 1).Line("PRE-CUENTA").Size(1, 1).Bold(false)
	}
	tk.Align(printer.AlignLeft)
	if f.ShowOrderInfo {
		right := fmt.Sprintf("Cuenta %d", t.BillNumber)
		if t.PreBill {
			right = ""
		}
		tk.Separator().Columns(fmt.Sprintf("Orden #%d", t.OrderID), right)
		if t.TableName != "" {
			tk.Columns("Mesa", t.TableName)
		}
		if t.GuestLabel != "" {
			tk.Columns("Cuenta de", t.GuestLabel)
		}
		if t.CustomerName != "" {
			tk.Columns("Cliente", t.CustomerName)
		}
	}

	tk.Separator()
	iw, ih := sizeOf(f.ItemSize)
	for _, it := range t.Items {
		tk.Size(iw, ih).Columns(fmt.Sprintf("%d %s", it.Quantity, it.Name), m(it.Amount)).Size(1, 1)
		if f.ShowItemNotes {
			for _, mod := range splitModifiers(it.Modifiers) {
				tk.Line("  + " + mod)
			}
			if it.Notes != "" {
				tk.Line("  " + it.Notes)
			}
		}
	}

	tw, th := sizeOf(f.TotalSize)
	total := func(label string, v float64) {
		tk.Bold(true).Size(tw, th).Columns(label, m(v)).Size(1, 1).Bold(false)
	}
	tk.Separator().Columns("Subtotal", m(t.Subtotal))
	if t.Discount > 0 {
		label := "Descuento"
		if t.DiscountType == "PERCENT" {
			label = fmt.Sprintf("Descuento (%g%%)", t.DiscountValue)
		}
		tk.Columns(label, "-"+m(t.Discount))
		if t.DiscountReason != "" {
			tk.Line("  " + t.DiscountReason)
		}
	}
	if t.PricesIncludeTax {
		total("TOTAL", t.Total)
		if f.ShowTax {
			tk.Columns("IVA incluido", m(t.Tax))
		}
	} else {
		// Si el IVA se agrega al precio, siempre se desglosa.
		tk.Columns("IVA", m(t.Tax))
		total("TOTAL", t.Total)
	}
	if t.Tip > 0 {
		tk.Columns("Propina", m(t.Tip))
		total("TOTAL PAGADO", t.Grand)
	}
	if t.PreBill {
		// Propina sugerida sobre el total, redondeada al peso.
		tk.Separator().Line("Propina sugerida:")
		for _, pct := range []int64{10, 15} {
			tip := (toCents(t.Total)*pct/100 + 50) / 100 * 100
			tk.Columns(fmt.Sprintf("  %d%%  %s", pct, m(fromCents(tip))), "total "+m(fromCents(toCents(t.Total)+tip)))
		}
		tk.Feed(1).Align(printer.AlignCenter).Line("Este documento no es").Line("un comprobante de pago.").Align(printer.AlignLeft)
	}

	if f.ShowPayments && len(t.Payments) > 0 {
		tk.Separator()
		for _, p := range t.Payments {
			label := p.Method
			if p.Reference != "" {
				label += " (" + p.Reference + ")"
			}
			tk.Columns(label, m(p.Amount))
			if p.Received > p.Amount {
				tk.Columns("  Recibido", m(p.Received))
			}
		}
		if t.Change > 0 {
			tk.Bold(true).Columns("Cambio", m(t.Change)).Bold(false)
		}
	}

	if footer := nonEmptyLines(f.FooterLines); len(footer) > 0 {
		tk.Separator().Align(printer.AlignCenter)
		for _, l := range footer {
			tk.Line(l)
		}
	}
	return tk.Feed(f.FeedLines).Cut()
}

func nonEmptyLines(text string) []string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// comandaDoc arma la comanda de un área con su formato. Cada copia sale
// completa y con su propio corte.
func comandaDoc(h comandaHeader, lines []comandaLine, kind, areaName string, paperWidth int, f ComandaFormat) *printer.Ticket {
	tk := printer.NewTicket(paperWidth)
	var where string
	switch {
	case h.TableName != "":
		where = h.TableName
	case h.OrderType == "FLASH":
		where = "Mostrador"
	default:
		where = "Para llevar"
	}
	if h.Customer != "" && h.TableName == "" {
		where += ": " + h.Customer
	}
	if h.Guest != "" {
		where += " · " + h.Guest
	}
	// Con personas, la comanda va en secciones "— Juan —" (salvo que sea la
	// comanda de una sola persona).
	byGuest := false
	if h.Guest == "" {
		for _, l := range lines {
			byGuest = byGuest || l.GuestPos > 0
		}
	}

	for c := 0; c < f.Copies; c++ {
		w, hh := sizeOf(f.TitleSize)
		tk.Align(printer.AlignCenter).Bold(true).Size(w, hh).Line(kind).Size(1, 1)
		if f.ShowAreaName && areaName != "" {
			tk.Line(strings.ToUpper(areaName))
		}
		if f.Copies > 1 {
			tk.Line(fmt.Sprintf("Copia %d de %d", c+1, f.Copies))
		}
		tk.Bold(false).Align(printer.AlignLeft).Separator()

		w, hh = sizeOf(f.LocationSize)
		tk.Bold(true).Size(w, hh).Line(where).Size(1, 1).Bold(false)
		if f.ShowOrderInfo {
			tk.Columns(fmt.Sprintf("Orden #%d", h.OrderID), time.Now().Format("15:04"))
		}
		tk.Separator()

		iw, ih := sizeOf(f.ItemSize)
		mw, mh := sizeOf(f.ModifierSize)
		nw, nh := sizeOf(f.NoteSize)
		lastGuest := -1
		for _, l := range lines {
			if byGuest && l.GuestPos != lastGuest {
				tk.Align(printer.AlignCenter).Bold(true).Line("-- " + l.Guest + " --").Bold(false).Align(printer.AlignLeft)
				lastGuest = l.GuestPos
			}
			if l.Removed {
				tk.Bold(true).Line(fmt.Sprintf("YA NO VA: %d x %s", l.Quantity, l.Name)).Bold(false)
				if l.Modifiers != "" {
					tk.Line("   (" + strings.ReplaceAll(l.Modifiers, modifierSep, ", ") + ")")
				}
				continue
			}
			tk.Bold(true).Size(iw, ih).Line(fmt.Sprintf("%d x %s", l.Quantity, l.Name)).Size(1, 1).Bold(false)
			for _, m := range splitModifiers(l.Modifiers) {
				tk.Bold(true).Size(mw, mh).Line("   + "+m).Size(1, 1).Bold(false)
			}
			if l.Notes != "" {
				tk.Size(nw, nh).Line("   > "+l.Notes).Size(1, 1)
			}
			if l.Before != "" {
				tk.Line("   (cambio: antes " + l.Before + ")")
			}
		}

		if h.Notes != "" {
			ow, oh := sizeOf(f.OrderNoteSize)
			tk.Separator().Bold(true).Size(ow, oh).Line("NOTA: "+h.Notes).Size(1, 1).Bold(false)
		}
		tk.Feed(f.FeedLines).Cut()
	}
	return tk
}

// splitModifiers separa el texto guardado de los modificadores de una línea.
func splitModifiers(text string) []string {
	if text == "" {
		return nil
	}
	return strings.Split(text, modifierSep)
}

// cashReportDoc arma el corte Z de un turno cerrado.
func cashReportDoc(rep *CashReport, paperWidth int) *printer.Ticket {
	s := rep.Session
	m := formatMoney
	tk := printer.NewTicket(paperWidth)
	tk.Align(printer.AlignCenter).Bold(true).Size(2, 2).Line("CORTE DE CAJA").Size(1, 1).
		Line(rep.BusinessName).Bold(false).
		Line(fmt.Sprintf("Turno #%d", s.ID)).Align(printer.AlignLeft).Separator().
		Columns("Abrió", s.OpenedBy+" "+s.OpenedAt.Local().Format("02/01 15:04"))
	if s.ClosedAt != nil {
		tk.Columns("Cerró", s.ClosedBy+" "+s.ClosedAt.Local().Format("02/01 15:04"))
	}

	tk.Separator().Bold(true).Line("VENTAS").Bold(false).
		Columns("Cuentas cobradas", fmt.Sprint(rep.Bills)).
		Columns("Subtotal", m(rep.Subtotal)).
		Columns("Descuentos", "-"+m(rep.Discounts)).
		Columns("Ventas", m(rep.Sales)).
		Columns("IVA", m(rep.Tax)).
		Columns("Propinas", m(rep.Tips)).
		Bold(true).Columns("TOTAL COBRADO", m(rep.Collected)).Bold(false).
		Columns("Ticket promedio", m(rep.AvgTicket))

	tk.Separator().Bold(true).Line("POR MÉTODO DE PAGO").Bold(false)
	for _, mt := range rep.Methods {
		tk.Columns(fmt.Sprintf("%s (%d)", mt.Label, mt.Count), m(mt.Amount))
		if mt.Tips > 0 {
			tk.Columns("  de propina", m(mt.Tips))
		}
	}
	if len(rep.Payroll) > 0 {
		tk.Line("Cargos a nómina (no entran a caja):")
		for _, p := range rep.Payroll {
			tk.Columns(fmt.Sprintf("  %s (%d)", p.Name, p.Count), m(p.Amount))
		}
	}

	tk.Separator().Bold(true).Line("EFECTIVO").Bold(false).
		Columns("Fondo inicial", m(s.OpeningFloat)).
		Columns("Cobros en efectivo", m(rep.CashSales))
	for _, mv := range rep.Movements {
		sign := "-"
		if mv.In {
			sign = "+"
		}
		tk.Columns(mv.Label, sign+m(mv.Amount)).Line("  " + mv.Reason)
	}

	tk.Separator().Bold(true).Line("ARQUEO").Bold(false)
	for _, a := range rep.Arqueo() {
		tk.Line(a.Label).
			Columns("  Sistema", m(a.Expected)).
			Columns("  Reportado", m(a.Reported)).
			Bold(true).Columns("  Diferencia", signedMoney(a.Diff())).Bold(false)
	}

	if len(rep.Cancellations)+len(rep.Voids)+len(rep.Discounted)+len(rep.DrawerOpens) > 0 {
		tk.Separator().Bold(true).Line("CONTROL").Bold(false)
		for _, e := range rep.Cancellations {
			tk.Columns("Sin pago: "+e.Title, m(e.Amount)).Line("  " + e.Reason)
		}
		for _, e := range rep.Voids {
			tk.Columns("Anulado: "+e.Title, m(e.Amount)).Line("  " + e.Reason)
		}
		for _, e := range rep.Discounted {
			tk.Columns("Desc.: "+e.Title, "-"+m(e.Amount))
			if e.Reason != "" {
				tk.Line("  " + e.Reason)
			}
		}
		if n := len(rep.DrawerOpens); n > 0 {
			tk.Columns("Aperturas manuales de cajón", fmt.Sprint(n))
		}
	}

	if s.CloseNotes != "" {
		tk.Separator().Line("Notas: " + s.CloseNotes)
	}
	tk.Feed(2).Align(printer.AlignCenter).Line("_______________________").Line("Firma")
	return tk.Feed(3).Cut()
}

// testDoc es el ticket de "Imprimir prueba" de Dispositivos: sirve para
// revisar conexión, ancho de papel y acentos.
func testDoc(business string, p *PrinterView) *printer.Ticket {
	t := printer.NewTicket(p.PaperWidth)
	conn := "USB (CUPS: " + p.SystemName + ")"
	if p.ConnectionType == printer.ConnNetwork {
		conn = "Red " + p.SystemName
	}
	t.Align(printer.AlignCenter).Bold(true).Size(2, 2).Line(business).Size(1, 1).Bold(false).
		Line("Prueba de impresión").Separator().
		Align(printer.AlignLeft).
		Columns("Impresora", p.Name).
		Line("Conexión: "+conn).
		Columns("Papel", fmt.Sprintf("%d mm / %d col", p.PaperWidth, t.Cols())).
		Columns("Fecha", time.Now().Format("02/01/2006 15:04")).
		Separator().
		Line("Acentos: áéíóú ÁÉÍÓÚ ñÑ ü ¿? ¡!").
		Columns("1x Latte 12oz", "$65.00").
		Columns("2x Chilaquiles Verdes con pollo", "$240.00").
		Bold(true).Columns("TOTAL", "$305.00").Bold(false).
		Separator().
		Align(printer.AlignCenter).Line("Si lees esto, la impresora").Line("está lista.").
		Feed(3).Cut()
	return t
}
