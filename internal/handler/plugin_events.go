package handler

import (
	"context"
	"database/sql"
	"strings"

	"github.com/calebpyn/open-pos/internal/plugins"
)

// --- Eventos para plugins ---
//
// Cada operación que cambia lo vendido deja un evento en plugin_events dentro
// de su misma transacción (ver internal/plugins). El contrato de cada evento
// está en docs/plugins.md: si cambias un payload, actualízalo ahí y en sdk/.

// eventItem es un producto de una orden tal como lo ve un plugin.
type eventItem struct {
	ItemID    int64           `json:"item_id"`
	OrderID   int64           `json:"order_id"`
	ProductID int64           `json:"product_id"`
	Product   string          `json:"product"`
	Quantity  int             `json:"quantity"`
	UnitPrice float64         `json:"unit_price"` // incluye los extras de modificadores
	Status    string          `json:"status"`
	Notes     string          `json:"notes"`
	Modifiers []eventModifier `json:"modifiers"`
}

type eventModifier struct {
	OptionID int64  `json:"option_id"`
	Name     string `json:"name"`
}

// eventItems carga los productos de la orden que cumplen where (sobre
// order_items oi), con sus modificadores.
func eventItems(ctx context.Context, tx *sql.Tx, where string, args ...any) ([]eventItem, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT oi.id, oi.order_id, oi.product_id, p.name, oi.quantity, oi.unit_price, oi.status, COALESCE(oi.notes, '')
		FROM order_items oi JOIN products p ON p.id = oi.product_id
		WHERE `+where+` ORDER BY oi.id`, args...)
	if err != nil {
		return nil, err
	}
	items := []eventItem{}
	for rows.Next() {
		var it eventItem
		if err := rows.Scan(&it.ItemID, &it.OrderID, &it.ProductID, &it.Product, &it.Quantity, &it.UnitPrice,
			&it.Status, &it.Notes); err != nil {
			rows.Close()
			return nil, err
		}
		it.Modifiers = []eventModifier{}
		items = append(items, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range items {
		mods, err := tx.QueryContext(ctx, `
			SELECT COALESCE(modifier_option_id, 0), COALESCE(name, '') FROM order_item_modifiers
			WHERE order_item_id = ? ORDER BY id`, items[i].ItemID)
		if err != nil {
			return nil, err
		}
		for mods.Next() {
			var m eventModifier
			if err := mods.Scan(&m.OptionID, &m.Name); err != nil {
				mods.Close()
				return nil, err
			}
			items[i].Modifiers = append(items[i].Modifiers, m)
		}
		mods.Close()
		if err := mods.Err(); err != nil {
			return nil, err
		}
	}
	return items, nil
}

// eventItemByID carga un solo producto.
func eventItemByID(ctx context.Context, tx *sql.Tx, id int64) (eventItem, error) {
	items, err := eventItems(ctx, tx, `oi.id = ?`, id)
	if err != nil {
		return eventItem{}, err
	}
	if len(items) == 0 {
		return eventItem{}, sql.ErrNoRows
	}
	return items[0], nil
}

// emitItemsAdded registra los productos recién agregados a una orden.
func emitItemsAdded(ctx context.Context, tx *sql.Tx, orderID int64, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	items, err := eventItems(ctx, tx, `oi.id IN (`+ph+`)`, args...)
	if err != nil {
		return err
	}
	var orderType string
	if err := tx.QueryRowContext(ctx, `SELECT order_type FROM orders WHERE id = ?`, orderID).Scan(&orderType); err != nil {
		return err
	}
	return plugins.Emit(ctx, tx, "items.added", map[string]any{
		"order_id": orderID, "order_type": orderType, "user_id": terminalUser(ctx), "items": items,
	})
}

// Acciones del panel que cambian el catálogo: un plugin (p. ej. inventario)
// vuelve a leerlo con /api/plugin/v1/catalog.
var catalogActions = map[string]bool{
	"CATEGORY_CREATED": true, "CATEGORY_UPDATED": true,
	"PRODUCT_CREATED": true, "PRODUCT_UPDATED": true, "PRODUCT_AVAILABILITY": true,
	"MODIFIER_GROUP_CREATED": true, "MODIFIER_GROUP_UPDATED": true,
	"AREA_CREATED": true, "AREA_UPDATED": true,
}

func emitCatalogChanged(ctx context.Context, tx *sql.Tx, action string, details map[string]any) error {
	if !catalogActions[action] {
		return nil
	}
	return plugins.Emit(ctx, tx, "catalog.changed", map[string]any{"action": action, "details": details})
}

// emitBillPaid registra una cuenta cobrada con sus productos y pagos.
func emitBillPaid(ctx context.Context, tx *sql.Tx, orderID, billID int64, billNumber int, totalCents, tipCents int64) error {
	items, err := eventItems(ctx, tx, `oi.bill_id = ?`, billID)
	if err != nil {
		return err
	}
	type payment struct {
		Method string  `json:"method"`
		Amount float64 `json:"amount"`
	}
	payments := []payment{}
	rows, err := tx.QueryContext(ctx, `SELECT payment_method, amount FROM payments WHERE bill_id = ? ORDER BY id`, billID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var p payment
		if err := rows.Scan(&p.Method, &p.Amount); err != nil {
			rows.Close()
			return err
		}
		payments = append(payments, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	return plugins.Emit(ctx, tx, "bill.paid", map[string]any{
		"order_id": orderID, "bill_id": billID, "bill_number": billNumber,
		"total": fromCents(totalCents), "tip": fromCents(tipCents),
		"items": items, "payments": payments, "user_id": terminalUser(ctx),
	})
}
