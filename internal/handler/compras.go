package handler

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// --- Compras ---
//
// La lista de compras del negocio, en lugar de la hoja de papel con "falta" y
// "urge". Caja abre una lista; el equipo anota lo que falta desde su terminal
// (con su PIN); quien surte la ve por lugar (Costco, mercado...), con
// presupuesto, y va tachando; caja la cierra y lo que no se compró pasa solo a
// la siguiente. El catálogo de insumos se administra en Admin → Compras.

// comprasUser es quién usa Compras y qué puede hacer.
type comprasUser struct {
	UserID  int64 // 0 = compu central sin usuario
	Name    string
	Central bool
	CanOpen bool // abrir, mandar a compras y cerrar listas
	CanShop bool // surtir
}

// userArg es el usuario para guardar en la base (NULL en la compu central).
func (u comprasUser) userArg() any {
	if u.UserID == 0 {
		return nil
	}
	return u.UserID
}

// comprasWho: la compu central y las terminales de caja completa abren listas
// y surten; en las de mesero, los encargados y cajeros abren listas y surten
// quienes el admin marcó (y encargados).
func (h *POSHandler) comprasWho(ctx context.Context) (comprasUser, error) {
	t := terminalFrom(ctx)
	u := comprasUser{UserID: t.UserID, Name: t.UserName, Central: t.Central}
	if u.Name == "" {
		u.Name = "Caja"
	}
	if t.Kind == TerminalFull {
		u.CanOpen, u.CanShop = true, true
		return u, nil
	}
	switch t.UserRole {
	case RoleAdmin, RoleManager:
		u.CanOpen, u.CanShop = true, true
	case RoleCashier:
		u.CanOpen = true
	}
	if !u.CanShop && u.UserID > 0 {
		if err := h.DB.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM compras_shoppers WHERE user_id = ?)`,
			u.UserID).Scan(&u.CanShop); err != nil {
			return u, err
		}
	}
	return u, nil
}

// --- Modelo ---

type comprasList struct {
	ID         int64
	Status     string
	OpenedAt   time.Time
	OpenedBy   string
	ShoppingAt sql.NullTime
}

func (l *comprasList) StatusLabel() string {
	return map[string]string{"OPEN": "Abierta", "SHOPPING": "En compras", "CLOSED": "Cerrada"}[l.Status]
}

type comprasRequest struct {
	UserID int64
	Name   string
	Urgent bool
	Note   string
}

type comprasSource struct {
	PlaceID    int64
	Place      string
	Brand      string
	PriceCents *int64
	Rank       int
	SortOrder  int    // orden del lugar (para agrupar la compra)
	Note       string // por qué aquí (o solo si no hay en otro lado)
	// Presentación en este lugar (ver compras_units.go).
	PackLabel   string
	ContentQty  *float64
	ContentUnit string
	ByWeight    bool // a granel: PriceCents es por kg
}

type comprasItem struct {
	ID               int64
	SupplyID         int64
	Supply           string
	Unit             string
	Category         string
	Location         string
	Urgent           bool
	Quantity         float64 // presentaciones, o kg si se compra a granel
	Status           string
	FirstRequestedAt time.Time
	Carried          bool
	Late             bool // se pidió cuando ya estaban comprando
	Requests         []comprasRequest
	Sources          []comprasSource // en orden de preferencia
	Substitutes      []string
	BoughtCents      *int64
	BoughtPlace      string
	Note             string
}

// Preferred es donde se compra normalmente (nil si no tiene lugar).
func (it *comprasItem) Preferred() *comprasSource {
	if len(it.Sources) == 0 {
		return nil
	}
	return &it.Sources[0]
}

// Alternatives son los otros lugares.
func (it *comprasItem) Alternatives() []comprasSource {
	if len(it.Sources) < 2 {
		return nil
	}
	return it.Sources[1:]
}

// EstimateCents es el precio de referencia × cantidad (0 sin precio). A
// granel el precio es por kg y la cantidad en kg.
func (it *comprasItem) EstimateCents() int64 {
	for _, s := range it.Sources {
		if s.PriceCents != nil {
			return int64(math.Round(float64(*s.PriceCents) * it.Quantity))
		}
	}
	return 0
}

func (it *comprasItem) HasEstimate() bool { return it.EstimateCents() > 0 }

// RequestedBy es "Ana, Luis".
func (it *comprasItem) RequestedBy() string {
	var names []string
	seen := map[string]bool{}
	for _, r := range it.Requests {
		if !seen[r.Name] {
			seen[r.Name] = true
			names = append(names, r.Name)
		}
	}
	return strings.Join(names, ", ")
}

// Notes junta las notas de quienes lo pidieron.
func (it *comprasItem) Notes() []string {
	var out []string
	for _, r := range it.Requests {
		if r.Note != "" {
			out = append(out, r.Name+": "+r.Note)
		}
	}
	return out
}

// comprasActive es la lista abierta o en compras (nil si no hay).
func comprasActive(ctx context.Context, q queryer) (*comprasList, error) {
	var l comprasList
	err := q.QueryRowContext(ctx, `
		SELECT l.id, l.status, l.opened_at, COALESCE(u.name, 'Caja'), l.shopping_at
		FROM compras_lists l LEFT JOIN users u ON u.id = l.opened_by
		WHERE l.status <> 'CLOSED'`).Scan(&l.ID, &l.Status, &l.OpenedAt, &l.OpenedBy, &l.ShoppingAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &l, nil
}

// comprasItems carga los renglones de una lista con quién los pidió, dónde
// se compran y sus sustitutos.
func comprasItems(ctx context.Context, q queryer, listID int64) ([]*comprasItem, error) {
	var items []*comprasItem
	byID := map[int64]*comprasItem{}
	bySupply := map[int64]*comprasItem{}
	err := eachRow(ctx, q, `
		SELECT i.id, s.id, s.name, s.unit, s.category, s.location, i.urgent, i.quantity, i.status,
		       i.first_requested_at, i.carried_from IS NOT NULL,
		       i.late,
		       i.bought_price_cents, COALESCE(p.name, ''), i.resolution_note
		FROM compras_items i
		JOIN compras_lists l ON l.id = i.list_id
		JOIN compras_supplies s ON s.id = i.supply_id
		LEFT JOIN compras_places p ON p.id = i.bought_place_id
		WHERE i.list_id = ?
		ORDER BY i.urgent DESC, s.name_key`, []any{listID}, func(scan func(...any) error) error {
		it := &comprasItem{}
		if err := scan(&it.ID, &it.SupplyID, &it.Supply, &it.Unit, &it.Category, &it.Location, &it.Urgent, &it.Quantity,
			&it.Status, &it.FirstRequestedAt, &it.Carried, &it.Late, &it.BoughtCents, &it.BoughtPlace, &it.Note); err != nil {
			return err
		}
		items = append(items, it)
		byID[it.ID], bySupply[it.SupplyID] = it, it
		return nil
	})
	if err != nil || len(items) == 0 {
		return items, err
	}
	if err := eachRow(ctx, q, `
		SELECT r.item_id, COALESCE(r.user_id, 0), COALESCE(u.name, 'Caja'), r.urgent, r.note
		FROM compras_requests r JOIN compras_items i ON i.id = r.item_id LEFT JOIN users u ON u.id = r.user_id
		WHERE i.list_id = ? ORDER BY r.created_at, r.id`, []any{listID}, func(scan func(...any) error) error {
		var itemID int64
		var req comprasRequest
		if err := scan(&itemID, &req.UserID, &req.Name, &req.Urgent, &req.Note); err != nil {
			return err
		}
		byID[itemID].Requests = append(byID[itemID].Requests, req)
		return nil
	}); err != nil {
		return nil, err
	}
	if err := eachRow(ctx, q, `
		SELECT src.supply_id, p.id, p.name, src.brand, src.price_cents, src.rank, p.sort_order, src.note,
		       src.pack_label, src.content_qty, src.content_unit, src.by_weight
		FROM compras_sources src JOIN compras_places p ON p.id = src.place_id
		JOIN compras_items i ON i.supply_id = src.supply_id AND i.list_id = ?
		WHERE p.is_active = 1 ORDER BY src.rank, p.sort_order, p.name_key`, []any{listID}, func(scan func(...any) error) error {
		var supplyID int64
		var s comprasSource
		if err := scan(&supplyID, &s.PlaceID, &s.Place, &s.Brand, &s.PriceCents, &s.Rank, &s.SortOrder, &s.Note,
			&s.PackLabel, &s.ContentQty, &s.ContentUnit, &s.ByWeight); err != nil {
			return err
		}
		s.fillContent(bySupply[supplyID].Unit) // los capturados antes del contenido
		bySupply[supplyID].Sources = append(bySupply[supplyID].Sources, s)
		return nil
	}); err != nil {
		return nil, err
	}
	err = eachRow(ctx, q, `
		SELECT x.supply_id, s.name FROM compras_substitutes x
		JOIN compras_supplies s ON s.id = x.substitute_id
		JOIN compras_items i ON i.supply_id = x.supply_id AND i.list_id = ?
		WHERE s.is_active = 1 ORDER BY s.name_key`, []any{listID}, func(scan func(...any) error) error {
		var supplyID int64
		var name string
		if err := scan(&supplyID, &name); err != nil {
			return err
		}
		bySupply[supplyID].Substitutes = append(bySupply[supplyID].Substitutes, name)
		return nil
	})
	return items, err
}

type comprasTotals struct {
	Items, Urgent, Pending, Bought, NotFound int
	EstimateCents                            int64 // de lo pendiente y lo comprado
	SpentCents                               int64
	Missing                                  int // renglones sin precio de referencia
}

func comprasTotalsOf(items []*comprasItem) comprasTotals {
	var t comprasTotals
	for _, it := range items {
		t.Items++
		if it.Urgent {
			t.Urgent++
		}
		switch it.Status {
		case "PENDING":
			t.Pending++
		case "BOUGHT":
			t.Bought++
			if it.BoughtCents != nil {
				t.SpentCents += *it.BoughtCents
			}
		case "NOT_FOUND":
			t.NotFound++
			continue
		}
		if it.HasEstimate() {
			t.EstimateCents += it.EstimateCents()
		} else {
			t.Missing++
		}
	}
	return t
}

// --- Abrir, mandar a compras y cerrar ---

// comprasOpen abre una lista. Lo que no se surtió en la anterior pasa solo,
// con quién lo pidió, su nota y desde cuándo.
func (h *POSHandler) comprasOpen(ctx context.Context, u comprasUser) error {
	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if l, err := comprasActive(ctx, tx); err != nil {
		return err
	} else if l != nil {
		return badRequest("Ya hay una lista abierta.")
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO compras_lists (opened_by) VALUES (?)`, u.userArg())
	if err != nil {
		return err
	}
	listID, _ := res.LastInsertId()
	var prev int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM compras_lists WHERE status = 'CLOSED' ORDER BY closed_at DESC, id DESC LIMIT 1`).Scan(&prev)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if prev > 0 {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO compras_items (list_id, supply_id, urgent, quantity, first_requested_at, carried_from)
			SELECT ?, i.supply_id, i.urgent, i.quantity, i.first_requested_at, i.id
			FROM compras_items i JOIN compras_supplies s ON s.id = i.supply_id
			WHERE i.list_id = ? AND i.status <> 'BOUGHT' AND s.is_active = 1`, listID, prev); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO compras_requests (item_id, user_id, urgent, note, created_at)
			SELECT i.id, r.user_id, r.urgent, r.note, r.created_at
			FROM compras_items i JOIN compras_requests r ON r.item_id = i.carried_from
			WHERE i.list_id = ?`, listID); err != nil {
			return err
		}
	}
	if err := audit(ctx, tx, "COMPRAS_LIST_OPENED", map[string]any{"list_id": listID, "user": u.Name}); err != nil {
		return err
	}
	return tx.Commit()
}

func (h *POSHandler) comprasSetStatus(ctx context.Context, u comprasUser, from, to string) error {
	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	l, err := comprasActive(ctx, tx)
	if err != nil {
		return err
	}
	if l == nil {
		return badRequest("No hay lista abierta.")
	}
	if from != "" && l.Status != from {
		return badRequest("La lista ya cambió de estado; recarga la página.")
	}
	switch to {
	case "SHOPPING":
		_, err = tx.ExecContext(ctx, `UPDATE compras_lists SET status = 'SHOPPING', shopping_at = CURRENT_TIMESTAMP WHERE id = ?`, l.ID)
	case "CLOSED":
		_, err = tx.ExecContext(ctx, `UPDATE compras_lists SET status = 'CLOSED', closed_at = CURRENT_TIMESTAMP, closed_by = ? WHERE id = ?`,
			u.userArg(), l.ID)
		if err == nil {
			err = audit(ctx, tx, "COMPRAS_LIST_CLOSED", map[string]any{"list_id": l.ID, "user": u.Name})
		}
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// --- Pedir ---

// comprasRequestSupply agrega (o actualiza) el pedido de un insumo.
func (h *POSHandler) comprasRequestSupply(ctx context.Context, u comprasUser, supplyID int64, urgent bool, note string) error {
	note = strings.TrimSpace(note)
	if len([]rune(note)) > 300 {
		note = string([]rune(note)[:300])
	}
	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	l, err := comprasActive(ctx, tx)
	if err != nil {
		return err
	}
	if l == nil {
		return badRequest("Todavía no hay lista abierta. Pídele a caja que la abra.")
	}
	var ok bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM compras_supplies WHERE id = ? AND is_active = 1)`, supplyID).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return badRequest("Ese insumo ya no existe.")
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO compras_items (list_id, supply_id, urgent, late) VALUES (?, ?, ?, ?)
		ON CONFLICT (list_id, supply_id) DO UPDATE SET urgent = MAX(urgent, excluded.urgent)`,
		l.ID, supplyID, urgent, l.Status == "SHOPPING"); err != nil {
		return err
	}
	var itemID int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM compras_items WHERE list_id = ? AND supply_id = ?`, l.ID, supplyID).Scan(&itemID); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE compras_requests SET urgent = ?, note = CASE WHEN ? = '' THEN note ELSE ? END
		WHERE item_id = ? AND user_id IS ?`, urgent, note, note, itemID, u.userArg())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO compras_requests (item_id, user_id, urgent, note) VALUES (?, ?, ?, ?)`,
			itemID, u.userArg(), urgent, note); err != nil {
			return err
		}
	}
	if err := comprasRecomputeUrgent(ctx, tx, itemID); err != nil {
		return err
	}
	return tx.Commit()
}

