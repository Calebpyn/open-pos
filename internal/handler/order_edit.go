package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/calebpyn/open-pos/internal/printer"
)

// --- Editar una orden ya enviada ---
//
// Cambiar un producto que no se ha cobrado (otro producto, modificadores,
// cantidad o nota) o agregar productos a la orden. Lo autoriza caja con su
// PIN y queda en la bitácora. A cocina/barra le llega una comanda normal con
// una línea discreta "(cambio: antes ...)".

type editItemRequest struct {
	PIN         string  `json:"pin"`
	ProductID   int64   `json:"product_id"`
	Quantity    int     `json:"quantity"`
	ModifierIDs []int64 `json:"modifier_ids"`
	Notes       string  `json:"notes"`
}

type itemState struct {
	ProductID int64   `json:"product_id"`
	Product   string  `json:"product"`
	AreaID    int64   `json:"-"`
	Quantity  int     `json:"quantity"`
	Modifiers string  `json:"modifiers"`
	Notes     string  `json:"notes"`
	UnitPrice float64 `json:"unit_price"`
}

func (s itemState) label() string {
	l := fmt.Sprintf("%d x %s", s.Quantity, s.Product)
	if s.Modifiers != "" {
		l += " + " + strings.ReplaceAll(s.Modifiers, modifierSep, ", ")
	}
	return l
}

// catalogLine valida un producto con sus modificadores y regresa su precio
// unitario (base + extras) del catálogo.
func catalogLine(ctx context.Context, q queryer, productID int64, modifierIDs []int64) (name string, areaID, unitCents int64, modsText string, mods []chosenModifier, err error) {
	var base float64
	var area sql.NullInt64
	err = q.QueryRowContext(ctx, `
		SELECT name, price, area_id FROM products WHERE id = ? AND is_available = 1 AND COALESCE(is_active, 1) = 1`,
		productID).Scan(&name, &base, &area)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, 0, "", nil, badRequest("Ese producto no existe o está agotado")
	}
	if err != nil {
		return
	}
	extra, modsText, mods, err := resolveModifiers(ctx, q, productID, modifierIDs)
	if err != nil {
		return
	}
	return name, area.Int64, toCents(base) + extra, modsText, mods, nil
}

func replaceItemModifiers(ctx context.Context, tx *sql.Tx, itemID int64, mods []chosenModifier) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM order_item_modifiers WHERE order_item_id = ?`, itemID); err != nil {
		return err
	}
	for _, m := range mods {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO order_item_modifiers (order_item_id, modifier_option_id, unit_price, name) VALUES (?, ?, ?, ?)`,
			itemID, m.OptionID, fromCents(m.PriceCents), m.Name); err != nil {
			return err
		}
	}
	return nil
}

func sameIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	a, b = append([]int64(nil), a...), append([]int64(nil), b...)
	sort.Slice(a, func(i, j int) bool { return a[i] < a[j] })
	sort.Slice(b, func(i, j int) bool { return b[i] < b[j] })
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// PUT /api/order-items/{id} - Edita un producto de una orden abierta.
func (h *POSHandler) EditOrderItem(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	itemID, ok := pathID(r)
	if !ok {
		http.Error(w, "ID de producto inválido", http.StatusBadRequest)
		return
	}
	var req editItemRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "JSON inválido", http.StatusBadRequest)
		return
	}
	req.Notes = strings.TrimSpace(req.Notes)
	if req.Quantity < 1 || req.Quantity > 99 {
		http.Error(w, "La cantidad debe estar entre 1 y 99", http.StatusBadRequest)
		return
	}

	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		serverError(w, "Error al iniciar transacción", err)
		return
	}
	defer tx.Rollback()
	u, ok := h.cashierByPIN(w, r, tx, req.PIN)
	if !ok {
		return
	}

	var orderID, delivered int64
	var orderStatus, itemStatus, modIDs string
	var billID sql.NullInt64
	var before itemState
	var beforeArea sql.NullInt64
	err = tx.QueryRowContext(ctx, `
		SELECT oi.order_id, o.status, oi.status, oi.bill_id, oi.product_id, p.name, p.area_id, oi.quantity,
		       COALESCE(oi.modifiers_text, ''), COALESCE(oi.notes, ''), oi.unit_price, oi.delivered_quantity,
		       COALESCE((SELECT GROUP_CONCAT(modifier_option_id) FROM order_item_modifiers WHERE order_item_id = oi.id), '')
		FROM order_items oi JOIN orders o ON o.id = oi.order_id JOIN products p ON p.id = oi.product_id
		WHERE oi.id = ?`, itemID).Scan(&orderID, &orderStatus, &itemStatus, &billID, &before.ProductID, &before.Product,
		&beforeArea, &before.Quantity, &before.Modifiers, &before.Notes, &before.UnitPrice, &delivered, &modIDs)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "Producto no encontrado", http.StatusNotFound)
		return
	}
	if err != nil {
		serverError(w, "Error consultando el producto", err)
		return
	}
	before.AreaID = beforeArea.Int64
	switch {
	case orderStatus != "ACTIVE":
		http.Error(w, "La orden ya está cerrada", http.StatusConflict)
		return
	case itemStatus == "VOID":
		http.Error(w, "El producto fue anulado", http.StatusConflict)
		return
	case billID.Valid:
		http.Error(w, "El producto ya se cobró; no se puede editar", http.StatusConflict)
		return
	}
	var beforeMods []int64
	for _, s := range strings.Split(modIDs, ",") {
		var id int64
		if _, err := fmt.Sscan(s, &id); err == nil {
			beforeMods = append(beforeMods, id)
		}
	}

	name, area, unitCents, modsText, mods, err := catalogLine(ctx, tx, req.ProductID, req.ModifierIDs)
	var bad badRequest
	if errors.As(err, &bad) {
		http.Error(w, bad.Error(), http.StatusUnprocessableEntity)
		return
	}
	if err != nil {
		serverError(w, "Error validando el producto", err)
		return
	}
	after := itemState{ProductID: req.ProductID, Product: name, AreaID: area, Quantity: req.Quantity,
		Modifiers: modsText, Notes: req.Notes, UnitPrice: fromCents(unitCents)}

	// Si cambia lo que se prepara, hay que prepararlo de nuevo.
	prepChanged := after.ProductID != before.ProductID || !sameIDs(req.ModifierIDs, beforeMods) || after.Notes != before.Notes
	if !prepChanged && after.Quantity == before.Quantity {
		http.Error(w, "No hay cambios que guardar", http.StatusBadRequest)
		return
	}

	if prepChanged {
		// Se vuelve a imprimir y a entregar completo; el tiempo de salida
		// empieza de nuevo.
		_, err = tx.ExecContext(ctx, `
			UPDATE order_items SET product_id = ?, unit_price = ?, quantity = ?, notes = ?, modifiers_text = ?,
			       printed_quantity = 0, delivered_quantity = 0, delivered_at = NULL, created_at = CURRENT_TIMESTAMP
			WHERE id = ?`, after.ProductID, after.UnitPrice, after.Quantity, nullIfEmpty(after.Notes), nullIfEmpty(after.Modifiers), itemID)
	} else {
		// Solo cambia la cantidad: se conserva lo impreso y lo entregado.
		_, err = tx.ExecContext(ctx, `
			UPDATE order_items SET quantity = ?,
			       printed_quantity = MIN(printed_quantity, ?),
			       delivered_quantity = MIN(delivered_quantity, ?),
			       delivered_at = CASE WHEN MIN(delivered_quantity, ?) >= ? THEN COALESCE(delivered_at, CURRENT_TIMESTAMP) ELSE NULL END
			WHERE id = ?`, after.Quantity, after.Quantity, after.Quantity, after.Quantity, after.Quantity, itemID)
	}
	if err != nil {
		serverError(w, "Error guardando el cambio", err)
		return
	}
	if prepChanged {
		if err := replaceItemModifiers(ctx, tx, itemID, mods); err != nil {
			serverError(w, "Error guardando modificadores", err)
			return
		}
	}
	if err := syncOrderDelivered(ctx, tx, orderID); err != nil {
		serverError(w, "Error actualizando la orden", err)
		return
	}
	if err := auditBy(ctx, tx, u, r, "ITEM_EDITED", map[string]any{
		"order_id": orderID, "item_id": itemID, "before": before, "after": after,
		"product": before.Product + " → " + after.Product,
	}); err != nil {
		serverError(w, "Error registrando bitácora", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "Error guardando el cambio", err)
		return
	}

	// Comanda de la corrección.
	var out PrintOutcome
	switch {
	case prepChanged:
		out = printCorrection(ctx, h.DB, orderID, itemID, before, after)
	case after.Quantity > before.Quantity:
		// Más piezas: se imprimen como adicional, igual que al pedir más.
		out = h.printNewComandas(ctx, orderID)
	default:
		// Menos piezas: se avisa al área.
		gone := before
		gone.Quantity = before.Quantity - after.Quantity
		out = printCorrectionLines(ctx, h.DB, orderID, []comandaLine{{ItemID: itemID, AreaID: before.AreaID,
			Name: before.Product, Quantity: gone.Quantity, Modifiers: before.Modifiers, Removed: true}})
	}
	writeJSON(w, http.StatusOK, map[string]any{"item_id": itemID, "before": before, "after": after, "prints": out})
}

