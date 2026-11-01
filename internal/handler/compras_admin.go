package handler

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// --- Administración de Compras (Admin → 🛒 Compras) ---

type comprasSupply struct {
	ID          int64
	Name        string
	Category    string
	Unit        string
	Location    string
	Notes       string
	Active      bool
	Sources     []comprasSource
	Substitutes []int64
}

type comprasPlace struct {
	ID        int64
	Name      string
	Kind      string
	Address   string
	Phone     string
	Notes     string
	SortOrder int
	Active    bool
	Supplies  int
}

var comprasPlaceKinds = []struct{ Key, Label string }{
	{"TIENDA", "Tienda / súper"}, {"MERCADO", "Mercado"}, {"PROVEEDOR", "Proveedor (entrega)"}, {"EN_LINEA", "En línea"},
}

func comprasLoadPlaces(ctx context.Context, q queryer, onlyActive bool) ([]comprasPlace, error) {
	places := []comprasPlace{}
	err := eachRow(ctx, q, `
		SELECT p.id, p.name, p.kind, p.address, p.phone, p.notes, p.sort_order, p.is_active,
		       (SELECT COUNT(*) FROM compras_sources s WHERE s.place_id = p.id)
		FROM compras_places p WHERE p.is_active = 1 OR ? = 0
		ORDER BY p.sort_order, p.name_key`, []any{onlyActive}, func(scan func(...any) error) error {
		var p comprasPlace
		if err := scan(&p.ID, &p.Name, &p.Kind, &p.Address, &p.Phone, &p.Notes, &p.SortOrder, &p.Active, &p.Supplies); err != nil {
			return err
		}
		places = append(places, p)
		return nil
	})
	return places, err
}

// comprasSupplyRow es un insumo en la lista del panel, que lo agrupa y filtra
// en el navegador (por ubicación, categoría, lugar o precio).
type comprasSupplyRow struct {
	ID         int64    `json:"id"`
	Name       string   `json:"name"`
	Category   string   `json:"category"`
	Unit       string   `json:"unit"`
	Location   string   `json:"location"`
	Active     bool     `json:"active"`
	Places     []string `json:"places"` // en orden de prioridad (solo lugares activos)
	PriceCents *int64   `json:"price"`  // del lugar preferido, por presentación (o por kg a granel)
	PriceLabel string   `json:"price_label"`
	UnitPrice  string   `json:"unit_price"` // "$175.00/kg"
	Pack       string   `json:"pack"`       // presentación del lugar de siempre
	Cheaper    string   `json:"cheaper"`    // "Costco ($70.00 · $175.00/kg)" si otro lugar sale más barato
	Subs       int      `json:"subs"`       // sustitutos
}

type comprasShopper struct {
	ID      int64
	Name    string
	Role    string
	Shopper bool
}