// comprasWithdraw quita mi pedido. Si nadie más lo pidió (y no venía de la
// lista anterior), el renglón desaparece.
func (h *POSHandler) comprasWithdraw(ctx context.Context, u comprasUser, supplyID int64) error {
	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	l, err := comprasActive(ctx, tx)
	if err != nil || l == nil {
		return err
	}
	var itemID int64
	var status string
	var carried bool
	err = tx.QueryRowContext(ctx, `SELECT id, status, carried_from IS NOT NULL FROM compras_items WHERE list_id = ? AND supply_id = ?`,
		l.ID, supplyID).Scan(&itemID, &status, &carried)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if status == "BOUGHT" {
		return badRequest("Ya se compró.")
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM compras_requests WHERE item_id = ? AND user_id IS ?`, itemID, u.userArg()); err != nil {
		return err
	}
	var left int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM compras_requests WHERE item_id = ?`, itemID).Scan(&left); err != nil {
		return err
	}
	if left == 0 && !carried {
		_, err = tx.ExecContext(ctx, `DELETE FROM compras_items WHERE id = ?`, itemID)
	} else {
		err = comprasRecomputeUrgent(ctx, tx, itemID)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func comprasRecomputeUrgent(ctx context.Context, tx *sql.Tx, itemID int64) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE compras_items SET urgent = COALESCE((SELECT MAX(urgent) FROM compras_requests WHERE item_id = ?), 0)
		WHERE id = ?`, itemID, itemID)
	return err
}

// --- Comprar ---

// comprasShopItem valida que el renglón sea de la lista activa.
func comprasShopItem(ctx context.Context, tx *sql.Tx, itemID int64) (supplyID int64, qty float64, err error) {
	err = tx.QueryRowContext(ctx, `
		SELECT i.supply_id, i.quantity FROM compras_items i JOIN compras_lists l ON l.id = i.list_id
		WHERE i.id = ? AND l.status <> 'CLOSED'`, itemID).Scan(&supplyID, &qty)
	if errors.Is(err, sql.ErrNoRows) {
		err = badRequest("Ese renglón ya no está en la lista activa.")
	}
	return
}

// comprasBuy marca un renglón como comprado. Con precio, actualiza el
// precio de referencia del lugar y el historial.
// qty va en las presentaciones del lugar donde se compró (o kg a granel), así
// el precio que se guarda es el de esa presentación.
func (h *POSHandler) comprasBuy(ctx context.Context, u comprasUser, itemID int64, qty float64, totalCents *int64, placeID int64) error {
	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := comprasBuyTx(ctx, tx, u, itemID, qty, totalCents, placeID); err != nil {
		return err
	}
	return tx.Commit()
}

// comprasBuyTx es comprasBuy dentro de una transacción (también la usan los
// tickets).
func comprasBuyTx(ctx context.Context, tx *sql.Tx, u comprasUser, itemID int64, qty float64, totalCents *int64, placeID int64) error {
	supplyID, listQty, err := comprasShopItem(ctx, tx, itemID)
	if err != nil {
		return err
	}
	if qty <= 0 {
		qty = listQty
	}
	var place any
	if placeID > 0 {
		var ok bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM compras_places WHERE id = ?)`, placeID).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return badRequest("Lugar inválido.")
		}
		place = placeID
	}
	var total any
	if totalCents != nil {
		total = *totalCents
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE compras_items SET status = 'BOUGHT', bought_quantity = ?, bought_price_cents = ?, bought_place_id = ?,
		       bought_by = ?, bought_at = CURRENT_TIMESTAMP, resolution_note = ''
		WHERE id = ?`, qty, total, place, u.userArg(), itemID); err != nil {
		return err
	}
	if totalCents != nil && *totalCents > 0 {
		return comprasRecordPrice(ctx, tx, supplyID, placeID, qty, *totalCents)
	}
	return nil
}

// comprasRecordPrice guarda lo que costó un insumo (total ÷ cantidad) en el
// historial y como precio de referencia del lugar.
func comprasRecordPrice(ctx context.Context, tx *sql.Tx, supplyID, placeID int64, qty float64, totalCents int64) error {
	if qty <= 0 || totalCents <= 0 {
		return nil
	}
	var place any
	if placeID > 0 {
		place = placeID
	}
	unit := int64(math.Round(float64(totalCents) / qty))
	if _, err := tx.ExecContext(ctx, `INSERT INTO compras_price_history (supply_id, place_id, price_cents) VALUES (?, ?, ?)`,
		supplyID, place, unit); err != nil {
		return err
	}
	if placeID == 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO compras_sources (supply_id, place_id, price_cents, rank)
		VALUES (?, ?, ?, (SELECT COALESCE(MAX(rank), 0) + 1 FROM compras_sources WHERE supply_id = ?))
		ON CONFLICT (supply_id, place_id) DO UPDATE SET price_cents = excluded.price_cents, updated_at = CURRENT_TIMESTAMP`,
		supplyID, placeID, unit, supplyID)
	return err
}

