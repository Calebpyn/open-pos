package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

type AdminTable struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Zone      string `json:"zone"`
	Status    string `json:"status"`
	SortOrder int    `json:"sort_order"`
	IsActive  bool   `json:"is_active"`
	X         int    `json:"x"`
	Y         int    `json:"y"`
	W         int    `json:"w"`
	H         int    `json:"h"`
	Shape     string `json:"shape"`
}

// GET /api/admin/tables
func (h *POSHandler) AdminListTables(w http.ResponseWriter, r *http.Request) {
	rows, err := h.DB.QueryContext(r.Context(), `
		SELECT id, name, COALESCE(zone, ''), status, sort_order, COALESCE(is_active, 1),
		       pos_x, pos_y, width, height, shape
		FROM dining_tables ORDER BY sort_order, id`)
	if err != nil {
		serverError(w, "Error consultando mesas", err)
		return
	}
	defer rows.Close()
	out := []AdminTable{}
	for rows.Next() {
		var t AdminTable
		if err := rows.Scan(&t.ID, &t.Name, &t.Zone, &t.Status, &t.SortOrder, &t.IsActive,
			&t.X, &t.Y, &t.W, &t.H, &t.Shape); err != nil {
			serverError(w, "Error leyendo mesas", err)
			return
		}
		out = append(out, t)
	}
	writeJSON(w, http.StatusOK, out)
}

func decodeTable(w http.ResponseWriter, r *http.Request) (*AdminTable, bool) {
	var t AdminTable
	if err := json.NewDecoder(r.Body).Decode(&t); err != nil {
		http.Error(w, "JSON inválido", http.StatusBadRequest)
		return nil, false
	}
	t.Name, t.Zone = strings.TrimSpace(t.Name), strings.TrimSpace(t.Zone)
	if t.Name == "" {
		http.Error(w, "Escribe el nombre de la mesa", http.StatusBadRequest)
		return nil, false
	}
	if t.Zone == "" {
		t.Zone = "General"
	}
	return &t, true
}

// POST /api/admin/tables
func (h *POSHandler) AdminCreateTable(w http.ResponseWriter, r *http.Request) {
	t, ok := decodeTable(w, r)
	if !ok {
		return
	}
	h.adminWrite(w, r, "TABLE_CREATED", func(tx *sql.Tx) (map[string]any, error) {
		res, err := tx.ExecContext(r.Context(),
			`INSERT INTO dining_tables (name, zone, status, sort_order, is_active) VALUES (?, ?, 'FREE', ?, ?)`,
			t.Name, t.Zone, t.SortOrder, t.IsActive)
		if err != nil {
			return nil, err
		}
		t.ID, _ = res.LastInsertId()
		// Una mesa nueva aparece en el primer lugar libre del plano de su zona.
		if err := placeTable(r.Context(), tx, t.ID, true); err != nil {
			return nil, err
		}
		return map[string]any{"table": t}, nil
	})
}

// PUT /api/admin/tables/{id} - Una mesa ocupada no se puede archivar.
func (h *POSHandler) AdminUpdateTable(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.Error(w, "ID inválido", http.StatusBadRequest)
		return
	}
	t, ok := decodeTable(w, r)
	if !ok {
		return
	}
	t.ID = id
	h.adminWrite(w, r, "TABLE_UPDATED", func(tx *sql.Tx) (map[string]any, error) {
		var open int
		var oldZone string
		var wasActive bool
		err := tx.QueryRowContext(r.Context(), `
			SELECT (SELECT COUNT(*) FROM table_sessions WHERE table_id = dt.id AND status = 'OPEN'),
			       COALESCE(dt.zone, ''), COALESCE(dt.is_active, 1)
			FROM dining_tables dt WHERE dt.id = ?`, id).Scan(&open, &oldZone, &wasActive)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNotFound
		}
		if err != nil {
			return nil, err
		}
		if !t.IsActive && open > 0 {
			return nil, badRequest("La mesa está ocupada; ciérrala antes de archivarla")
		}
		if _, err := tx.ExecContext(r.Context(),
			`UPDATE dining_tables SET name = ?, zone = ?, sort_order = ?, is_active = ? WHERE id = ?`,
			t.Name, t.Zone, t.SortOrder, t.IsActive, id); err != nil {
			return nil, err
		}
		// Al cambiar de zona o reactivarse, su lugar puede estar ocupado.
		if t.Zone != oldZone || (t.IsActive && !wasActive) {
			if err := placeTable(r.Context(), tx, id, t.Zone != oldZone); err != nil {
				return nil, err
			}
		}
		return map[string]any{"table": t}, nil
	})
}

type zoneRequest struct {
	Zone string `json:"zone"`
	To   string `json:"to"`
}

func decodeZone(w http.ResponseWriter, r *http.Request) (*zoneRequest, bool) {
	var z zoneRequest
	if err := json.NewDecoder(r.Body).Decode(&z); err != nil {
		http.Error(w, "JSON inválido", http.StatusBadRequest)
		return nil, false
	}
	z.Zone, z.To = strings.TrimSpace(z.Zone), strings.TrimSpace(z.To)
	if z.Zone == "" {
		http.Error(w, "Indica la zona", http.StatusBadRequest)
		return nil, false
	}
	return &z, true
}