func comprasSupplyRows(ctx context.Context, q queryer) ([]comprasSupplyRow, error) {
	rows := []comprasSupplyRow{}
	index := map[int64]int{}
	err := eachRow(ctx, q, `
		SELECT s.id, s.name, s.category, s.unit, s.location, s.is_active,
		       (SELECT COUNT(*) FROM compras_substitutes x WHERE x.supply_id = s.id)
		FROM compras_supplies s ORDER BY s.name_key`, nil, func(scan func(...any) error) error {
		r := comprasSupplyRow{Places: []string{}}
		if err := scan(&r.ID, &r.Name, &r.Category, &r.Unit, &r.Location, &r.Active, &r.Subs); err != nil {
			return err
		}
		index[r.ID] = len(rows)
		rows = append(rows, r)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sources := map[int64][]comprasSource{}
	err = eachRow(ctx, q, `
		SELECT x.supply_id, p.name, x.price_cents, x.pack_label, x.content_qty, x.content_unit, x.by_weight
		FROM compras_sources x JOIN compras_places p ON p.id = x.place_id
		WHERE p.is_active = 1 ORDER BY x.supply_id, x.rank`, nil, func(scan func(...any) error) error {
		var id int64
		var s comprasSource
		if err := scan(&id, &s.Place, &s.PriceCents, &s.PackLabel, &s.ContentQty, &s.ContentUnit, &s.ByWeight); err != nil {
			return err
		}
		sources[id] = append(sources[id], s)
		return nil
	})
	for id, list := range sources {
		r := &rows[index[id]]
		for i := range list {
			list[i].fillContent(r.Unit) // los capturados antes del contenido
			r.Places = append(r.Places, list[i].Place)
		}
		pref := list[0]
		r.PriceCents, r.PriceLabel, r.UnitPrice, r.Pack = pref.PriceCents, pref.PriceLabel(), pref.UnitPriceLabel(), pref.Pack(r.Unit)
		if i := comprasCheapest(list); i > 0 {
			cheap := list[i]
			r.Cheaper = cheap.Place + " (" + cheap.PriceLabel()
			if u := cheap.UnitPriceLabel(); u != "" && !cheap.ByWeight {
				r.Cheaper += " · " + u
			}
			r.Cheaper += ")"
		}
	}
	return rows, err
}

// GET /admin/compras?tab=insumos|lugares|surten|importar
func (h *POSHandler) ComprasAdmin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tab := r.URL.Query().Get("tab")
	data := h.adminPageData(r, "compras")
	data["Tab"] = tab
	data["Error"] = r.URL.Query().Get("error")
	data["Saved"] = r.URL.Query().Get("ok")
	var err error
	switch tab {
	case "lugares":
		data["Places"], err = comprasLoadPlaces(ctx, h.DB, false)
		data["Kinds"] = comprasPlaceKinds
	case "surten":
		var people []comprasShopper
		err = eachRow(ctx, h.DB, `
			SELECT u.id, u.name, u.role, EXISTS (SELECT 1 FROM compras_shoppers s WHERE s.user_id = u.id)
			FROM users u WHERE u.is_active = 1 ORDER BY u.name`, nil, func(scan func(...any) error) error {
			var p comprasShopper
			if err := scan(&p.ID, &p.Name, &p.Role, &p.Shopper); err != nil {
				return err
			}
			if p.Role == RoleAdmin || p.Role == RoleManager {
				p.Shopper = true // siempre pueden
			}
			people = append(people, p)
			return nil
		})
		data["People"] = people
		data["RoleNames"] = roleLabels
	case "importar":
	default:
		data["Tab"] = "insumos"
		data["Supplies"], err = comprasSupplyRows(ctx, h.DB)
	}
	if err != nil {
		serverError(w, "Error cargando Compras", err)
		return
	}
	renderPage(w, data, "admin/compras.html", "admin/_nav.html")
}

func comprasLoadSupply(ctx context.Context, q queryer, id int64) (*comprasSupply, error) {
	sp := &comprasSupply{}
	if err := q.QueryRowContext(ctx, `SELECT id, name, category, unit, location, notes, is_active FROM compras_supplies WHERE id = ?`,
		id).Scan(&sp.ID, &sp.Name, &sp.Category, &sp.Unit, &sp.Location, &sp.Notes, &sp.Active); err != nil {
		return nil, err
	}
	err := eachRow(ctx, q, `SELECT p.id, p.name, x.brand, x.price_cents, x.rank, p.sort_order, x.note,
		       x.pack_label, x.content_qty, x.content_unit, x.by_weight FROM compras_sources x
		JOIN compras_places p ON p.id = x.place_id WHERE x.supply_id = ? ORDER BY x.rank`, []any{id},
		func(scan func(...any) error) error {
			var s comprasSource
			if err := scan(&s.PlaceID, &s.Place, &s.Brand, &s.PriceCents, &s.Rank, &s.SortOrder, &s.Note,
				&s.PackLabel, &s.ContentQty, &s.ContentUnit, &s.ByWeight); err != nil {
				return err
			}
			sp.Sources = append(sp.Sources, s)
			return nil
		})
	if err != nil {
		return nil, err
	}
	err = eachRow(ctx, q, `SELECT substitute_id FROM compras_substitutes WHERE supply_id = ?`, []any{id},
		func(scan func(...any) error) error {
			var sub int64
			if err := scan(&sub); err != nil {
				return err
			}
			sp.Substitutes = append(sp.Substitutes, sub)
			return nil
		})
	return sp, err
}

// GET /admin/compras/insumos/nuevo y /admin/compras/insumos/{id}
func (h *POSHandler) ComprasSupplyForm(w http.ResponseWriter, r *http.Request) {
	sp := &comprasSupply{Active: true}
	if id, ok := pathID(r); ok {
		var err error
		if sp, err = comprasLoadSupply(r.Context(), h.DB, id); errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		} else if err != nil {
			serverError(w, "Error cargando el insumo", err)
			return
		}
	}
	h.comprasSupplyForm(w, r, sp, "")
}

