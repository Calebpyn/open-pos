package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// --- Cuentas por persona ---
//
// Una orden se puede dividir desde la toma en personas (1, 2, 3...), cada una
// con nombre opcional. Cada producto es de una persona o de la mesa en general
// (guest_id NULL, "Mesa"). Sirve para cobrar a cada quien lo suyo y para que
// la comanda y Expo digan de quién es cada platillo.

type Guest struct {
	ID       int64  `json:"id"`
	Position int    `json:"position"`
	Name     string `json:"name"`
	Label    string `json:"label"`
}

// guestLabel es como se muestra una persona: su nombre o "Persona N".
func guestLabel(position int, name string) string {
	if name = strings.TrimSpace(name); name != "" {
		return name
	}
	if position <= 0 {
		return "Mesa"
	}
	return fmt.Sprintf("Persona %d", position)
}

const maxGuests = 40

// ensureGuest regresa el id de la persona número position de la orden (la
// crea si no existe). Un nombre no vacío reemplaza al anterior.
func ensureGuest(ctx context.Context, tx *sql.Tx, orderID int64, position int, name string) (int64, error) {
	if position < 1 || position > maxGuests {
		return 0, badRequest(fmt.Sprintf("Número de persona inválido (1 a %d)", maxGuests))
	}
	name = strings.TrimSpace(name)
	if len([]rune(name)) > 30 {
		name = string([]rune(name)[:30])
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO order_guests (order_id, position, name) VALUES (?, ?, ?)
		ON CONFLICT(order_id, position) DO UPDATE SET name = CASE WHEN excluded.name != '' THEN excluded.name ELSE name END`,
		orderID, position, name); err != nil {
		return 0, err
	}
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM order_guests WHERE order_id = ? AND position = ?`, orderID, position).Scan(&id)
	return id, err
}

// guestFor resuelve la persona de un producto: nil si es de la mesa.
func guestFor(ctx context.Context, tx *sql.Tx, orderID int64, position int, names map[int]string) (any, error) {
	if position <= 0 {
		return nil, nil
	}
	return ensureGuest(ctx, tx, orderID, position, names[position])
}

func loadGuests(ctx context.Context, q queryer, orderID int64) ([]Guest, error) {
	out := []Guest{}
	err := eachRow(ctx, q, `SELECT id, position, name FROM order_guests WHERE order_id = ? ORDER BY position`,
		[]any{orderID}, func(scan func(...any) error) error {
			var g Guest
			if err := scan(&g.ID, &g.Position, &g.Name); err != nil {
				return err
			}
			g.Label = guestLabel(g.Position, g.Name)
			out = append(out, g)
			return nil
		})
	return out, err
}

// GET /api/tables/{id}/guests - Personas de la orden abierta de una mesa, para
// seguir agregando a cada quien en la siguiente ronda.
func (h *POSHandler) TableGuests(w http.ResponseWriter, r *http.Request) {
	tableID, ok := pathID(r)
	if !ok {
		http.Error(w, "ID de mesa inválido", http.StatusBadRequest)
		return
	}
	var orderID int64
	err := h.DB.QueryRowContext(r.Context(), `
		SELECT o.id FROM orders o JOIN table_sessions ts ON ts.id = o.session_id
		WHERE ts.table_id = ? AND ts.status = 'OPEN' AND o.status = 'ACTIVE'
		ORDER BY o.id DESC LIMIT 1`, tableID).Scan(&orderID)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusOK, map[string]any{"order_id": nil, "guests": []Guest{}})
		return
	}
	if err != nil {
		serverError(w, "Error consultando la mesa", err)
		return
	}
	guests, err := loadGuests(r.Context(), h.DB, orderID)
	if err != nil {
		serverError(w, "Error consultando personas", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"order_id": orderID, "guests": guests})
}

type moveItemRequest struct {
	Position int    `json:"position"` // 0 = mesa
	Name     string `json:"name"`     // para una persona nueva
	Quantity int    `json:"quantity"` // piezas a mover (0 = todas)
}

// PUT /api/order-items/{id}/guest - Pasa un producto (o algunas de sus piezas)
// a otra persona. No cambia montos, solo a quién le toca: no pide PIN.
func (h *POSHandler) MoveItemToGuest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	itemID, ok := pathID(r)
	if !ok {
		http.Error(w, "ID de producto inválido", http.StatusBadRequest)
		return
	}
	var req moveItemRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "JSON inválido", http.StatusBadRequest)
		return
	}
	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		serverError(w, "Error al iniciar transacción", err)
		return
	}
	defer tx.Rollback()

	var orderID, qty int64
	var status, itemStatus string
	var billID sql.NullInt64
	err = tx.QueryRowContext(ctx, `
		SELECT oi.order_id, o.status, oi.status, oi.bill_id, oi.quantity
		FROM order_items oi JOIN orders o ON o.id = oi.order_id WHERE oi.id = ?`, itemID).
		Scan(&orderID, &status, &itemStatus, &billID, &qty)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "Producto no encontrado", http.StatusNotFound)
		return
	}
	if err != nil {
		serverError(w, "Error consultando el producto", err)
		return
	}
	if status != "ACTIVE" || itemStatus == "VOID" || billID.Valid {
		http.Error(w, "Solo se mueven productos sin cobrar de una orden abierta", http.StatusConflict)
		return
	}
	var guest any
	if req.Position > 0 {
		id, err := ensureGuest(ctx, tx, orderID, req.Position, req.Name)
		var bad badRequest
		if errors.As(err, &bad) {
			http.Error(w, bad.Error(), http.StatusBadRequest)
			return
		}
		if err != nil {
			serverError(w, "Error guardando la persona", err)
			return
		}
		guest = id
	}
	id := itemID
	if req.Quantity > 0 && int64(req.Quantity) < qty {
		// Solo algunas piezas: el renglón se parte y se mueve la parte nueva.
		if id, err = splitOrderItem(ctx, tx, itemID, int64(req.Quantity)); err != nil {
			serverError(w, "Error separando piezas", err)
			return
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE order_items SET guest_id = ? WHERE id = ?`, guest, id); err != nil {
		serverError(w, "Error moviendo el producto", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "Error moviendo el producto", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"item_id": id})
}

// billGuestLabel es la persona de una cuenta si todos sus productos son de
// ella ("Cuenta de Juan"); vacío si es de la mesa o mezcla.
func billGuestLabel(ctx context.Context, q queryer, where string, args ...any) string {
	var n int
	var pos sql.NullInt64
	var name sql.NullString
	err := q.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT COALESCE(oi.guest_id, 0)), MAX(g.position), MAX(g.name)
		FROM order_items oi LEFT JOIN order_guests g ON g.id = oi.guest_id
		WHERE `+where, args...).Scan(&n, &pos, &name)
	if err != nil || n != 1 || !pos.Valid {
		return ""
	}
	return guestLabel(int(pos.Int64), name.String)
}
