package handler

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// RegisterComprasRoutes agrega las pantallas de Compras y su administración.
func (h *POSHandler) RegisterComprasRoutes(mux *http.ServeMux) {
	handle := mux.HandleFunc
	handle("GET /compras", h.comprasPage(nil, h.ComprasRequestPage))
	handle("POST /compras/pedir/{id}", h.comprasPage(nil, h.ComprasRequest))
	handle("POST /compras/pedir/{id}/quitar", h.comprasPage(nil, h.ComprasWithdraw))
	shop := func(u comprasUser) bool { return u.CanShop }
	handle("GET /compras/comprar", h.comprasPage(shop, h.ComprasShopPage))
	handle("POST /compras/comprar/{id}/{action}", h.comprasPage(shop, h.ComprasShopAction))
	open := func(u comprasUser) bool { return u.CanOpen }
	handle("GET /compras/lista", h.comprasPage(open, h.ComprasListPage))
	handle("POST /compras/lista/{action}", h.comprasPage(open, h.ComprasListAction))
	handle("GET /compras/lista/texto", h.comprasPage(func(u comprasUser) bool { return u.CanShop || u.CanOpen }, h.ComprasText))
	handle("GET /compras/tickets", h.comprasPage(shop, h.ComprasTicketsPage))
	handle("POST /compras/tickets", h.comprasPage(shop, h.ComprasUploadTicket))
	handle("GET /compras/tickets/{id}", h.comprasPage(shop, h.ComprasTicketPage))
	handle("GET /compras/tickets/{id}/imagen", h.comprasPage(shop, h.ComprasTicketImage))
	handle("POST /compras/tickets/{id}/{action}", h.comprasPage(shop, h.ComprasTicketAction))

	admin := h.RequireAdmin
	handle("GET /admin/compras", admin(h.ComprasAdmin))
	handle("GET /admin/compras/insumos/nuevo", admin(h.ComprasSupplyForm))
	handle("GET /admin/compras/insumos/{id}", admin(h.ComprasSupplyForm))
	handle("POST /admin/compras/insumos", admin(h.ComprasSaveSupply))
	handle("POST /admin/compras/insumos/{id}", admin(h.ComprasSaveSupply))
	handle("POST /admin/compras/lugares", admin(h.ComprasSavePlace))
	handle("POST /admin/compras/lugares/{id}", admin(h.ComprasSavePlace))
	handle("POST /admin/compras/surten/{id}", admin(h.ComprasSetShopper))
	handle("POST /admin/compras/importar", admin(h.ComprasImport))
}

const ctxCompras ctxKey = 300

func comprasFrom(ctx context.Context) comprasUser {
	u, _ := ctx.Value(ctxCompras).(comprasUser)
	return u
}

// comprasPage identifica a quien usa Compras y revisa su permiso.
func (h *POSHandler) comprasPage(allowed func(comprasUser) bool, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, err := h.comprasWho(r.Context())
		if err != nil {
			serverError(w, "Error identificando al usuario", err)
			return
		}
		if allowed != nil && !allowed(u) {
			if r.Method == http.MethodGet {
				http.Redirect(w, r, "/compras", http.StatusSeeOther)
				return
			}
			http.Error(w, "No tienes permiso para esto.", http.StatusForbidden)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxCompras, u)))
	}
}

func comprasRender(w http.ResponseWriter, r *http.Request, file, tab string, data map[string]any) {
	data["Who"] = comprasFrom(r.Context())
	data["Tab"] = tab
	renderPage(w, data, "compras/"+file, "compras/layout.html")
}

// --- Pedir ---

type comprasRow struct {
	ID       int64
	Name     string
	Unit     string
	Category string
	Item     *comprasItem    // si ya está en la lista
	Mine     *comprasRequest // mi pedido, si lo hice
}

type comprasLocation struct {
	Name string
	Rows []*comprasRow
}

// comprasRequestData arma la hoja para pedir, agrupada por dónde se guarda
// cada insumo (como se recorre el local) o, con by == "categoria", por
// categoría.
func (h *POSHandler) comprasRequestData(ctx context.Context, u comprasUser, by string) (map[string]any, error) {
	list, err := comprasActive(ctx, h.DB)
	if err != nil {
		return nil, err
	}
	items := map[int64]*comprasItem{}
	var carried []*comprasItem
	if list != nil {
		loaded, err := comprasItems(ctx, h.DB, list.ID)
		if err != nil {
			return nil, err
		}
		for _, it := range loaded {
			items[it.SupplyID] = it
			if it.Carried && it.Status != "BOUGHT" {
				carried = append(carried, it)
			}
		}
	}
	var groups []*comprasLocation
	index := map[string]*comprasLocation{}
	if by != "categoria" {
		by = "ubicacion"
	}
	group := "location"
	if by == "categoria" {
		group = "category"
	}
	err = eachRow(ctx, h.DB, `SELECT id, name, unit, category, `+group+` FROM compras_supplies
		WHERE is_active = 1 ORDER BY `+group+` = '', lower(`+group+`), name_key`, nil, func(scan func(...any) error) error {
		c := &comprasRow{}
		var location string
		if err := scan(&c.ID, &c.Name, &c.Unit, &c.Category, &location); err != nil {
			return err
		}
		if it := items[c.ID]; it != nil {
			c.Item = it
			for i := range it.Requests {
				if it.Requests[i].UserID == u.UserID {
					c.Mine = &it.Requests[i]
				}
			}
		}
		if location == "" {
			location = "Otros"
		}
		g, ok := index[location]
		if !ok {
			g = &comprasLocation{Name: location}
			index[location] = g
			groups = append(groups, g)
		}
		g.Rows = append(g.Rows, c)
		return nil
	})
	return map[string]any{"List": list, "Groups": groups, "Carried": carried, "Count": len(items), "By": by}, err
}