func (h *POSHandler) comprasSetItem(ctx context.Context, itemID int64, query string, args ...any) error {
	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, _, err := comprasShopItem(ctx, tx, itemID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, query, append(args, itemID)...); err != nil {
		return err
	}
	return tx.Commit()
}

type comprasPlaceGroup struct {
	Name          string
	PlaceID       int64
	SortOrder     int
	Items         []*comprasItem
	EstimateCents int64
	Pending       int
}

// comprasByPlace agrupa la lista por el lugar de siempre de cada insumo.
func comprasByPlace(items []*comprasItem) []*comprasPlaceGroup {
	index := map[int64]*comprasPlaceGroup{}
	var groups []*comprasPlaceGroup
	for _, it := range items {
		var key int64
		name, order := "Sin lugar asignado", 0
		if p := it.Preferred(); p != nil {
			key, name, order = p.PlaceID, p.Place, p.SortOrder
		}
		g, ok := index[key]
		if !ok {
			g = &comprasPlaceGroup{Name: name, PlaceID: key, SortOrder: order}
			index[key] = g
			groups = append(groups, g)
		}
		g.Items = append(g.Items, it)
		if it.Status == "PENDING" {
			g.Pending++
			g.EstimateCents += it.EstimateCents()
		}
	}
	// Orden fijo (el de los lugares y luego el nombre) para que nada brinque
	// mientras se va tachando; "sin lugar" al final.
	sort.SliceStable(groups, func(i, j int) bool {
		a, b := groups[i], groups[j]
		if (a.PlaceID == 0) != (b.PlaceID == 0) {
			return b.PlaceID == 0
		}
		if a.SortOrder != b.SortOrder {
			return a.SortOrder < b.SortOrder
		}
		return comprasKey(a.Name) < comprasKey(b.Name)
	})
	for _, g := range groups {
		sort.SliceStable(g.Items, func(i, j int) bool {
			a, b := g.Items[i], g.Items[j]
			if (a.Status == "PENDING") != (b.Status == "PENDING") {
				return a.Status == "PENDING"
			}
			return a.Urgent && !b.Urgent
		})
	}
	return groups
}

