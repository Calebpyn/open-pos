package handler

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
)

type CreateOrderRequest struct {
	TableID    *int64              `json:"table_id"`   // Puntero para permitir null (ventas sin mesa)
	OrderType  string              `json:"order_type"` // DINE_IN, TAKEAWAY, FLASH
	Customer   string              `json:"customer"`   // Nombre/Folio del cliente
	GuestCount int                 `json:"guest_count"`
	Notes      string              `json:"notes"` // Comentario general de la orden
	Items      []CreateItemRequest `json:"items"`
	// ClientRef identifica el envío: si llega dos veces (doble toque,
	// reintento de red) el segundo no duplica productos ni comandas.
	ClientRef string `json:"client_ref"`
	// Cuentas por persona: nombre de cada persona por su número (opcional).
	GuestNames map[int]string `json:"guest_names"`
}

type CreateItemRequest struct {
	ProductID int64 `json:"product_id"`
	// UnitPrice se ignora: el precio siempre se toma del catálogo para que
	// una terminal no pueda registrar un precio distinto.
	UnitPrice float64 `json:"unit_price"`
	Quantity  int     `json:"quantity"`
	Notes     string  `json:"notes"`
	// Opciones de modificadores elegidas (leche, jarabes...). Se validan
	// contra los grupos del producto y su precio sale del catálogo.
	ModifierIDs []int64 `json:"modifier_ids"`
	// Persona de la orden a la que pertenece (0 = la mesa en general).
	Guest int `json:"guest"`
}

var validOrderTypes = map[string]bool{"DINE_IN": true, "TAKEAWAY": true, "FLASH": true}