// GET /compras - La hoja para anotar lo que falta.
func (h *POSHandler) ComprasRequestPage(w http.ResponseWriter, r *http.Request) {
	data, err := h.comprasRequestData(r.Context(), comprasFrom(r.Context()), r.URL.Query().Get("por"))
	if err != nil {
		serverError(w, "Error cargando la lista", err)
		return
	}
	comprasRender(w, r, "pedir.html", "pedir", data)
}

// comprasRow responde el renglón actualizado de un insumo (htmx).
func (h *POSHandler) comprasRowResponse(w http.ResponseWriter, r *http.Request, supplyID int64, errMsg string) {
	data, err := h.comprasRequestData(r.Context(), comprasFrom(r.Context()), "")
	if err != nil {
		serverError(w, "Error cargando la lista", err)
		return
	}
	for _, g := range data["Groups"].([]*comprasLocation) {
		for _, c := range g.Rows {
			if c.ID == supplyID {
				renderBlock(w, "compras_row", map[string]any{"C": c, "List": data["List"], "Error": errMsg},
					"compras/pedir.html", "compras/layout.html")
				return
			}
		}
	}
	if errMsg == "" {
		errMsg = "Ese insumo ya no existe."
	}
	http.Error(w, errMsg, http.StatusUnprocessableEntity)
}

func (h *POSHandler) comprasRowResult(w http.ResponseWriter, r *http.Request, id int64, err error) {
	var bad badRequest
	if errors.As(err, &bad) {
		h.comprasRowResponse(w, r, id, bad.Error())
		return
	}
	if err != nil {
		serverError(w, "Error guardando el pedido", err)
		return
	}
	h.comprasRowResponse(w, r, id, "")
}

// POST /compras/pedir/{id} - falta (urgent=0) o urge (urgent=1), con nota.
func (h *POSHandler) ComprasRequest(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	err := h.comprasRequestSupply(r.Context(), comprasFrom(r.Context()), id, r.FormValue("urgent") == "1", r.FormValue("note"))
	h.comprasRowResult(w, r, id, err)
}

// POST /compras/pedir/{id}/quitar
func (h *POSHandler) ComprasWithdraw(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	h.comprasRowResult(w, r, id, h.comprasWithdraw(r.Context(), comprasFrom(r.Context()), id))
}

// --- Comprar ---

func (h *POSHandler) comprasShopData(ctx context.Context) (map[string]any, error) {
	list, err := comprasActive(ctx, h.DB)
	if err != nil {
		return nil, err
	}
	data := map[string]any{"List": list}
	if list == nil {
		return data, nil
	}
	items, err := comprasItems(ctx, h.DB, list.ID)
	if err != nil {
		return nil, err
	}
	places, err := comprasLoadPlaces(ctx, h.DB, true)
	if err != nil {
		return nil, err
	}
	settings, err := loadSettings(ctx, h.DB)
	if err != nil {
		return nil, err
	}
	data["Places"] = places
	data["Groups"] = comprasByPlace(items)
	data["Totals"] = comprasTotalsOf(items)
	data["WhatsApp"] = whatsappLink(comprasText(settings.BusinessName, items))
	return data, nil
}

// GET /compras/comprar - Lo que hay que comprar, por lugar.
func (h *POSHandler) ComprasShopPage(w http.ResponseWriter, r *http.Request) {
	data, err := h.comprasShopData(r.Context())
	if err != nil {
		serverError(w, "Error cargando la lista", err)
		return
	}
	comprasRender(w, r, "comprar.html", "comprar", data)
}