// comprasText es la lista para mandar por WhatsApp.
func comprasText(business string, items []*comprasItem) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🛒 Lista de compras · %s\n", business)
	for _, g := range comprasByPlace(items) {
		if g.Pending == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n📍 %s\n", g.Name)
		for _, it := range g.Items {
			if it.Status != "PENDING" {
				continue
			}
			line := "• " + it.Supply
			if q := it.QtyLabel(); q != "" {
				line += " " + q
			}
			if pack := it.PackLabel(); pack != "" && !it.ByWeight() {
				line += " (" + pack + ")"
			}
			if it.Urgent {
				line += " ‼️ URGE"
			}
			if it.HasEstimate() {
				line += " · ~" + formatCents(it.EstimateCents())
			}
			b.WriteString(line + "\n")
			for _, n := range it.Notes() {
				b.WriteString("   " + n + "\n")
			}
		}
	}
	if t := comprasTotalsOf(items); t.EstimateCents > 0 {
		fmt.Fprintf(&b, "\nPresupuesto estimado: %s\n", formatCents(t.EstimateCents))
	}
	return b.String()
}

// --- Utilidades ---

// comprasKey normaliza un nombre para buscar y evitar duplicados: sin
// acentos, minúsculas y espacios simples.
func comprasKey(s string) string {
	s = comprasAccents.Replace(strings.ToLower(strings.TrimSpace(s)))
	return strings.Join(strings.Fields(s), " ")
}