// POST /api/orders/add - Agrega productos a una mesa o genera venta directa
func (h *POSHandler) AddToOrder(w http.ResponseWriter, r *http.Request) {
	var req CreateOrderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "JSON inválido", http.StatusBadRequest)
		return
	}

	if req.OrderType == "" {
		req.OrderType = "DINE_IN"
	}
	if err := validateOrderRequest(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.OrderType == "FLASH" && isWaiterTerminal(r.Context()) {
		http.Error(w, "La venta de mostrador se hace desde caja", http.StatusForbidden)
		return
	}

	tx, err := h.DB.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "Error al iniciar transacción", err)
		return
	}
	defer tx.Rollback()

	// La transacción toma el candado de escritura, así que un envío repetido
	// espera a que termine el primero y aquí ya lo encuentra.
	if req.ClientRef != "" {
		var prevOrder int64
		err := tx.QueryRowContext(r.Context(), `SELECT order_id FROM order_requests WHERE ref = ?`, req.ClientRef).Scan(&prevOrder)
		if err == nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"success": true, "duplicate": true, "order_id": prevOrder,
				"message": "La comanda ya estaba registrada",
				"prints":  map[string]any{"printed": []string{}, "failed": []string{}},
			})
			return
		}
		if !errors.Is(err, sql.ErrNoRows) {
			serverError(w, "Error consultando el envío", err)
			return
		}
	}

	var sessionID int64

	// 1. Manejo de Sesión según el Tipo de Orden
	if req.OrderType == "DINE_IN" {
		// Orden en Comedor con Mesa
		err = tx.QueryRowContext(r.Context(),
			`SELECT id FROM table_sessions WHERE table_id = ? AND status = 'OPEN'`,
			*req.TableID).Scan(&sessionID)

		if err == sql.ErrNoRows {
			res, err := tx.ExecContext(r.Context(),
				`INSERT INTO table_sessions (table_id, guest_count, status) VALUES (?, ?, 'OPEN')`,
				*req.TableID, req.GuestCount)
			if err != nil {
				serverError(w, "Error creando sesión de mesa", err)
				return
			}
			sessionID, _ = res.LastInsertId()

			_, err = tx.ExecContext(r.Context(),
				`UPDATE dining_tables SET status = 'OCCUPIED' WHERE id = ?`, *req.TableID)
			if err != nil {
				serverError(w, "Error actualizando estado de mesa", err)
				return
			}
		} else if err != nil {
			serverError(w, "Error consultando sesión de mesa", err)
			return
		}
	} else {
		// Venta Flash o Para Llevar (Sin mesa asignada)
		res, err := tx.ExecContext(r.Context(),
			`INSERT INTO table_sessions (table_id, guest_count, status) VALUES (NULL, 1, 'OPEN')`)
		if err != nil {
			serverError(w, "Error creando sesión directa", err)
			return
		}
		sessionID, _ = res.LastInsertId()
	}

	// 2. Buscar o crear Orden Activa
	var orderID int64
	err = tx.QueryRowContext(r.Context(),
		`SELECT id FROM orders WHERE session_id = ? AND status = 'ACTIVE'`,
		sessionID).Scan(&orderID)

	if err == sql.ErrNoRows {
		var customer any
		if req.OrderType != "DINE_IN" && req.Customer != "" {
			customer = req.Customer
		}
		res, err := tx.ExecContext(r.Context(),
			`INSERT INTO orders (session_id, order_type, customer_name, notes, status, user_id) VALUES (?, ?, ?, ?, 'ACTIVE', ?)`,
			sessionID, req.OrderType, customer, req.Notes, terminalUser(r.Context()))
		if err != nil {
			serverError(w, "Error creando orden", err)
			return
		}
		orderID, _ = res.LastInsertId()
	} else if err != nil {
		serverError(w, "Error consultando orden activa", err)
		return
	} else if note := strings.TrimSpace(req.Notes); note != "" {
		// Comanda adicional sobre una orden abierta: su nota se suma a las anteriores.
		if _, err := tx.ExecContext(r.Context(), `
			UPDATE orders SET notes = CASE WHEN COALESCE(notes, '') = '' THEN ? ELSE notes || ' | ' || ? END,
			                  updated_at = CURRENT_TIMESTAMP
			WHERE id = ?`, note, note, orderID); err != nil {
			serverError(w, "Error guardando nota de la orden", err)
			return
		}
	}

	// 3. Insertar los ítems con el precio vigente del catálogo. El precio de
	// la línea incluye los extras de sus modificadores.
	for _, item := range req.Items {
		var basePrice float64
		err := tx.QueryRowContext(r.Context(), `
			SELECT price FROM products WHERE id = ? AND is_available = 1 AND COALESCE(is_active, 1) = 1`,
			item.ProductID).Scan(&basePrice)
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, fmt.Sprintf("El producto %d no existe o no está disponible", item.ProductID), http.StatusUnprocessableEntity)
			return
		}
		if err != nil {
			serverError(w, "Error consultando producto", err)
			return
		}
		extra, modsText, mods, err := resolveModifiers(r.Context(), tx, item.ProductID, item.ModifierIDs)
		var bad badRequest
		if errors.As(err, &bad) {
			http.Error(w, bad.Error(), http.StatusUnprocessableEntity)
			return
		}
		if err != nil {
			serverError(w, "Error validando modificadores", err)
			return
		}
		if req.OrderType == "FLASH" {
			item.Guest = 0 // la venta de mostrador no se divide por persona
		}
		guest, err := guestFor(r.Context(), tx, orderID, item.Guest, req.GuestNames)
		if errors.As(err, &bad) {
			http.Error(w, bad.Error(), http.StatusBadRequest)
			return
		}
		if err != nil {
			serverError(w, "Error guardando la persona", err)
			return
		}
		res, err := tx.ExecContext(r.Context(), `
			INSERT INTO order_items (order_id, product_id, unit_price, quantity, printed_quantity, notes, modifiers_text, status, guest_id, added_by)
			VALUES (?, ?, ?, ?, 0, ?, ?, 'PENDING', ?, ?)`,
			orderID, item.ProductID, fromCents(toCents(basePrice)+extra), item.Quantity, item.Notes, nullIfEmpty(modsText), guest,
			terminalUser(r.Context()))
		if err != nil {
			serverError(w, "Error guardando ítem", err)
			return
		}
		itemID, _ := res.LastInsertId()
		for _, m := range mods {
			if _, err := tx.ExecContext(r.Context(), `
				INSERT INTO order_item_modifiers (order_item_id, modifier_option_id, unit_price, name) VALUES (?, ?, ?, ?)`,
				itemID, m.OptionID, fromCents(m.PriceCents), m.Name); err != nil {
				serverError(w, "Error guardando modificadores", err)
				return
			}
		}
	}

	if req.ClientRef != "" {
		if _, err := tx.ExecContext(r.Context(),
			`INSERT INTO order_requests (ref, order_id) VALUES (?, ?)`, req.ClientRef, orderID); err != nil {
			serverError(w, "Error registrando el envío", err)
			return
		}
	}

	if err := tx.Commit(); err != nil {
		serverError(w, "Error al confirmar transacción", err)
		return
	}

	// La orden ya quedó registrada; si la impresión falla se reporta y se
	// puede reintentar con "Re-Imprimir" en Expo.
	prints := h.printNewComandas(r.Context(), orderID)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]any{
		"success":    true,
		"order_id":   orderID,
		"session_id": sessionID,
		"message":    "Comanda registrada con éxito",
		"prints":     prints,
	})
}

func validateOrderRequest(req *CreateOrderRequest) error {
	req.Customer = strings.TrimSpace(req.Customer)

	if !validOrderTypes[req.OrderType] {
		return fmt.Errorf("Tipo de orden inválido: %q", req.OrderType)
	}
	if req.OrderType == "DINE_IN" && req.TableID == nil {
		return fmt.Errorf("Las órdenes en mesa requieren table_id")
	}
	if req.OrderType == "TAKEAWAY" && req.Customer == "" {
		return fmt.Errorf("Las órdenes para llevar requieren nombre del cliente")
	}
	if len(req.Items) == 0 {
		return fmt.Errorf("La orden no tiene productos")
	}
	if len(req.ClientRef) > 64 {
		return fmt.Errorf("Identificador de envío inválido")
	}
	for _, item := range req.Items {
		if item.Quantity <= 0 {
			return fmt.Errorf("Cantidad inválida para el producto %d", item.ProductID)
		}
	}
	if req.GuestCount <= 0 {
		req.GuestCount = 1
	}
	return nil
}

// serverError registra el error real en el log y responde un mensaje genérico.
func serverError(w http.ResponseWriter, msg string, err error) {
	log.Printf("%s: %v", msg, err)
	http.Error(w, msg, http.StatusInternalServerError)
}
