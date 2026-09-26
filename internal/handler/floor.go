package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// --- Plano del salón ---
//
// Cada zona es una cuadrícula. Las mesas y los elementos fijos (paredes,
// barra, puerta, textos) ocupan celdas completas y no se enciman.

const (
	floorMaxCols  = 60 // tamaño máximo del plano, en celdas
	floorMaxRows  = 40
	floorMaxSize  = 12 // lado máximo de un elemento
	floorAutoCols = 16 // ancho en que se acomodan las mesas nuevas
)

var floorKinds = map[string]string{
	"WALL":    "",
	"COUNTER": "Barra",
	"DOOR":    "Entrada",
	"LABEL":   "Texto",
}

type rect struct{ X, Y, W, H int }

func (a rect) overlaps(b rect) bool {
	return a.X < b.X+b.W && b.X < a.X+a.W && a.Y < b.Y+b.H && b.Y < a.Y+a.H
}

func (a rect) grow(n int) rect { return rect{a.X - n, a.Y - n, a.W + 2*n, a.H + 2*n} }

// valid indica si el rectángulo cabe en el plano.
func (a rect) valid() bool {
	return a.X >= 0 && a.Y >= 0 && a.W >= 1 && a.H >= 1 && a.W <= floorMaxSize && a.H <= floorMaxSize &&
		a.X+a.W <= floorMaxCols && a.Y+a.H <= floorMaxRows
}

func collides(r rect, others []rect) bool {
	for _, o := range others {
		if r.overlaps(o) {
			return true
		}
	}
	return false
}

// occupiedCells regresa lo que ya ocupa espacio en la zona: mesas activas y
// elementos fijos, sin contar la mesa o el elemento que se está moviendo.
func occupiedCells(ctx context.Context, q queryer, zone string, skipTable, skipItem int64) ([]rect, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT pos_x, pos_y, width, height FROM dining_tables
		WHERE zone = ?1 AND COALESCE(is_active, 1) = 1 AND id != ?2
		UNION ALL
		SELECT pos_x, pos_y, width, height FROM floor_items WHERE zone = ?1 AND id != ?3`,
		zone, skipTable, skipItem)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []rect
	for rows.Next() {
		var r rect
		if err := rows.Scan(&r.X, &r.Y, &r.W, &r.H); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// freeSpot busca el primer lugar libre de w×h, de arriba abajo y de izquierda
// a derecha. Prefiere dejar una celda de pasillo alrededor.
func freeSpot(occ []rect, w, h int) (int, int, bool) {
	for _, margin := range []int{1, 0} {
		for y := 0; y+h <= floorMaxRows; y++ {
			for x := 0; x+w <= floorAutoCols; x++ {
				r := rect{x, y, w, h}
				if margin > 0 && (x == 0 || y == 0) {
					continue // pegado a la orilla no deja pasillo
				}
				if !collides(r.grow(margin), occ) && !collides(r, occ) {
					return x, y, true
				}
			}
		}
	}
	return 0, 0, false
}

var errFloorFull = badRequest("Ya no hay espacio libre en el plano de esta zona")

// placeTable mueve la mesa a un lugar libre si su posición actual choca con
// otra cosa (mesa nueva, cambio de zona, zonas unidas, mesa reactivada).
func placeTable(ctx context.Context, tx *sql.Tx, id int64, force bool) error {
	var zone string
	var r rect
	var active bool
	if err := tx.QueryRowContext(ctx, `
		SELECT zone, pos_x, pos_y, width, height, COALESCE(is_active, 1) FROM dining_tables WHERE id = ?`, id).
		Scan(&zone, &r.X, &r.Y, &r.W, &r.H, &active); err != nil {
		return err
	}
	if !active {
		return nil
	}
	occ, err := occupiedCells(ctx, tx, zone, id, 0)
	if err != nil {
		return err
	}
	if !force && r.valid() && !collides(r, occ) {
		return nil
	}
	x, y, ok := freeSpot(occ, r.W, r.H)
	if !ok {
		return errFloorFull
	}
	_, err = tx.ExecContext(ctx, `UPDATE dining_tables SET pos_x = ?, pos_y = ? WHERE id = ?`, x, y, id)
	return err
}

// placeZoneItems acomoda los elementos fijos de una zona que chocan (al unir
// dos zonas).
func placeZoneItems(ctx context.Context, tx *sql.Tx, zone string) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, pos_x, pos_y, width, height FROM floor_items WHERE zone = ? ORDER BY id`, zone)
	if err != nil {
		return err
	}
	type item struct {
		id int64
		r  rect
	}
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.r.X, &it.r.Y, &it.r.W, &it.r.H); err != nil {
			rows.Close()
			return err
		}
		items = append(items, it)
	}
	rows.Close()
	for _, it := range items {
		occ, err := occupiedCells(ctx, tx, zone, 0, it.id)
		if err != nil {
			return err
		}
		if !collides(it.r, occ) {
			continue
		}
		x, y, ok := freeSpot(occ, it.r.W, it.r.H)
		if !ok {
			return errFloorFull
		}
		if _, err := tx.ExecContext(ctx, `UPDATE floor_items SET pos_x = ?, pos_y = ? WHERE id = ?`, x, y, it.id); err != nil {
			return err
		}
	}
	return nil
}