// POST /api/admin/zones/rename - Cambia el nombre de una zona en todas sus
// mesas. Si el nombre nuevo ya existe, las dos zonas se juntan.
func (h *POSHandler) AdminRenameZone(w http.ResponseWriter, r *http.Request) {
	z, ok := decodeZone(w, r)
	if !ok {
		return
	}
	if z.To == "" {
		http.Error(w, "Escribe el nombre nuevo de la zona", http.StatusBadRequest)
		return
	}
	h.adminWrite(w, r, "ZONE_RENAMED", func(tx *sql.Tx) (map[string]any, error) {
		var merging bool
		if err := tx.QueryRowContext(r.Context(),
			`SELECT EXISTS (SELECT 1 FROM dining_tables WHERE zone = ?)`, z.To).Scan(&merging); err != nil {
			return nil, err
		}
		ids, err := tableIDsInZone(r.Context(), tx, z.Zone)
		if err != nil {
			return nil, err
		}
		if len(ids) == 0 {
			return nil, errNotFound
		}
		if _, err := tx.ExecContext(r.Context(), `UPDATE dining_tables SET zone = ? WHERE zone = ?`, z.To, z.Zone); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(r.Context(), `UPDATE floor_items SET zone = ? WHERE zone = ?`, z.To, z.Zone); err != nil {
			return nil, err
		}
		// Al unir dos zonas, sus planos se juntan: lo que choque se reacomoda.
		if merging && z.To != z.Zone {
			for _, id := range ids {
				if err := placeTable(r.Context(), tx, id, false); err != nil {
					return nil, err
				}
			}
			if err := placeZoneItems(r.Context(), tx, z.To); err != nil {
				return nil, err
			}
		}
		n := len(ids)
		return map[string]any{"from": z.Zone, "to": z.To, "tables": n}, nil
	})
}

// POST /api/admin/zones/archive - Archiva todas las mesas de una zona (p. ej.
// una zona que el negocio no tiene). Falla si alguna está ocupada.
func (h *POSHandler) AdminArchiveZone(w http.ResponseWriter, r *http.Request) {
	z, ok := decodeZone(w, r)
	if !ok {
		return
	}
	h.adminWrite(w, r, "ZONE_ARCHIVED", func(tx *sql.Tx) (map[string]any, error) {
		var open int
		if err := tx.QueryRowContext(r.Context(), `
			SELECT COUNT(*) FROM table_sessions ts JOIN dining_tables dt ON dt.id = ts.table_id
			WHERE dt.zone = ? AND ts.status = 'OPEN'`, z.Zone).Scan(&open); err != nil {
			return nil, err
		}
		if open > 0 {
			return nil, badRequest("Hay mesas ocupadas en esta zona; ciérralas antes de archivarla")
		}
		res, err := tx.ExecContext(r.Context(), `UPDATE dining_tables SET is_active = 0 WHERE zone = ? AND is_active = 1`, z.Zone)
		if err != nil {
			return nil, err
		}
		n, _ := res.RowsAffected()
		return map[string]any{"zone": z.Zone, "tables": n}, nil
	})
}

// GET /api/admin/settings
func (h *POSHandler) AdminGetSettings(w http.ResponseWriter, r *http.Request) {
	s, err := loadSettings(r.Context(), h.DB)
	if err != nil {
		serverError(w, "Error cargando configuración", err)
		return
	}
	writeJSON(w, http.StatusOK, s)
}

// PUT /api/admin/settings - El IVA nuevo aplica solo a los cobros siguientes.
func (h *POSHandler) AdminUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var s Settings
	if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
		http.Error(w, "JSON inválido", http.StatusBadRequest)
		return
	}
	s.BusinessName, s.TicketFooter = strings.TrimSpace(s.BusinessName), strings.TrimSpace(s.TicketFooter)
	if s.BusinessName == "" {
		http.Error(w, "Escribe el nombre del negocio", http.StatusBadRequest)
		return
	}
	if s.TaxRate < 0 || s.TaxRate > 1 {
		http.Error(w, "La tasa de IVA debe estar entre 0% y 100%", http.StatusBadRequest)
		return
	}
	if s.StaffDiscountPct < 0 || s.StaffDiscountPct > 100 {
		http.Error(w, "El descuento de colaborador debe estar entre 0% y 100%", http.StatusBadRequest)
		return
	}
	h.adminWrite(w, r, "SETTINGS_UPDATED", func(tx *sql.Tx) (map[string]any, error) {
		before, err := loadSettings(r.Context(), tx)
		if err != nil {
			return nil, err
		}
		includeTax := "0"
		if s.PricesIncludeTax {
			includeTax = "1"
		}
		for k, v := range map[string]string{
			"business_name":      s.BusinessName,
			"tax_rate":           strconv.FormatFloat(s.TaxRate, 'f', -1, 64),
			"prices_include_tax": includeTax,
			"staff_discount_pct": strconv.FormatFloat(s.StaffDiscountPct, 'f', -1, 64),
		} {
			if _, err := tx.ExecContext(r.Context(),
				`INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
				return nil, err
			}
		}
		return map[string]any{"before": before, "after": s}, nil
	})
}

func tableIDsInZone(ctx context.Context, tx *sql.Tx, zone string) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM dining_tables WHERE zone = ? ORDER BY sort_order, id`, zone)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