// POST /compras/comprar/{id}/{comprado|no-habia|pendiente|cantidad}
func (h *POSHandler) ComprasShopAction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	u := comprasFrom(ctx)
	var err error
	switch r.PathValue("action") {
	case "comprado":
		var qty float64
		if v := r.FormValue("quantity"); v != "" {
			qty, err = parseQty(v)
		}
		place, _ := strconv.ParseInt(r.FormValue("place"), 10, 64)
		var total *int64
		cents, has, perr := parseCents(r.FormValue("total"))
		switch {
		case err != nil:
		case perr != nil:
			err = perr
		case has:
			total = &cents
		}
		if err == nil {
			err = h.comprasBuy(ctx, u, id, qty, total, place)
		}
	case "no-habia":
		err = h.comprasSetItem(ctx, id, `
			UPDATE compras_items SET status = 'NOT_FOUND', resolution_note = ?, bought_by = ?, bought_at = CURRENT_TIMESTAMP,
			       bought_quantity = NULL, bought_price_cents = NULL, bought_place_id = NULL WHERE id = ?`,
			strings.TrimSpace(r.FormValue("note")), u.userArg())
	case "pendiente":
		err = h.comprasSetItem(ctx, id, `
			UPDATE compras_items SET status = 'PENDING', resolution_note = '', bought_by = NULL, bought_at = NULL,
			       bought_quantity = NULL, bought_price_cents = NULL, bought_place_id = NULL WHERE id = ?`)
	case "cantidad":
		qty, perr := parseQty(r.FormValue("quantity"))
		if perr != nil {
			err = perr
		} else {
			err = h.comprasSetItem(ctx, id, `UPDATE compras_items SET quantity = ? WHERE id = ?`, qty)
		}
	default:
		http.NotFound(w, r)
		return
	}
	var bad badRequest
	if err != nil && !errors.As(err, &bad) {
		serverError(w, "Error guardando la compra", err)
		return
	}
	data, lerr := h.comprasShopData(ctx)
	if lerr != nil {
		serverError(w, "Error cargando la lista", lerr)
		return
	}
	if err != nil {
		data["Error"] = err.Error()
	}
	data["Who"] = u
	renderBlock(w, "compras_shop", data, "compras/comprar.html", "compras/layout.html")
}

// --- Lista (caja) ---

type comprasHistory struct {
	ID         int64
	OpenedAt   string
	Items      int
	Bought     int
	NotFound   int
	SpentCents int64
}

// GET /compras/lista - Estado de la lista actual e historial.
func (h *POSHandler) ComprasListPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	list, err := comprasActive(ctx, h.DB)
	if err != nil {
		serverError(w, "Error cargando la lista", err)
		return
	}
	data := map[string]any{"List": list, "Error": r.URL.Query().Get("error")}
	if list != nil {
		items, err := comprasItems(ctx, h.DB, list.ID)
		if err != nil {
			serverError(w, "Error cargando la lista", err)
			return
		}
		data["Items"] = items
		data["Totals"] = comprasTotalsOf(items)
	}
	var history []comprasHistory
	if err := eachRow(ctx, h.DB, `
		SELECT l.id, strftime('%d/%m/%Y', l.opened_at, 'localtime'), COUNT(i.id),
		       COALESCE(SUM(i.status = 'BOUGHT'), 0), COALESCE(SUM(i.status = 'NOT_FOUND'), 0),
		       COALESCE(SUM(i.bought_price_cents), 0)
		FROM compras_lists l LEFT JOIN compras_items i ON i.list_id = l.id
		WHERE l.status = 'CLOSED' GROUP BY l.id ORDER BY l.closed_at DESC, l.id DESC LIMIT 10`, nil,
		func(scan func(...any) error) error {
			var hh comprasHistory
			if err := scan(&hh.ID, &hh.OpenedAt, &hh.Items, &hh.Bought, &hh.NotFound, &hh.SpentCents); err != nil {
				return err
			}
			history = append(history, hh)
			return nil
		}); err != nil {
		serverError(w, "Error cargando el historial", err)
		return
	}
	data["History"] = history
	comprasRender(w, r, "lista.html", "lista", data)
}

// POST /compras/lista/{abrir|a-compras|cerrar}
func (h *POSHandler) ComprasListAction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := comprasFrom(ctx)
	var err error
	switch r.PathValue("action") {
	case "abrir":
		err = h.comprasOpen(ctx, u)
	case "a-compras":
		err = h.comprasSetStatus(ctx, u, "OPEN", "SHOPPING")
	case "cerrar":
		err = h.comprasSetStatus(ctx, u, "", "CLOSED")
	default:
		http.NotFound(w, r)
		return
	}
	var bad badRequest
	if errors.As(err, &bad) {
		http.Redirect(w, r, "/compras/lista?error="+url.QueryEscape(bad.Error()), http.StatusSeeOther)
		return
	}
	if err != nil {
		serverError(w, "Error actualizando la lista", err)
		return
	}
	http.Redirect(w, r, "/compras/lista", http.StatusSeeOther)
}

// GET /compras/lista/texto - La lista en texto, para copiar.
func (h *POSHandler) ComprasText(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	list, err := comprasActive(ctx, h.DB)
	if err != nil {
		serverError(w, "Error cargando la lista", err)
		return
	}
	if list == nil {
		http.Error(w, "No hay lista abierta", http.StatusNotFound)
		return
	}
	items, err := comprasItems(ctx, h.DB, list.ID)
	if err != nil {
		serverError(w, "Error cargando la lista", err)
		return
	}
	settings, err := loadSettings(ctx, h.DB)
	if err != nil {
		serverError(w, "Error cargando la configuración", err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(comprasText(settings.BusinessName, items)))
}