// printCorrection imprime el producto como quedó, con la línea "(cambio:
// antes ...)"; si cambió de área, al área anterior le avisa que ya no va.
func printCorrection(ctx context.Context, db *sql.DB, orderID, itemID int64, before, after itemState) PrintOutcome {
	lines := []comandaLine{{ItemID: itemID, AreaID: after.AreaID, Name: after.Product, Quantity: after.Quantity,
		Modifiers: after.Modifiers, Notes: after.Notes, Before: before.label()}}
	if before.AreaID != after.AreaID {
		lines = append(lines, comandaLine{ItemID: itemID, AreaID: before.AreaID, Name: before.Product,
			Quantity: before.Quantity, Modifiers: before.Modifiers, Removed: true})
	}
	out := printCorrectionLines(ctx, db, orderID, lines)
	if len(out.Failed) == 0 {
		db.ExecContext(ctx, `UPDATE order_items SET printed_quantity = quantity WHERE id = ?`, itemID)
	}
	return out
}

// printCorrectionLines manda líneas sueltas a la impresora de su área, con el
// encabezado normal de la orden.
func printCorrectionLines(ctx context.Context, db *sql.DB, orderID int64, lines []comandaLine) PrintOutcome {
	out := newOutcome()
	// De quién es cada producto (cuentas por persona).
	for i := range lines {
		var pos sql.NullInt64
		var name sql.NullString
		db.QueryRowContext(ctx, `
			SELECT g.position, g.name FROM order_items oi JOIN order_guests g ON g.id = oi.guest_id WHERE oi.id = ?`,
			lines[i].ItemID).Scan(&pos, &name)
		if pos.Valid {
			lines[i].GuestPos, lines[i].Guest = int(pos.Int64), guestLabel(int(pos.Int64), name.String)
		}
	}
	var hdr comandaHeader
	if err := db.QueryRowContext(ctx, `
		SELECT o.id, o.order_type, COALESCE(t.name, ''), COALESCE(o.customer_name, ''), ''
		FROM orders o
		LEFT JOIN table_sessions ts ON ts.id = o.session_id
		LEFT JOIN dining_tables t ON t.id = ts.table_id
		WHERE o.id = ?`, orderID).Scan(&hdr.OrderID, &hdr.OrderType, &hdr.TableName, &hdr.Customer, &hdr.Notes); err != nil {
		out.Failed = append(out.Failed, "No se pudo preparar la comanda: "+err.Error())
		return out
	}
	routing, err := loadRouting(ctx, db)
	if err != nil {
		out.Failed = append(out.Failed, "No se pudo preparar la comanda: "+err.Error())
		return out
	}
	for _, l := range lines {
		if l.AreaID == 0 {
			continue
		}
		var areaName string
		db.QueryRowContext(ctx, `SELECT name FROM production_areas WHERE id = ?`, l.AreaID).Scan(&areaName)
		f, err := loadComandaFormat(ctx, db, l.AreaID)
		if err != nil {
			out.Failed = append(out.Failed, err.Error())
			continue
		}
		f.Copies = 1
		line := l
		for _, p := range routing.forArea(l.AreaID) {
			out.record(p, sendDoc(ctx, p, func(width int) *printer.Ticket {
				return comandaDoc(hdr, []comandaLine{line}, comandaNew, areaName, width, f)
			}))
		}
	}
	return out
}

