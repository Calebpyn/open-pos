package handler

import (
	"database/sql"
	"encoding/json"
	"net/http"

	"github.com/calebpyn/open-pos/internal/domain"
)

type POSHandler struct {
	DB        *sql.DB
	BackupDir string // carpeta de respaldos de la base
	logins    *loginLimiter
}

func NewPOSHandler(db *sql.DB) *POSHandler {
	return &POSHandler{DB: db, logins: newLoginLimiter()}
}

// GET /api/tables - Obtiene todas las mesas con su estado actual
func (h *POSHandler) GetTables(w http.ResponseWriter, r *http.Request) {
	query := `SELECT id, name, zone, status, sort_order FROM dining_tables WHERE COALESCE(is_active, 1) = 1 ORDER BY sort_order ASC`
	rows, err := h.DB.QueryContext(r.Context(), query)
	if err != nil {
		http.Error(w, "Error al consultar mesas", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var tables []domain.DiningTable
	for rows.Next() {
		var t domain.DiningTable
		if err := rows.Scan(&t.ID, &t.Name, &t.Zone, &t.Status, &t.SortOrder); err != nil {
			http.Error(w, "Error al leer datos", http.StatusInternalServerError)
			return
		}
		tables = append(tables, t)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(tables)
}

// GET /api/menu - Obtiene categorías con sus productos disponibles
func (h *POSHandler) GetMenu(w http.ResponseWriter, r *http.Request) {
	categoriesQuery := `SELECT id, name, color_hex FROM categories WHERE is_active = 1 ORDER BY sort_order ASC`
	catRows, err := h.DB.QueryContext(r.Context(), categoriesQuery)
	if err != nil {
		http.Error(w, "Error al consultar menú", http.StatusInternalServerError)
		return
	}
	defer catRows.Close()

	type MenuCategory struct {
		domain.Category
		Products []domain.Product `json:"products"`
	}

	var menu []MenuCategory

	for catRows.Next() {
		var mc MenuCategory
		if err := catRows.Scan(&mc.ID, &mc.Name, &mc.ColorHex); err != nil {
			continue
		}

		prodQuery := `SELECT id, category_id, name, price, description FROM products WHERE category_id = ? AND is_available = 1 AND COALESCE(is_active, 1) = 1 ORDER BY sort_order ASC`
		pRows, err := h.DB.QueryContext(r.Context(), prodQuery, mc.ID)
		if err == nil {
			for pRows.Next() {
				var p domain.Product
				if err := pRows.Scan(&p.ID, &p.CategoryID, &p.Name, &p.Price, &p.Description); err == nil {
					mc.Products = append(mc.Products, p)
				}
			}
			pRows.Close()
		}

		menu = append(menu, mc)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(menu)
}
