package handler

import (
	"database/sql"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"
)

type ExpoItem struct {
	ID           int64
	Name         string
	Quantity     int
	Modifiers    string
	Notes        string
	Sent         bool // salió en comanda
	Paid         bool
	DeliveredQty int  // piezas ya entregadas
	Delivered    bool // todas las piezas entregadas
	ElapsedMin   int  // esperando desde que se pidió; si ya se entregó, lo que tardó
	Extra        bool // se oculta mientras la tarjeta está compacta
	GuestPos     int
	Guest        string // cuentas por persona: de quién es
	GuestHeader  bool   // primer platillo de su persona: se muestra el nombre
}

func (i ExpoItem) Elapsed() string { return formatMinutes(i.ElapsedMin) }

// maxUnitBoxes es hasta cuántas piezas se dibujan como casillas; con más se
// muestra un contador ("3/8").
const maxUnitBoxes = 6

// Units regresa una casilla por pieza (true = entregada) para dibujarlas.
func (i ExpoItem) Units() []bool {
	if i.Quantity > maxUnitBoxes {
		return nil
	}
	out := make([]bool, i.Quantity)
	for k := 0; k < i.DeliveredQty && k < i.Quantity; k++ {
		out[k] = true
	}
	return out
}

// Partial indica que ya salió alguna pieza pero no todas.
func (i ExpoItem) Partial() bool { return i.DeliveredQty > 0 && !i.Delivered }

// Semáforo de espera por producto: la urgencia está en lo que falta entregar.
func (i ExpoItem) TimerClass() string {
	switch {
	case i.Delivered:
		return "bg-gray-100 text-gray-400"
	case i.ElapsedMin >= 18:
		return "bg-rose-100 text-rose-800 animate-pulse"
	case i.ElapsedMin >= 10:
		return "bg-amber-100 text-amber-800"
	default:
		return "bg-emerald-100 text-emerald-800"
	}
}

type ExpoOrder struct {
	Waiter        bool // terminal de mesero: sin cobrar ni cerrar sin pagar
	OrderID       int64
	OrderType     string // DINE_IN, TAKEAWAY, FLASH
	Status        string // ACTIVE, PAID
	Delivered     bool
	HasBills      bool
	TableName     string
	Customer      string
	Notes         string
	ElapsedMin    int // desde que se abrió la mesa (o la orden, si no tiene mesa)
	PendingAmount float64
	Items         []ExpoItem
}

func (o ExpoOrder) Title() string {
	switch {
	case o.TableName != "":
		return o.TableName
	case o.Customer != "":
		return o.Customer
	case o.OrderType == "FLASH":
		return "Venta Mostrador"
	default:
		return "Para Llevar"
	}
}

func (o ExpoOrder) Elapsed() string { return formatMinutes(o.ElapsedMin) }

func (o ExpoOrder) CanDeliver() bool { return o.OrderType != "DINE_IN" && !o.Delivered }

func (o ExpoOrder) count(match func(ExpoItem) bool) int {
	n := 0
	for _, it := range o.Items {
		if match(it) {
			n += it.Quantity
		}
	}
	return n
}

func (o ExpoOrder) TotalItems() int { return o.count(func(ExpoItem) bool { return true }) }
func (o ExpoOrder) DeliveredItems() int {
	n := 0
	for _, it := range o.Items {
		n += it.DeliveredQty
	}
	return n
}
func (o ExpoOrder) SentItems() int { return o.count(func(i ExpoItem) bool { return i.Sent }) }

func (o ExpoOrder) ProgressPct() int {
	if t := o.TotalItems(); t > 0 {
		return o.DeliveredItems() * 100 / t
	}
	return 0
}

// Border resalta la tarjeta cuando algún producto lleva demasiado esperando.
func (o ExpoOrder) Border() string {
	worst := 0
	for _, it := range o.Items {
		if !it.Delivered && it.ElapsedMin > worst {
			worst = it.ElapsedMin
		}
	}
	switch {
	case worst >= 18:
		return "border-rose-400 ring-2 ring-rose-200"
	case worst >= 10:
		return "border-amber-300"
	default:
		return "border-gray-200"
	}
}