type addItemsRequest struct {
	PIN   string              `json:"pin"`
	Items []CreateItemRequest `json:"items"`
}

// POST /api/orders/{id}/items - Agrega productos a una orden abierta desde la
// pantalla de la orden (sirve también para las de para llevar).
func (h *POSHandler) AddItemsToOrder(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orderID, ok := pathID(r)
	if !ok {
		http.Error(w, "ID de orden inválido", http.StatusBadRequest)
		return
	}
	var req addItemsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "JSON inválido", http.StatusBadRequest)
		return
	}
	if len(req.Items) == 0 {
		http.Error(w, "Agrega al menos un producto", http.StatusBadRequest)
		return
	}
	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		serverError(w, "Error al iniciar transacción", err)
		return
	}
	defer tx.Rollback()
	u, ok := h.cashierByPIN(w, r, tx, req.PIN)
	if !ok {
		return
	}
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM orders WHERE id = ?`, orderID).Scan(&status); err != nil {
		http.Error(w, "Orden no encontrada", http.StatusNotFound)
		return
	}
	if status != "ACTIVE" {
		http.Error(w, "La orden ya está cerrada", http.StatusConflict)
		return
	}
	var added []string
	for _, it := range req.Items {
		if it.Quantity < 1 || it.Quantity > 99 {
			http.Error(w, "La cantidad debe estar entre 1 y 99", http.StatusBadRequest)
			return
		}
		name, _, unitCents, modsText, mods, err := catalogLine(ctx, tx, it.ProductID, it.ModifierIDs)
		var bad badRequest
		if errors.As(err, &bad) {
			http.Error(w, bad.Error(), http.StatusUnprocessableEntity)
			return
		}
		if err != nil {
			serverError(w, "Error validando el producto", err)
			return
		}
		guest, err := guestFor(ctx, tx, orderID, it.Guest, nil)
		if errors.As(err, &bad) {
			http.Error(w, bad.Error(), http.StatusBadRequest)
			return
		}
		if err != nil {
			serverError(w, "Error guardando la persona", err)
			return
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO order_items (order_id, product_id, unit_price, quantity, printed_quantity, notes, modifiers_text, status, guest_id)
			VALUES (?, ?, ?, ?, 0, ?, ?, 'PENDING', ?)`,
			orderID, it.ProductID, fromCents(unitCents), it.Quantity, nullIfEmpty(strings.TrimSpace(it.Notes)), nullIfEmpty(modsText), guest)
		if err != nil {
			serverError(w, "Error guardando el producto", err)
			return
		}
		id, _ := res.LastInsertId()
		if err := replaceItemModifiers(ctx, tx, id, mods); err != nil {
			serverError(w, "Error guardando modificadores", err)
			return
		}
		added = append(added, fmt.Sprintf("%d x %s", it.Quantity, name))
	}
	// La orden vuelve a tener algo por entregar.
	if err := syncOrderDelivered(ctx, tx, orderID); err != nil {
		serverError(w, "Error actualizando la orden", err)
		return
	}
	if err := auditBy(ctx, tx, u, r, "ORDER_ITEMS_ADDED", map[string]any{
		"order_id": orderID, "items": added, "product": strings.Join(added, ", "),
	}); err != nil {
		serverError(w, "Error registrando bitácora", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "Error guardando", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"order_id": orderID, "prints": h.printNewComandas(ctx, orderID)})
}