type comprasOption struct {
	ID   int64
	Name string
}

func (h *POSHandler) comprasSupplyForm(w http.ResponseWriter, r *http.Request, sp *comprasSupply, errMsg string) {
	ctx := r.Context()
	places, err := comprasLoadPlaces(ctx, h.DB, true)
	if err != nil {
		serverError(w, "Error cargando lugares", err)
		return
	}
	var others []comprasOption
	var cats, locs []string
	err = eachRow(ctx, h.DB, `SELECT id, name FROM compras_supplies WHERE is_active = 1 AND id <> ? ORDER BY name_key`,
		[]any{sp.ID}, func(scan func(...any) error) error {
			var o comprasOption
			if err := scan(&o.ID, &o.Name); err != nil {
				return err
			}
			others = append(others, o)
			return nil
		})
	for _, q := range []struct {
		sql string
		dst *[]string
	}{
		{`SELECT DISTINCT category FROM compras_supplies WHERE category <> '' ORDER BY 1`, &cats},
		{`SELECT DISTINCT location FROM compras_supplies WHERE location <> '' ORDER BY 1`, &locs},
	} {
		if err != nil {
			break
		}
		err = eachRow(ctx, h.DB, q.sql, nil, func(scan func(...any) error) error {
			var s string
			if err := scan(&s); err != nil {
				return err
			}
			*q.dst = append(*q.dst, s)
			return nil
		})
	}
	if err != nil {
		serverError(w, "Error cargando el formulario", err)
		return
	}
	subs := map[int64]bool{}
	for _, id := range sp.Substitutes {
		subs[id] = true
	}
	// Los lugares del insumo para el formulario (Alpine), en su orden.
	type sourceRow struct {
		Place    string `json:"place"`
		Brand    string `json:"brand"`
		Price    string `json:"price"`
		Note     string `json:"note"`
		Pack     string `json:"pack"`
		Qty      string `json:"qty"`
		Unit     string `json:"unit"`
		ByWeight bool   `json:"byWeight"`
	}
	rows := []sourceRow{}
	for _, s := range sp.Sources {
		row := sourceRow{Place: strconv.FormatInt(s.PlaceID, 10), Brand: s.Brand, Note: s.Note,
			Pack: s.PackLabel, Unit: s.ContentUnit, ByWeight: s.ByWeight}
		if s.PriceCents != nil {
			row.Price = formatCents(*s.PriceCents)
		}
		if s.ContentQty != nil {
			row.Qty = formatQty(*s.ContentQty)
		}
		rows = append(rows, row)
	}
	data := h.adminPageData(r, "compras")
	for k, v := range map[string]any{"Supply": sp, "Places": places, "Others": others, "Subs": subs,
		"Categories": cats, "Locations": locs, "Error": errMsg, "SourceRows": rows, "Units": comprasUnitOptions} {
		data[k] = v
	}
	if errMsg != "" {
		w.WriteHeader(http.StatusUnprocessableEntity)
	}
	renderPage(w, data, "admin/compras_insumo.html", "admin/_nav.html")
}