// Una orden grande se muestra compacta: solo los primeros pendientes (los que
// llevan más esperando) y un botón para ver el resto.
const (
	expoCompactAfter = 4 // renglones que caben sin compactar
	expoCompactShow  = 3 // pendientes visibles en modo compacto
)

// markGuests pone el nombre de la persona sobre su primer platillo, solo si la
// orden se dividió por personas.
func (o *ExpoOrder) markGuests() {
	split := false
	for _, it := range o.Items {
		split = split || it.GuestPos > 0
	}
	last := -1
	for i := range o.Items {
		if split && o.Items[i].GuestPos != last {
			o.Items[i].GuestHeader = true
			last = o.Items[i].GuestPos
		}
	}
}

func (o *ExpoOrder) markExtras() {
	if len(o.Items) <= expoCompactAfter {
		return
	}
	shown := 0
	for i := range o.Items {
		it := &o.Items[i]
		if it.Delivered || shown >= expoCompactShow {
			it.Extra = true
			continue
		}
		shown++
	}
}

func (o ExpoOrder) HiddenLines() int {
	n := 0
	for _, it := range o.Items {
		if it.Extra {
			n++
		}
	}
	return n
}

func (o ExpoOrder) HiddenLabel() string {
	pending, delivered := 0, 0
	for _, it := range o.Items {
		switch {
		case it.Extra && it.Delivered:
			delivered++
		case it.Extra:
			pending++
		}
	}
	switch {
	case pending == 0:
		return fmt.Sprintf("Ver %d entregado(s)", delivered)
	case delivered == 0:
		return fmt.Sprintf("Ver %d pendiente(s) más", pending)
	default:
		return fmt.Sprintf("Ver %d más (%d entregado(s))", pending+delivered, delivered)
	}
}

func formatMinutes(m int) string {
	if m < 60 {
		return fmt.Sprintf("%d min", m)
	}
	return fmt.Sprintf("%d h %02d", m/60, m%60)
}