var comprasAccents = strings.NewReplacer("á", "a", "é", "e", "í", "i", "ó", "o", "ú", "u", "ü", "u", "ñ", "n",
	"à", "a", "è", "e", "ì", "i", "ò", "o", "ù", "u")

// formatCents es "$1,234.50".
func formatCents(cents int64) string {
	neg := cents < 0
	if neg {
		cents = -cents
	}
	pesos := strconv.FormatInt(cents/100, 10)
	for i := len(pesos) - 3; i > 0; i -= 3 {
		pesos = pesos[:i] + "," + pesos[i:]
	}
	s := fmt.Sprintf("$%s.%02d", pesos, cents%100)
	if neg {
		s = "-" + s
	}
	return s
}

// parseCents lee "1,234.50" o "$85". Vacío = sin precio.
func parseCents(s string) (int64, bool, error) {
	s = strings.TrimSpace(strings.NewReplacer("$", "", ",", "", " ", "").Replace(s))
	if s == "" {
		return 0, false, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 || f > 10_000_000 {
		return 0, false, badRequest(fmt.Sprintf("Precio inválido: %q", s))
	}
	return int64(f*100 + 0.5), true, nil
}

func comprasAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "ahora"
	case d < time.Hour:
		return fmt.Sprintf("hace %d min", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("hace %d h", int(d.Hours()))
	case d < 48*time.Hour:
		return "ayer"
	default:
		return fmt.Sprintf("hace %d días", int(d.Hours()/24))
	}
}

var comprasFuncs = template.FuncMap{
	"cents":     formatCents,
	"centsPtr":  func(p *int64) string { return formatCents(*p) },
	"ago":       comprasAgo,
	"searchKey": comprasKey,
	"joinNames": strings.Join,
	"plus":      func(a, b int) int { return a + b },
	"days":      func(t time.Time) int { return int(time.Since(t).Hours() / 24) },
	"localDate": func(t time.Time) string { return t.Local().Format("02/01/2006") },
	// args arma un mapa para pasarle varios datos a un bloque.
	"args": func(kv ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	},
}

// renderBlock ejecuta un bloque de una plantilla (respuesta a htmx).
func renderBlock(w http.ResponseWriter, block string, data any, files ...string) {
	t, err := page(files...)
	if err != nil {
		serverError(w, "Error cargando plantilla", err)
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, block, data); err != nil {
		serverError(w, "Error renderizando plantilla", err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w)
}

func whatsappLink(text string) string { return "https://wa.me/?text=" + url.QueryEscape(text) }