// POST /admin/compras/insumos[/{id}]
func (h *POSHandler) ComprasSaveSupply(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Formulario inválido", http.StatusBadRequest)
		return
	}
	sp := &comprasSupply{
		Name: strings.TrimSpace(r.FormValue("name")), Category: strings.TrimSpace(r.FormValue("category")),
		Unit: strings.TrimSpace(r.FormValue("unit")), Location: strings.TrimSpace(r.FormValue("location")),
		Notes: strings.TrimSpace(r.FormValue("notes")), Active: formChecked(r, "active"),
	}
	sp.ID, _ = pathID(r)
	// Lugares en orden de preferencia; cada campo es una lista alineada con
	// place[] (la casilla "a granel" va como by_weight[] = "0"/"1").
	field := func(name string, i int) string {
		if v := r.Form[name]; i < len(v) {
			return strings.TrimSpace(v[i])
		}
		return ""
	}
	places := r.Form["place"]
	seen := map[int64]bool{}
	for i, p := range places {
		id, _ := strconv.ParseInt(p, 10, 64)
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		src := comprasSource{PlaceID: id, Rank: len(sp.Sources) + 1, Brand: field("brand", i), Note: field("source_note", i),
			PackLabel: field("pack_label", i), ByWeight: field("by_weight", i) == "1"}
		cents, ok, err := parseCents(field("price", i))
		if err != nil {
			h.comprasSupplyForm(w, r, sp, err.Error())
			return
		}
		if ok {
			src.PriceCents = &cents
		}
		if q := field("content_qty", i); q != "" && !src.ByWeight {
			qty, err := strconv.ParseFloat(strings.Replace(q, ",", ".", 1), 64)
			unit := field("content_unit", i)
			if _, known := comprasUnits[unit]; err != nil || qty <= 0 || !known {
				h.comprasSupplyForm(w, r, sp, "El contenido de la presentación debe ser un número con su unidad (g, kg, ml, L o pz).")
				return
			}
			src.ContentQty, src.ContentUnit = &qty, unit
		}
		sp.Sources = append(sp.Sources, src)
	}
	for _, v := range r.Form["substitute"] {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil && id != sp.ID {
			sp.Substitutes = append(sp.Substitutes, id)
		}
	}
	if sp.Name == "" {
		h.comprasSupplyForm(w, r, sp, "Escribe el nombre del insumo.")
		return
	}
	err := h.comprasTx(r, func(tx *sql.Tx) error {
		if err := comprasSaveSupply(r.Context(), tx, sp); err != nil {
			return err
		}
		admin, _ := currentUser(r.Context())
		return auditBy(r.Context(), tx, admin, r, "COMPRAS_SUPPLY_SAVED", map[string]any{"supply_id": sp.ID, "name": sp.Name})
	})
	var bad badRequest
	switch {
	case errors.As(err, &bad):
		h.comprasSupplyForm(w, r, sp, bad.Error())
	case errors.Is(err, sql.ErrNoRows):
		http.NotFound(w, r)
	case err != nil:
		serverError(w, "Error guardando el insumo", err)
	default:
		http.Redirect(w, r, "/admin/compras?ok="+url.QueryEscape(sp.Name), http.StatusSeeOther)
	}
}