var expoTmpl = template.Must(template.New("expo").Funcs(pageFuncs).Parse(`
{{- if not . -}}
<div class="h-full flex flex-col items-center justify-center text-gray-400 py-20">
	<span class="text-4xl mb-2">☕</span>
	<p class="text-sm font-medium">No hay órdenes activas en este canal</p>
</div>
{{- else -}}
<div class="expo-cards flex gap-4 items-start pr-1 pb-4">
{{- range . }}
	<div class="expo-card bg-white rounded-2xl shadow-sm border {{.Border}} flex flex-col" data-order-id="{{.OrderID}}">
		<div class="p-4 pb-3 border-b border-gray-100">
			<div class="flex justify-between items-start gap-2">
				<div class="min-w-0">
					<span class="text-[10px] uppercase font-mono font-bold text-gray-400 tracking-wider">ORDEN #{{.OrderID}}{{if eq .OrderType "FLASH"}} · FLASH{{end}}</span>
					<h4 class="text-xl font-black text-gray-900 leading-tight truncate">{{.Title}}</h4>
				</div>
				<span title="Tiempo desde que se abrió {{if .TableName}}la mesa{{else}}la orden{{end}}"
				      class="flex items-center gap-1 px-2.5 py-1 rounded-full bg-slate-100 text-slate-700 text-xs font-mono font-bold whitespace-nowrap">
					🕐 {{.Elapsed}}
				</span>
			</div>
			<div class="flex justify-between items-center mt-2 text-xs">
				<span class="font-semibold text-gray-500">{{.DeliveredItems}}/{{.TotalItems}} entregados</span>
				{{- if eq .Status "PAID" }}
				<span class="font-bold text-emerald-700">✔ Pagada</span>
				{{- else }}
				<span class="font-mono font-bold text-gray-800">Por cobrar: {{money .PendingAmount}}</span>
				{{- end }}
			</div>
			<div class="mt-2 h-1.5 bg-gray-100 rounded-full overflow-hidden">
				<div class="h-full bg-emerald-500 transition-all" style="width: {{.ProgressPct}}%"></div>
			</div>
		</div>

		{{- if .Notes }}
		<p class="mx-3 mt-2 px-2.5 py-1.5 rounded-lg bg-amber-50 border border-amber-200 text-xs text-amber-900">📝 {{.Notes}}</p>
		{{- end }}

		<ul class="p-2 space-y-1"
		    hx-on::after-request="if (!event.detail.successful) toast(event.detail.xhr.responseText || 'No se pudo conectar con el servidor', 'error')">
		{{- range .Items }}
			{{- if .GuestHeader }}
			<li{{if .Extra}} data-extra{{end}} class="px-2 pt-1.5 text-[11px] font-black uppercase tracking-wider text-sky-800">{{.Guest}}</li>
			{{- end }}
			<li{{if .Extra}} data-extra{{end}} class="flex items-stretch gap-1">
				<button hx-post="/api/order-items/{{.ID}}/{{if .Delivered}}undeliver{{else}}deliver{{end}}" hx-swap="none"
				        title="{{if .Delivered}}Tocar para regresar una pieza a pendiente{{else if gt .Quantity 1}}Tocar para marcar una pieza como entregada{{else}}Tocar para marcar como entregado{{end}}"
				        class="flex-1 min-w-0 flex items-start gap-2.5 p-2 rounded-xl text-left transition {{if .Delivered}}bg-gray-50{{else}}hover:bg-emerald-50 active:bg-emerald-100{{end}}">
					{{- if .Units }}
					<span class="mt-0.5 shrink-0 flex flex-wrap gap-0.5 {{if gt .Quantity 3}}w-[62px]{{end}}">
						{{- range .Units }}
						<span class="w-5 h-5 rounded-md border-2 flex items-center justify-center text-[11px] font-black {{if .}}bg-emerald-500 border-emerald-500 text-white{{else}}border-gray-300{{end}}">{{if .}}✓{{end}}</span>
						{{- end }}
					</span>
					{{- else }}
					<span class="mt-0.5 shrink-0 px-1.5 h-5 rounded-md border-2 flex items-center text-[11px] font-black font-mono {{if .Delivered}}bg-emerald-500 border-emerald-500 text-white{{else if .Partial}}border-emerald-500 text-emerald-700{{else}}border-gray-300 text-gray-500{{end}}">{{.DeliveredQty}}/{{.Quantity}}</span>
					{{- end }}
					<span class="flex-1 min-w-0">
						<span class="block text-sm font-bold {{if .Delivered}}line-through text-gray-400{{else}}text-gray-800{{end}}">{{.Quantity}}× {{.Name}}</span>
						{{- if .Partial }}<span class="block text-[11px] font-bold text-emerald-700">{{.DeliveredQty}} de {{.Quantity}} entregado(s)</span>{{ end }}
						{{- if .Modifiers }}<span class="block text-xs font-semibold {{if .Delivered}}text-gray-300{{else}}text-sky-800{{end}}">+ {{.Modifiers}}</span>{{ end }}
						{{- if .Notes }}<span class="block text-xs italic {{if .Delivered}}text-gray-300{{else}}text-amber-700{{end}}">{{.Notes}}</span>{{ end }}
						{{- if or (not .Sent) .Paid }}
						<span class="flex gap-1 mt-0.5">
							{{- if not .Sent }}<span class="text-[10px] font-bold uppercase px-1.5 rounded bg-rose-100 text-rose-700">Sin comanda</span>{{ end }}
							{{- if .Paid }}<span class="text-[10px] font-bold uppercase px-1.5 rounded bg-emerald-100 text-emerald-700">Pagado</span>{{ end }}
						</span>
						{{- end }}
					</span>
					<span class="shrink-0 px-2 py-0.5 rounded-full text-[11px] font-mono font-bold {{.TimerClass}}">{{if .Delivered}}✓ {{end}}{{.Elapsed}}</span>
				</button>
				{{- if .Partial }}
				<button hx-post="/api/order-items/{{.ID}}/undeliver" hx-swap="none" title="Deshacer la última pieza marcada"
				        class="shrink-0 px-2 rounded-xl text-sm text-gray-400 hover:text-gray-700 hover:bg-gray-100">↶</button>
				{{- end }}
			</li>
		{{- end }}
		</ul>
		{{- if .HiddenLines }}
		<button onclick="toggleExpoCard(this)"
		        class="mx-2 mb-2 py-1.5 rounded-lg text-xs font-bold text-slate-600 bg-slate-50 hover:bg-slate-100 transition">
			<span class="when-collapsed">{{.HiddenLabel}} ▾</span>
			<span class="when-expanded">Ver menos ▴</span>
		</button>
		{{- end }}

		<div class="p-3 pt-2 mt-auto border-t border-gray-100">
			<div class="flex gap-2">
				<button hx-post="/api/orders/{{.OrderID}}/reprint" hx-swap="none"
				        hx-on::after-request="toast(event.detail.xhr.responseText || 'No se pudo conectar con el servidor', event.detail.successful ? 'ok' : 'error')"
				        title="Reimprimir la comanda completa"
				        class="{{if .CanDeliver}}px-3{{else}}flex-1{{end}} bg-slate-100 hover:bg-slate-200 text-slate-800 py-2 rounded-xl text-xs font-bold transition">
					🖨️{{if not .CanDeliver}} Re-Imprimir{{end}}
				</button>
				{{- if .CanDeliver }}
				<button hx-post="/api/orders/{{.OrderID}}/deliver" hx-swap="none"
				        title="Marcar todos los productos como entregados"
				        class="flex-1 bg-sky-100 hover:bg-sky-200 text-sky-900 py-2 rounded-xl text-xs font-bold transition">
					✅ Entregar todo
				</button>
				{{- end }}
				{{- if and (eq .Status "ACTIVE") (not .Waiter) }}
				<a href="/cobrar/{{.OrderID}}?volver=expo"
				   class="flex-1 text-center bg-emerald-600 hover:bg-emerald-700 text-white py-2 rounded-xl text-xs font-bold transition">
					💳 Cobrar
				</a>
				{{- end }}
			</div>
			{{- if and (eq .Status "ACTIVE") (not .Waiter) }}
			<button onclick="openCloseOrder(this)"
			        data-order-id="{{.OrderID}}" data-title="{{.Title}}"
			        data-sent="{{.SentItems}}" data-delivered="{{.DeliveredItems}}"
			        data-unpaid="{{money .PendingAmount}}" data-has-bills="{{.HasBills}}"
			        class="w-full mt-2 py-1 text-[11px] font-semibold text-rose-600 hover:text-rose-800 hover:underline">
				🚫 Cerrar sin pagar
			</button>
			{{- end }}
		</div>
	</div>
{{- end }}
</div>
{{- end }}
`))