// --- POS ---

type FloorTable struct {
	ID      int64   `json:"id"`
	Name    string  `json:"name"`
	Status  string  `json:"status"`
	X       int     `json:"x"`
	Y       int     `json:"y"`
	W       int     `json:"w"`
	H       int     `json:"h"`
	Shape   string  `json:"shape"`
	Minutes int     `json:"minutes"` // tiempo desde que se ocupó
	Total   float64 `json:"total"`   // consumo pendiente de cobro
}

type FloorItem struct {
	ID    int64  `json:"id"`
	Zone  string `json:"zone"`
	Kind  string `json:"kind"`
	Label string `json:"label"`
	X     int    `json:"x"`
	Y     int    `json:"y"`
	W     int    `json:"w"`
	H     int    `json:"h"`
}

type FloorZone struct {
	Name   string       `json:"name"`
	Tables []FloorTable `json:"tables"`
	Items  []FloorItem  `json:"items"`
}

// GET /api/tables/floor - Mesas activas por zona con su lugar en el plano,
// estado, tiempo ocupada y consumo, más los elementos fijos de cada zona.
func (h *POSHandler) GetFloor(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, err := h.DB.QueryContext(ctx, `
		SELECT dt.id, dt.name, COALESCE(dt.zone, 'General'), dt.status,
		       dt.pos_x, dt.pos_y, dt.width, dt.height, dt.shape,
		       COALESCE(CAST((julianday('now') - julianday(ts.opened_at)) * 1440 AS INTEGER), 0),
		       COALESCE((SELECT ROUND(SUM(oi.unit_price * oi.quantity), 2)
		                 FROM orders o JOIN order_items oi ON oi.order_id = o.id
		                 LEFT JOIN bills b ON b.id = oi.bill_id
		                 WHERE o.session_id = ts.id AND o.status = 'ACTIVE' AND oi.status != 'VOID'
		                   AND COALESCE(b.status, '') != 'PAID'), 0)
		FROM dining_tables dt
		LEFT JOIN table_sessions ts ON ts.table_id = dt.id AND ts.status = 'OPEN'
		WHERE COALESCE(dt.is_active, 1) = 1
		ORDER BY dt.sort_order, dt.id`)
	if err != nil {
		serverError(w, "Error consultando mesas", err)
		return
	}
	defer rows.Close()
	zones := []*FloorZone{}
	byName := map[string]*FloorZone{}
	for rows.Next() {
		var t FloorTable
		var zone string
		if err := rows.Scan(&t.ID, &t.Name, &zone, &t.Status, &t.X, &t.Y, &t.W, &t.H, &t.Shape, &t.Minutes, &t.Total); err != nil {
			serverError(w, "Error leyendo mesas", err)
			return
		}
		if t.Status != "OCCUPIED" {
			t.Minutes, t.Total = 0, 0
		}
		z := byName[zone]
		if z == nil {
			z = &FloorZone{Name: zone, Tables: []FloorTable{}, Items: []FloorItem{}}
			byName[zone] = z
			zones = append(zones, z)
		}
		z.Tables = append(z.Tables, t)
	}
	if err := rows.Err(); err != nil {
		serverError(w, "Error leyendo mesas", err)
		return
	}
	items, err := listFloorItems(ctx, h.DB)
	if err != nil {
		serverError(w, "Error consultando el plano", err)
		return
	}
	for _, it := range items {
		if z := byName[it.Zone]; z != nil {
			z.Items = append(z.Items, it)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"zones": zones})
}