func (h *POSHandler) comprasTx(r *http.Request, fn func(tx *sql.Tx) error) error {
	tx, err := h.DB.BeginTx(r.Context(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// comprasSaveSupply crea o actualiza un insumo con sus lugares y sustitutos.
func comprasSaveSupply(ctx context.Context, tx *sql.Tx, sp *comprasSupply) error {
	key := comprasKey(sp.Name)
	var dup bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM compras_supplies WHERE name_key = ? AND id <> ?)`, key, sp.ID).Scan(&dup); err != nil {
		return err
	}
	if dup {
		return badRequest("Ya hay un insumo con ese nombre.")
	}
	if sp.ID == 0 {
		res, err := tx.ExecContext(ctx, `INSERT INTO compras_supplies (name, name_key, category, unit, location, notes, is_active)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, sp.Name, key, sp.Category, sp.Unit, sp.Location, sp.Notes, sp.Active)
		if err != nil {
			return err
		}
		sp.ID, _ = res.LastInsertId()
	} else {
		res, err := tx.ExecContext(ctx, `UPDATE compras_supplies SET name = ?, name_key = ?, category = ?, unit = ?, location = ?,
			notes = ?, is_active = ? WHERE id = ?`, sp.Name, key, sp.Category, sp.Unit, sp.Location, sp.Notes, sp.Active, sp.ID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return sql.ErrNoRows
		}
	}
	keep := []any{sp.ID}
	for _, src := range sp.Sources {
		src.fillContent(sp.Unit)
		var ok bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM compras_places WHERE id = ?)`, src.PlaceID).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return badRequest("Algún lugar no existe.")
		}
		var price any
		if src.PriceCents != nil {
			price = *src.PriceCents
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO compras_sources (supply_id, place_id, brand, price_cents, rank, note,
			                             pack_label, content_qty, content_unit, by_weight)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (supply_id, place_id) DO UPDATE SET brand = excluded.brand, price_cents = excluded.price_cents,
			       rank = excluded.rank, note = excluded.note, pack_label = excluded.pack_label,
			       content_qty = excluded.content_qty, content_unit = excluded.content_unit, by_weight = excluded.by_weight,
			       updated_at = CURRENT_TIMESTAMP`,
			sp.ID, src.PlaceID, src.Brand, price, src.Rank, src.Note,
			src.PackLabel, src.ContentQty, src.ContentUnit, src.ByWeight); err != nil {
			return err
		}
		keep = append(keep, src.PlaceID)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM compras_sources WHERE supply_id = ? AND place_id NOT IN (0`+
		strings.Repeat(",?", len(keep)-1)+`)`, keep...); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM compras_substitutes WHERE supply_id = ?`, sp.ID); err != nil {
		return err
	}
	for _, sub := range sp.Substitutes {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO compras_substitutes (supply_id, substitute_id)
			SELECT ?, id FROM compras_supplies WHERE id = ? AND id <> ?`, sp.ID, sub, sp.ID); err != nil {
			return err
		}
	}
	return nil
}

// formChecked lee una casilla que va junto a un campo oculto con "0" (para
// que desmarcarla también llegue): llegan "0" y "1" si está marcada. Sin el
// campo (alta nueva) cuenta como marcada.
func formChecked(r *http.Request, name string) bool {
	r.ParseForm()
	values, ok := r.Form[name]
	if !ok {
		return true
	}
	for _, v := range values {
		if v == "1" {
			return true
		}
	}
	return false
}

// POST /admin/compras/lugares[/{id}]
func (h *POSHandler) ComprasSavePlace(w http.ResponseWriter, r *http.Request) {
	p := comprasPlace{
		Name: strings.TrimSpace(r.FormValue("name")), Kind: r.FormValue("kind"), Address: strings.TrimSpace(r.FormValue("address")),
		Phone: strings.TrimSpace(r.FormValue("phone")), Notes: strings.TrimSpace(r.FormValue("notes")),
		Active: formChecked(r, "active"),
	}
	p.SortOrder, _ = strconv.Atoi(r.FormValue("sort_order"))
	p.ID, _ = pathID(r)
	valid := false
	for _, k := range comprasPlaceKinds {
		valid = valid || k.Key == p.Kind
	}
	if !valid {
		p.Kind = "TIENDA"
	}
	back := func(msg string) {
		http.Redirect(w, r, "/admin/compras?tab=lugares&error="+url.QueryEscape(msg), http.StatusSeeOther)
	}
	if p.Name == "" {
		back("Escribe el nombre del lugar.")
		return
	}
	err := h.comprasTx(r, func(tx *sql.Tx) error {
		var dup bool
		if err := tx.QueryRowContext(r.Context(), `SELECT EXISTS (SELECT 1 FROM compras_places WHERE name_key = ? AND id <> ?)`,
			comprasKey(p.Name), p.ID).Scan(&dup); err != nil {
			return err
		}
		if dup {
			return badRequest("Ya hay un lugar con ese nombre.")
		}
		var err error
		if p.ID == 0 {
			_, err = tx.ExecContext(r.Context(), `INSERT INTO compras_places (name, name_key, kind, address, phone, notes, sort_order)
				VALUES (?, ?, ?, ?, ?, ?, ?)`, p.Name, comprasKey(p.Name), p.Kind, p.Address, p.Phone, p.Notes, p.SortOrder)
		} else {
			_, err = tx.ExecContext(r.Context(), `UPDATE compras_places SET name = ?, name_key = ?, kind = ?, address = ?, phone = ?,
				notes = ?, sort_order = ?, is_active = ? WHERE id = ?`, p.Name, comprasKey(p.Name), p.Kind, p.Address, p.Phone,
				p.Notes, p.SortOrder, p.Active, p.ID)
		}
		return err
	})
	var bad badRequest
	if errors.As(err, &bad) {
		back(bad.Error())
		return
	}
	if err != nil {
		serverError(w, "Error guardando el lugar", err)
		return
	}
	http.Redirect(w, r, "/admin/compras?tab=lugares", http.StatusSeeOther)
}

// POST /admin/compras/surten/{id} - shopper=1 para que surta desde su terminal.
func (h *POSHandler) ComprasSetShopper(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	q := `DELETE FROM compras_shoppers WHERE user_id = ?`
	if r.FormValue("shopper") == "1" {
		q = `INSERT OR IGNORE INTO compras_shoppers (user_id) SELECT id FROM users WHERE id = ?`
	}
	if _, err := h.DB.ExecContext(r.Context(), q, id); err != nil {
		serverError(w, "Error guardando", err)
		return
	}
	http.Redirect(w, r, "/admin/compras?tab=surten", http.StatusSeeOther)
}

// --- Importar desde Excel (CSV) ---

type comprasImportResult struct {
	Created, Updated, Places int
	Skipped                  []string
}

// Columnas reconocidas (sin acentos, minúsculas).
var comprasColumns = map[string]string{
	"nombre": "name", "insumo": "name", "producto": "name", "articulo": "name",
	"categoria": "category", "tipo": "category",
	"unidad": "unit", "presentacion": "unit",
	"ubicacion": "location", "area": "location", "donde se guarda": "location",
	"lugar": "place", "tienda": "place", "donde se compra": "place", "proveedor": "place",
	"precio": "price", "costo": "price",
	"marca":       "brand",
	"alternativa": "place2", "lugar 2": "place2", "lugar alternativo": "place2",
	"precio 2": "price2",
	"notas":    "notes", "nota": "notes",
}

// comprasImport da de alta (o actualiza por nombre) insumos de un CSV
// exportado de Excel. Crea los lugares que no existan.
func comprasImport(ctx context.Context, tx *sql.Tx, in io.Reader) (comprasImportResult, error) {
	var res comprasImportResult
	br := bufio.NewReader(in)
	head, _ := br.Peek(4096)
	firstLine, _, _ := strings.Cut(strings.TrimPrefix(string(head), "\ufeff"), "\n")
	cr := csv.NewReader(br)
	// Excel en español guarda con ";".
	if strings.Count(firstLine, ";") > strings.Count(firstLine, ",") {
		cr.Comma = ';'
	}
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	records, err := cr.ReadAll()
	if err != nil {
		return res, badRequest("No se pudo leer el archivo: " + err.Error())
	}
	if len(records) < 2 {
		return res, badRequest("El archivo no tiene renglones.")
	}
	cols := map[string]int{}
	for i, hdr := range records[0] {
		if key, ok := comprasColumns[comprasKey(strings.TrimPrefix(hdr, "\ufeff"))]; ok {
			if _, dup := cols[key]; !dup {
				cols[key] = i
			}
		}
	}
	if _, ok := cols["name"]; !ok {
		return res, badRequest(`Falta la columna del nombre ("Nombre", "Insumo" o "Producto").`)
	}
	get := func(rec []string, key string) string {
		if i, ok := cols[key]; ok && i < len(rec) {
			return strings.TrimSpace(rec[i])
		}
		return ""
	}
	placeIDs := map[string]int64{}
	placeFor := func(name string) (int64, error) {
		key := comprasKey(name)
		if id, ok := placeIDs[key]; ok {
			return id, nil
		}
		var id int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM compras_places WHERE name_key = ?`, key).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			r, ierr := tx.ExecContext(ctx, `INSERT INTO compras_places (name, name_key) VALUES (?, ?)`, name, key)
			if ierr != nil {
				return 0, ierr
			}
			id, err = r.LastInsertId()
			res.Places++
		}
		placeIDs[key] = id
		return id, err
	}
	for n, rec := range records[1:] {
		name := get(rec, "name")
		if name == "" {
			continue
		}
		sp := &comprasSupply{Name: name, Category: get(rec, "category"), Unit: get(rec, "unit"),
			Location: get(rec, "location"), Notes: get(rec, "notes"), Active: true}
		var existing int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM compras_supplies WHERE name_key = ?`, comprasKey(name)).Scan(&existing)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return res, err
		}
		if existing > 0 {
			// Conserva lo que ya tenía y solo llena lo que trae el archivo.
			cur, err := comprasLoadSupply(ctx, tx, existing)
			if err != nil {
				return res, err
			}
			for _, f := range []struct{ dst, src *string }{{&cur.Category, &sp.Category}, {&cur.Unit, &sp.Unit},
				{&cur.Location, &sp.Location}, {&cur.Notes, &sp.Notes}} {
				if *f.src != "" {
					*f.dst = *f.src
				}
			}
			sp = cur
		}
		for i, pair := range [][2]string{{"place", "price"}, {"place2", "price2"}} {
			placeName := get(rec, pair[0])
			if placeName == "" {
				continue
			}
			placeID, err := placeFor(placeName)
			if err != nil {
				return res, err
			}
			src := comprasSource{PlaceID: placeID}
			if i == 0 {
				src.Brand = get(rec, "brand")
			}
			if cents, ok, err := parseCents(get(rec, pair[1])); err != nil {
				res.Skipped = append(res.Skipped, fmt.Sprintf("Renglón %d (%s): %v", n+2, name, err))
			} else if ok {
				src.PriceCents = &cents
			}
			replaced := false
			for j := range sp.Sources {
				if sp.Sources[j].PlaceID == placeID {
					// El archivo no trae notas ni presentaciones por lugar: se conservan.
					old := sp.Sources[j]
					src.Note, src.PackLabel, src.ContentQty, src.ContentUnit, src.ByWeight =
						old.Note, old.PackLabel, old.ContentQty, old.ContentUnit, old.ByWeight
					sp.Sources[j] = src
					replaced = true
				}
			}
			if !replaced {
				sp.Sources = append(sp.Sources, src)
			}
		}
		for j := range sp.Sources {
			sp.Sources[j].Rank = j + 1
		}
		if err := comprasSaveSupply(ctx, tx, sp); err != nil {
			return res, err
		}
		if existing > 0 {
			res.Updated++
		} else {
			res.Created++
		}
	}
	return res, nil
}

// POST /admin/compras/importar
func (h *POSHandler) ComprasImport(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 5<<20)
	data := h.adminPageData(r, "compras")
	data["Tab"] = "importar"
	file, _, err := r.FormFile("file")
	if err != nil {
		data["Error"] = "Elige un archivo CSV."
		w.WriteHeader(http.StatusUnprocessableEntity)
		renderPage(w, data, "admin/compras.html", "admin/_nav.html")
		return
	}
	defer file.Close()
	var res comprasImportResult
	err = h.comprasTx(r, func(tx *sql.Tx) error {
		var err error
		if res, err = comprasImport(r.Context(), tx, file); err != nil {
			return err
		}
		admin, _ := currentUser(r.Context())
		return auditBy(r.Context(), tx, admin, r, "COMPRAS_IMPORTED", map[string]any{
			"created": res.Created, "updated": res.Updated, "places": res.Places})
	})
	var bad badRequest
	if errors.As(err, &bad) {
		data["Error"] = bad.Error()
		w.WriteHeader(http.StatusUnprocessableEntity)
		renderPage(w, data, "admin/compras.html", "admin/_nav.html")
		return
	}
	if err != nil {
		serverError(w, "Error importando", err)
		return
	}
	data["Result"] = res
	renderPage(w, data, "admin/compras.html", "admin/_nav.html")
}