// expoPaidMaxAge es cuánto sigue en Expo una orden de mostrador pagada que
// nadie marcó como entregada.
const expoPaidMaxAge = 6 * time.Hour

// GET /api/expo/html?type=DINE_IN|TAKEAWAY
func (h *UIHandler) GetExpoHTML(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	filter := `o.status = 'ACTIVE' AND o.order_type = 'DINE_IN'`
	if r.URL.Query().Get("type") == "TAKEAWAY" {
		// Para llevar / mostrador: se quedan hasta estar pagadas Y entregadas.
		// Una pagada que nadie marcó como entregada deja de mostrarse a las
		// expoPaidMaxAge: si no, las olvidadas se acumularían para siempre.
		filter = `o.order_type IN ('TAKEAWAY', 'FLASH')
			AND (o.status = 'ACTIVE' OR (o.status = 'PAID' AND o.delivered_at IS NULL
			                             AND o.closed_at >= datetime('now', '` + sqliteAgo(expoPaidMaxAge) + `')))`
	}

	rows, err := h.POS.DB.QueryContext(ctx, `
		SELECT
			o.id, o.order_type, o.status, o.delivered_at IS NOT NULL,
			EXISTS (SELECT 1 FROM bills b WHERE b.order_id = o.id),
			COALESCE(t.name, ''), COALESCE(o.customer_name, ''), COALESCE(o.notes, ''), ts.opened_at, o.created_at,
			COALESCE(SUM(CASE WHEN oi.status != 'VOID' AND oi.bill_id IS NULL THEN oi.unit_price * oi.quantity END), 0)
		FROM orders o
		LEFT JOIN table_sessions ts ON o.session_id = ts.id
		LEFT JOIN dining_tables t ON ts.table_id = t.id
		LEFT JOIN order_items oi ON o.id = oi.order_id
		WHERE `+filter+`
		GROUP BY o.id
		ORDER BY o.created_at ASC`)
	if err != nil {
		serverError(w, "Error cargando órdenes de expo", err)
		return
	}
	defer rows.Close()

	var orders []ExpoOrder
	byID := map[int64]int{}
	now := time.Now()
	for rows.Next() {
		var ord ExpoOrder
		// El driver convierte columnas DATETIME a time.Time (en UTC); se leen
		// por separado porque COALESCE haría perder el tipo.
		var openedAt sql.NullTime
		var createdAt time.Time
		if err := rows.Scan(&ord.OrderID, &ord.OrderType, &ord.Status, &ord.Delivered, &ord.HasBills,
			&ord.TableName, &ord.Customer, &ord.Notes, &openedAt, &createdAt, &ord.PendingAmount); err != nil {
			serverError(w, "Error leyendo órdenes de expo", err)
			return
		}
		if !openedAt.Valid {
			openedAt.Time = createdAt
		}
		ord.ElapsedMin = minutesBetween(openedAt.Time, now)
		byID[ord.OrderID] = len(orders)
		orders = append(orders, ord)
	}
	if err := rows.Err(); err != nil {
		serverError(w, "Error leyendo órdenes de expo", err)
		return
	}
	rows.Close()

	if len(orders) > 0 {
		ids := make([]any, 0, len(orders))
		for _, o := range orders {
			ids = append(ids, o.OrderID)
		}
		// Los productos se listan en el orden en que se pidieron para que no
		// cambien de lugar al tacharlos; con cuentas por persona, agrupados
		// por persona (la mesa al final).
		iRows, err := h.POS.DB.QueryContext(ctx, `
			SELECT oi.id, oi.order_id, p.name, oi.quantity, COALESCE(oi.modifiers_text, ''), COALESCE(oi.notes, ''),
			       oi.printed_quantity > 0, oi.bill_id IS NOT NULL, oi.created_at, oi.delivered_at, oi.delivered_quantity,
			       COALESCE(g.position, 0), COALESCE(g.name, '')
			FROM order_items oi JOIN products p ON p.id = oi.product_id
			LEFT JOIN order_guests g ON g.id = oi.guest_id
			WHERE oi.status != 'VOID' AND oi.order_id IN (?`+strings.Repeat(",?", len(ids)-1)+`)
			ORDER BY oi.order_id, CASE WHEN g.position IS NULL THEN 1 ELSE 0 END, g.position, oi.id`, ids...)
		if err != nil {
			serverError(w, "Error cargando productos de expo", err)
			return
		}
		defer iRows.Close()
		for iRows.Next() {
			var it ExpoItem
			var orderID int64
			var createdAt time.Time
			var deliveredAt sql.NullTime
			var guestName string
			if err := iRows.Scan(&it.ID, &orderID, &it.Name, &it.Quantity, &it.Modifiers, &it.Notes,
				&it.Sent, &it.Paid, &createdAt, &deliveredAt, &it.DeliveredQty, &it.GuestPos, &guestName); err != nil {
				serverError(w, "Error leyendo productos de expo", err)
				return
			}
			// El timer sigue corriendo hasta que sale la última pieza.
			end := now
			if it.DeliveredQty >= it.Quantity {
				it.Delivered = true
				if deliveredAt.Valid {
					end = deliveredAt.Time
				}
			}
			it.ElapsedMin = minutesBetween(createdAt, end)
			it.Guest = guestLabel(it.GuestPos, guestName)
			o := &orders[byID[orderID]]
			o.Items = append(o.Items, it)
		}
		if err := iRows.Err(); err != nil {
			serverError(w, "Error leyendo productos de expo", err)
			return
		}
		waiter := isWaiterTerminal(ctx)
		for i := range orders {
			orders[i].Waiter = waiter
			orders[i].markGuests()
			orders[i].markExtras()
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := expoTmpl.Execute(w, orders); err != nil {
		serverError(w, "Error renderizando expo", err)
	}
}

func minutesBetween(from, to time.Time) int {
	if m := int(to.Sub(from).Minutes()); m > 0 {
		return m
	}
	return 0
}