func listFloorItems(ctx context.Context, q queryer) ([]FloorItem, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, zone, kind, label, pos_x, pos_y, width, height FROM floor_items ORDER BY zone, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FloorItem{}
	for rows.Next() {
		var it FloorItem
		if err := rows.Scan(&it.ID, &it.Zone, &it.Kind, &it.Label, &it.X, &it.Y, &it.W, &it.H); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// --- Editor (admin) ---

type layoutRequest struct {
	X     int    `json:"x"`
	Y     int    `json:"y"`
	W     int    `json:"w"`
	H     int    `json:"h"`
	Shape string `json:"shape"`
}

func decodeLayout(w http.ResponseWriter, r *http.Request) (*layoutRequest, bool) {
	var l layoutRequest
	if err := json.NewDecoder(r.Body).Decode(&l); err != nil {
		http.Error(w, "JSON inválido", http.StatusBadRequest)
		return nil, false
	}
	if !(rect{l.X, l.Y, l.W, l.H}).valid() {
		http.Error(w, "La posición queda fuera del plano", http.StatusBadRequest)
		return nil, false
	}
	return &l, true
}

var errFloorCollision = badRequest("Ese lugar ya está ocupado")

// PUT /api/admin/tables/{id}/layout - Mueve o cambia el tamaño o la forma de
// una mesa en el plano. Mover mesas no se registra en la bitácora.
func (h *POSHandler) AdminMoveTable(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.Error(w, "ID inválido", http.StatusBadRequest)
		return
	}
	l, ok := decodeLayout(w, r)
	if !ok {
		return
	}
	if l.Shape != "ROUND" {
		l.Shape = "SQUARE"
	}
	h.adminWrite(w, r, "", func(tx *sql.Tx) (map[string]any, error) {
		var zone string
		err := tx.QueryRowContext(r.Context(), `SELECT zone FROM dining_tables WHERE id = ?`, id).Scan(&zone)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNotFound
		}
		if err != nil {
			return nil, err
		}
		occ, err := occupiedCells(r.Context(), tx, zone, id, 0)
		if err != nil {
			return nil, err
		}
		if collides(rect{l.X, l.Y, l.W, l.H}, occ) {
			return nil, errFloorCollision
		}
		if _, err := tx.ExecContext(r.Context(),
			`UPDATE dining_tables SET pos_x = ?, pos_y = ?, width = ?, height = ?, shape = ? WHERE id = ?`,
			l.X, l.Y, l.W, l.H, l.Shape, id); err != nil {
			return nil, err
		}
		return map[string]any{"id": id, "layout": l}, nil
	})
}

// GET /api/admin/floor-items
func (h *POSHandler) AdminListFloorItems(w http.ResponseWriter, r *http.Request) {
	items, err := listFloorItems(r.Context(), h.DB)
	if err != nil {
		serverError(w, "Error consultando el plano", err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func decodeFloorItem(w http.ResponseWriter, r *http.Request) (*FloorItem, bool) {
	var it FloorItem
	if err := json.NewDecoder(r.Body).Decode(&it); err != nil {
		http.Error(w, "JSON inválido", http.StatusBadRequest)
		return nil, false
	}
	it.Zone, it.Label = strings.TrimSpace(it.Zone), strings.TrimSpace(it.Label)
	if _, ok := floorKinds[it.Kind]; !ok {
		http.Error(w, "Tipo de elemento inválido", http.StatusBadRequest)
		return nil, false
	}
	if it.Zone == "" {
		http.Error(w, "Indica la zona", http.StatusBadRequest)
		return nil, false
	}
	if len([]rune(it.Label)) > 40 {
		http.Error(w, "El texto es muy largo (máximo 40 caracteres)", http.StatusBadRequest)
		return nil, false
	}
	return &it, true
}

// POST /api/admin/floor-items - Agrega una pared, barra, puerta o texto en
// el primer lugar libre de la zona.
func (h *POSHandler) AdminCreateFloorItem(w http.ResponseWriter, r *http.Request) {
	it, ok := decodeFloorItem(w, r)
	if !ok {
		return
	}
	if it.Label == "" {
		it.Label = floorKinds[it.Kind]
	}
	if !(rect{0, 0, it.W, it.H}).valid() {
		it.W, it.H = 3, 1
		if it.Kind == "LABEL" {
			it.W = 4
		}
	}
	h.adminWrite(w, r, "", func(tx *sql.Tx) (map[string]any, error) {
		occ, err := occupiedCells(r.Context(), tx, it.Zone, 0, 0)
		if err != nil {
			return nil, err
		}
		x, y, ok := freeSpot(occ, it.W, it.H)
		if !ok {
			return nil, errFloorFull
		}
		it.X, it.Y = x, y
		res, err := tx.ExecContext(r.Context(), `
			INSERT INTO floor_items (zone, kind, label, pos_x, pos_y, width, height) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			it.Zone, it.Kind, it.Label, it.X, it.Y, it.W, it.H)
		if err != nil {
			return nil, err
		}
		it.ID, _ = res.LastInsertId()
		return map[string]any{"item": it}, nil
	})
}

// PUT /api/admin/floor-items/{id} - Mueve, cambia de tamaño o de texto.
func (h *POSHandler) AdminUpdateFloorItem(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.Error(w, "ID inválido", http.StatusBadRequest)
		return
	}
	it, ok := decodeFloorItem(w, r)
	if !ok {
		return
	}
	it.ID = id
	if !(rect{it.X, it.Y, it.W, it.H}).valid() {
		http.Error(w, "La posición queda fuera del plano", http.StatusBadRequest)
		return
	}
	h.adminWrite(w, r, "", func(tx *sql.Tx) (map[string]any, error) {
		occ, err := occupiedCells(r.Context(), tx, it.Zone, 0, id)
		if err != nil {
			return nil, err
		}
		if collides(rect{it.X, it.Y, it.W, it.H}, occ) {
			return nil, errFloorCollision
		}
		res, err := tx.ExecContext(r.Context(), `
			UPDATE floor_items SET kind = ?, label = ?, pos_x = ?, pos_y = ?, width = ?, height = ?
			WHERE id = ? AND zone = ?`, it.Kind, it.Label, it.X, it.Y, it.W, it.H, id, it.Zone)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil, errNotFound
		}
		return map[string]any{"item": it}, nil
	})
}

// DELETE /api/admin/floor-items/{id} - Los elementos fijos son solo dibujo:
// se borran sin afectar el historial.
func (h *POSHandler) AdminDeleteFloorItem(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.Error(w, "ID inválido", http.StatusBadRequest)
		return
	}
	h.adminWrite(w, r, "", func(tx *sql.Tx) (map[string]any, error) {
		res, err := tx.ExecContext(r.Context(), `DELETE FROM floor_items WHERE id = ?`, id)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil, errNotFound
		}
		return map[string]any{"id": id}, nil
	})
}
