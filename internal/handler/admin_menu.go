package handler

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

type AdminCategory struct {
	ID        int64  `json:"id"`
	ParentID  *int64 `json:"parent_id"` // nil = categoría principal
	Name      string `json:"name"`
	ColorHex  string `json:"color_hex"`
	SortOrder int    `json:"sort_order"`
	IsActive  bool   `json:"is_active"`
}

type AdminProduct struct {
	ID          int64   `json:"id"`
	CategoryID  int64   `json:"category_id"`
	AreaID      *int64  `json:"area_id"` // área de producción; nil = no se imprime comanda
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Price       float64 `json:"price"`
	SKU         string  `json:"sku"`
	SortOrder   int     `json:"sort_order"`
	IsAvailable bool    `json:"is_available"`
	IsActive    bool    `json:"is_active"`
	GroupIDs    []int64 `json:"modifier_group_ids"`
}

type AdminArea struct {
	ID        int64    `json:"id"`
	Name      string   `json:"name"`
	ColorHex  string   `json:"color_hex"`
	SortOrder int      `json:"sort_order"`
	IsActive  bool     `json:"is_active"`
	Printers  []string `json:"printers"` // impresoras que reciben sus comandas
}

var colorRe = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)

// GET /api/admin/menu - Todo el catálogo, incluidos archivados y agotados.
func (h *POSHandler) AdminGetMenu(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cats := []AdminCategory{}
	if err := eachRow(ctx, h.DB, `
		SELECT id, parent_id, name, COALESCE(color_hex, '#3B82F6'), sort_order, is_active
		FROM categories ORDER BY sort_order, id`, nil, func(scan func(...any) error) error {
		var c AdminCategory
		var parent sql.NullInt64
		if err := scan(&c.ID, &parent, &c.Name, &c.ColorHex, &c.SortOrder, &c.IsActive); err != nil {
			return err
		}
		if parent.Valid {
			c.ParentID = &parent.Int64
		}
		cats = append(cats, c)
		return nil
	}); err != nil {
		serverError(w, "Error consultando categorías", err)
		return
	}

	groups, byProduct, err := loadModifierCatalog(ctx, h.DB, false)
	if err != nil {
		serverError(w, "Error consultando modificadores", err)
		return
	}
	prods := []AdminProduct{}
	if err := eachRow(ctx, h.DB, `
		SELECT id, category_id, area_id, name, COALESCE(description, ''), price, COALESCE(sku, ''),
		       sort_order, is_available, COALESCE(is_active, 1)
		FROM products ORDER BY sort_order, id`, nil, func(scan func(...any) error) error {
		var p AdminProduct
		var area sql.NullInt64
		if err := scan(&p.ID, &p.CategoryID, &area, &p.Name, &p.Description, &p.Price, &p.SKU,
			&p.SortOrder, &p.IsAvailable, &p.IsActive); err != nil {
			return err
		}
		if area.Valid {
			p.AreaID = &area.Int64
		}
		p.GroupIDs = byProduct[p.ID]
		if p.GroupIDs == nil {
			p.GroupIDs = []int64{}
		}
		prods = append(prods, p)
		return nil
	}); err != nil {
		serverError(w, "Error consultando productos", err)
		return
	}

	areas, err := loadAreas(r, h.DB)
	if err != nil {
		serverError(w, "Error consultando áreas", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"categories": cats, "products": prods, "areas": areas, "modifier_groups": groups,
	})
}

func loadAreas(r *http.Request, q queryer) ([]AdminArea, error) {
	areas := []AdminArea{}
	index := map[int64]int{}
	if err := eachRow(r.Context(), q, `
		SELECT id, name, color_hex, sort_order, is_active FROM production_areas ORDER BY sort_order, id`, nil,
		func(scan func(...any) error) error {
			a := AdminArea{Printers: []string{}}
			if err := scan(&a.ID, &a.Name, &a.ColorHex, &a.SortOrder, &a.IsActive); err != nil {
				return err
			}
			index[a.ID] = len(areas)
			areas = append(areas, a)
			return nil
		}); err != nil {
		return nil, err
	}
	err := eachRow(r.Context(), q, `
		SELECT pa.area_id, p.name FROM printer_areas pa JOIN printers p ON p.id = pa.printer_id
		WHERE COALESCE(p.is_active, 1) = 1 ORDER BY p.id`, nil, func(scan func(...any) error) error {
		var id int64
		var name string
		if err := scan(&id, &name); err != nil {
			return err
		}
		if i, ok := index[id]; ok {
			areas[i].Printers = append(areas[i].Printers, name)
		}
		return nil
	})
	return areas, err
}

// --- Categorías ---

func decodeCategory(w http.ResponseWriter, r *http.Request) (*AdminCategory, bool) {
	var c AdminCategory
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		http.Error(w, "JSON inválido", http.StatusBadRequest)
		return nil, false
	}
	c.Name = strings.TrimSpace(c.Name)
	if c.Name == "" {
		http.Error(w, "Escribe el nombre de la categoría", http.StatusBadRequest)
		return nil, false
	}
	if !colorRe.MatchString(c.ColorHex) {
		http.Error(w, "Color inválido", http.StatusBadRequest)
		return nil, false
	}
	return &c, true
}

// checkParent valida que la categoría padre exista y sea principal: solo hay
// un nivel de subcategorías (Bebidas -> Con café).
func checkParent(r *http.Request, tx *sql.Tx, c *AdminCategory) error {
	if c.ParentID == nil {
		return nil
	}
	if *c.ParentID == c.ID {
		return badRequest("Una categoría no puede ser su propia subcategoría")
	}
	var grand sql.NullInt64
	err := tx.QueryRowContext(r.Context(), `SELECT parent_id FROM categories WHERE id = ?`, *c.ParentID).Scan(&grand)
	if errors.Is(err, sql.ErrNoRows) {
		return badRequest("La categoría principal no existe")
	}
	if err != nil {
		return err
	}
	if grand.Valid {
		return badRequest("Solo hay un nivel de subcategorías: elige una categoría principal")
	}
	if c.ID != 0 {
		var children int
		if err := tx.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM categories WHERE parent_id = ?`, c.ID).Scan(&children); err != nil {
			return err
		}
		if children > 0 {
			return badRequest("Esta categoría tiene subcategorías; no puede volverse subcategoría")
		}
	}
	return nil
}

// POST /api/admin/categories
func (h *POSHandler) AdminCreateCategory(w http.ResponseWriter, r *http.Request) {
	c, ok := decodeCategory(w, r)
	if !ok {
		return
	}
	h.adminWrite(w, r, "CATEGORY_CREATED", func(tx *sql.Tx) (map[string]any, error) {
		if err := checkParent(r, tx, c); err != nil {
			return nil, err
		}
		res, err := tx.ExecContext(r.Context(),
			`INSERT INTO categories (parent_id, name, color_hex, sort_order, is_active) VALUES (?, ?, ?, ?, ?)`,
			c.ParentID, c.Name, c.ColorHex, c.SortOrder, c.IsActive)
		if err != nil {
			return nil, err
		}
		c.ID, _ = res.LastInsertId()
		return map[string]any{"category": c}, nil
	})
}

// PUT /api/admin/categories/{id}
func (h *POSHandler) AdminUpdateCategory(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.Error(w, "ID inválido", http.StatusBadRequest)
		return
	}
	c, ok := decodeCategory(w, r)
	if !ok {
		return
	}
	c.ID = id
	h.adminWrite(w, r, "CATEGORY_UPDATED", func(tx *sql.Tx) (map[string]any, error) {
		if err := checkParent(r, tx, c); err != nil {
			return nil, err
		}
		res, err := tx.ExecContext(r.Context(),
			`UPDATE categories SET parent_id = ?, name = ?, color_hex = ?, sort_order = ?, is_active = ? WHERE id = ?`,
			c.ParentID, c.Name, c.ColorHex, c.SortOrder, c.IsActive, id)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil, errNotFound
		}
		return map[string]any{"category": c}, nil
	})
}

// --- Productos ---

func decodeProduct(w http.ResponseWriter, r *http.Request) (*AdminProduct, bool) {
	var p AdminProduct
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, "JSON inválido", http.StatusBadRequest)
		return nil, false
	}
	p.Name, p.Description, p.SKU = strings.TrimSpace(p.Name), strings.TrimSpace(p.Description), strings.TrimSpace(p.SKU)
	switch {
	case p.Name == "":
		http.Error(w, "Escribe el nombre del producto", http.StatusBadRequest)
		return nil, false
	case p.Price < 0 || toCents(p.Price) > 10_000_000:
		http.Error(w, "Precio inválido", http.StatusBadRequest)
		return nil, false
	case p.CategoryID <= 0:
		http.Error(w, "Selecciona la categoría", http.StatusBadRequest)
		return nil, false
	}
	p.Price = fromCents(toCents(p.Price))
	return &p, true
}

func checkExists(r *http.Request, tx *sql.Tx, table string, id int64, msg string) error {
	var n int
	if err := tx.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM `+table+` WHERE id = ?`, id).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return badRequest(msg)
	}
	return nil
}

func checkProductRefs(r *http.Request, tx *sql.Tx, p *AdminProduct) error {
	if err := checkExists(r, tx, "categories", p.CategoryID, "La categoría no existe"); err != nil {
		return err
	}
	if p.AreaID != nil {
		if err := checkExists(r, tx, "production_areas", *p.AreaID, "El área de producción no existe"); err != nil {
			return err
		}
	}
	for _, g := range p.GroupIDs {
		if err := checkExists(r, tx, "modifier_groups", g, "Un grupo de modificadores no existe"); err != nil {
			return err
		}
	}
	return nil
}

// saveProductGroups reemplaza los grupos de modificadores del producto, en el
// orden en que se deben preguntar.
func saveProductGroups(r *http.Request, tx *sql.Tx, productID int64, groupIDs []int64) error {
	if _, err := tx.ExecContext(r.Context(), `DELETE FROM product_modifier_groups WHERE product_id = ?`, productID); err != nil {
		return err
	}
	for i, g := range groupIDs {
		if _, err := tx.ExecContext(r.Context(), `
			INSERT OR IGNORE INTO product_modifier_groups (product_id, modifier_group_id, sort_order) VALUES (?, ?, ?)`,
			productID, g, i); err != nil {
			return err
		}
	}
	return nil
}

// POST /api/admin/products
func (h *POSHandler) AdminCreateProduct(w http.ResponseWriter, r *http.Request) {
	p, ok := decodeProduct(w, r)
	if !ok {
		return
	}
	h.adminWrite(w, r, "PRODUCT_CREATED", func(tx *sql.Tx) (map[string]any, error) {
		if err := checkProductRefs(r, tx, p); err != nil {
			return nil, err
		}
		res, err := tx.ExecContext(r.Context(), `
			INSERT INTO products (category_id, area_id, name, description, price, sku, sort_order, is_available, is_active)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			p.CategoryID, p.AreaID, p.Name, nullIfEmpty(p.Description), p.Price, nullIfEmpty(p.SKU),
			p.SortOrder, p.IsAvailable, p.IsActive)
		if err != nil {
			return nil, err
		}
		p.ID, _ = res.LastInsertId()
		if err := saveProductGroups(r, tx, p.ID, p.GroupIDs); err != nil {
			return nil, err
		}
		return map[string]any{"product": p}, nil
	})
}

// PUT /api/admin/products/{id} - Los cambios de precio quedan en la bitácora
// con el antes y el después. Las órdenes ya registradas conservan su precio.
func (h *POSHandler) AdminUpdateProduct(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.Error(w, "ID inválido", http.StatusBadRequest)
		return
	}
	p, ok := decodeProduct(w, r)
	if !ok {
		return
	}
	p.ID = id
	h.adminWrite(w, r, "PRODUCT_UPDATED", func(tx *sql.Tx) (map[string]any, error) {
		if err := checkProductRefs(r, tx, p); err != nil {
			return nil, err
		}
		var before AdminProduct
		err := tx.QueryRowContext(r.Context(), `SELECT name, price, is_active FROM products WHERE id = ?`, id).
			Scan(&before.Name, &before.Price, &before.IsActive)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNotFound
		}
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(r.Context(), `
			UPDATE products SET category_id = ?, area_id = ?, name = ?, description = ?, price = ?, sku = ?,
			       sort_order = ?, is_available = ?, is_active = ?
			WHERE id = ?`,
			p.CategoryID, p.AreaID, p.Name, nullIfEmpty(p.Description), p.Price, nullIfEmpty(p.SKU),
			p.SortOrder, p.IsAvailable, p.IsActive, id); err != nil {
			return nil, err
		}
		if err := saveProductGroups(r, tx, id, p.GroupIDs); err != nil {
			return nil, err
		}
		details := map[string]any{"product_id": id, "name": p.Name}
		if toCents(before.Price) != toCents(p.Price) {
			details["price_before"], details["price_after"] = before.Price, p.Price
		}
		if before.Name != p.Name {
			details["name_before"] = before.Name
		}
		if before.IsActive != p.IsActive {
			details["archived"] = !p.IsActive
		}
		return details, nil
	})
}

// POST /api/admin/products/{id}/availability - Marca un producto agotado o
// disponible sin abrir el formulario.
func (h *POSHandler) AdminSetAvailability(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.Error(w, "ID inválido", http.StatusBadRequest)
		return
	}
	var req struct {
		Available bool `json:"available"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "JSON inválido", http.StatusBadRequest)
		return
	}
	h.adminWrite(w, r, "PRODUCT_AVAILABILITY", func(tx *sql.Tx) (map[string]any, error) {
		res, err := tx.ExecContext(r.Context(), `UPDATE products SET is_available = ? WHERE id = ?`, req.Available, id)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil, errNotFound
		}
		return map[string]any{"product_id": id, "available": req.Available}, nil
	})
}

// --- Áreas de producción ---

func decodeArea(w http.ResponseWriter, r *http.Request) (*AdminArea, bool) {
	var a AdminArea
	if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
		http.Error(w, "JSON inválido", http.StatusBadRequest)
		return nil, false
	}
	a.Name = strings.TrimSpace(a.Name)
	if a.Name == "" {
		http.Error(w, "Escribe el nombre del área", http.StatusBadRequest)
		return nil, false
	}
	if !colorRe.MatchString(a.ColorHex) {
		http.Error(w, "Color inválido", http.StatusBadRequest)
		return nil, false
	}
	return &a, true
}

// POST /api/admin/areas
func (h *POSHandler) AdminCreateArea(w http.ResponseWriter, r *http.Request) {
	a, ok := decodeArea(w, r)
	if !ok {
		return
	}
	h.adminWrite(w, r, "AREA_CREATED", func(tx *sql.Tx) (map[string]any, error) {
		res, err := tx.ExecContext(r.Context(),
			`INSERT INTO production_areas (name, color_hex, sort_order, is_active) VALUES (?, ?, ?, ?)`,
			a.Name, a.ColorHex, a.SortOrder, a.IsActive)
		if err != nil {
			return nil, err
		}
		a.ID, _ = res.LastInsertId()
		return map[string]any{"area": a}, nil
	})
}

// PUT /api/admin/areas/{id} - Un área con productos activos no se archiva.
func (h *POSHandler) AdminUpdateArea(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.Error(w, "ID inválido", http.StatusBadRequest)
		return
	}
	a, ok := decodeArea(w, r)
	if !ok {
		return
	}
	a.ID = id
	h.adminWrite(w, r, "AREA_UPDATED", func(tx *sql.Tx) (map[string]any, error) {
		if !a.IsActive {
			var n int
			if err := tx.QueryRowContext(r.Context(),
				`SELECT COUNT(*) FROM products WHERE area_id = ? AND COALESCE(is_active, 1) = 1`, id).Scan(&n); err != nil {
				return nil, err
			}
			if n > 0 {
				return nil, badRequest(fmt.Sprintf("El área tiene %d producto(s) activos; muévelos a otra área antes de archivarla", n))
			}
		}
		res, err := tx.ExecContext(r.Context(),
			`UPDATE production_areas SET name = ?, color_hex = ?, sort_order = ?, is_active = ? WHERE id = ?`,
			a.Name, a.ColorHex, a.SortOrder, a.IsActive, id)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil, errNotFound
		}
		return map[string]any{"area": a}, nil
	})
}

// --- Grupos de modificadores ---

func decodeGroup(w http.ResponseWriter, r *http.Request) (*ModifierGroup, bool) {
	var g ModifierGroup
	if err := json.NewDecoder(r.Body).Decode(&g); err != nil {
		http.Error(w, "JSON inválido", http.StatusBadRequest)
		return nil, false
	}
	g.Name = strings.TrimSpace(g.Name)
	var opts []ModifierOption
	for _, o := range g.Options {
		o.Name = strings.TrimSpace(o.Name)
		if o.Name == "" {
			continue
		}
		if o.PriceExtra < 0 {
			http.Error(w, "El precio extra no puede ser negativo", http.StatusBadRequest)
			return nil, false
		}
		o.PriceExtra = fromCents(toCents(o.PriceExtra))
		opts = append(opts, o)
	}
	g.Options = opts
	switch {
	case g.Name == "":
		http.Error(w, "Escribe el nombre del grupo", http.StatusBadRequest)
		return nil, false
	case len(g.Options) == 0:
		http.Error(w, "Agrega al menos una opción", http.StatusBadRequest)
		return nil, false
	case g.Min < 0 || g.Max < 1 || g.Min > g.Max:
		http.Error(w, "Revisa el mínimo y el máximo: el máximo es al menos 1 y no puede ser menor que el mínimo", http.StatusBadRequest)
		return nil, false
	case g.Min > len(g.Options):
		http.Error(w, "El mínimo es mayor que el número de opciones", http.StatusBadRequest)
		return nil, false
	}
	return &g, true
}

// saveGroupOptions actualiza las opciones existentes y agrega las nuevas. Las
// opciones no se borran (pueden estar en órdenes pasadas): se marcan como no
// disponibles.
func saveGroupOptions(r *http.Request, tx *sql.Tx, g *ModifierGroup) error {
	for i := range g.Options {
		o := &g.Options[i]
		if o.ID > 0 {
			res, err := tx.ExecContext(r.Context(), `
				UPDATE modifier_options SET name = ?, price_extra = ?, is_available = ?, sort_order = ?
				WHERE id = ? AND modifier_group_id = ?`,
				o.Name, o.PriceExtra, o.IsAvailable, i, o.ID, g.ID)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return badRequest("Una opción no pertenece a este grupo")
			}
			continue
		}
		res, err := tx.ExecContext(r.Context(), `
			INSERT INTO modifier_options (modifier_group_id, name, price_extra, is_available, sort_order)
			VALUES (?, ?, ?, ?, ?)`, g.ID, o.Name, o.PriceExtra, o.IsAvailable, i)
		if err != nil {
			return err
		}
		o.ID, _ = res.LastInsertId()
	}
	return nil
}

// POST /api/admin/modifier-groups
func (h *POSHandler) AdminCreateModifierGroup(w http.ResponseWriter, r *http.Request) {
	g, ok := decodeGroup(w, r)
	if !ok {
		return
	}
	h.adminWrite(w, r, "MODIFIER_GROUP_CREATED", func(tx *sql.Tx) (map[string]any, error) {
		res, err := tx.ExecContext(r.Context(), `
			INSERT INTO modifier_groups (name, min_selectable, max_selectable, sort_order, is_active) VALUES (?, ?, ?, ?, ?)`,
			g.Name, g.Min, g.Max, g.SortOrder, g.IsActive)
		if err != nil {
			return nil, err
		}
		g.ID, _ = res.LastInsertId()
		if err := saveGroupOptions(r, tx, g); err != nil {
			return nil, err
		}
		return map[string]any{"group": g}, nil
	})
}

// PUT /api/admin/modifier-groups/{id} - Los cambios de precio de las opciones
// solo aplican a órdenes nuevas.
func (h *POSHandler) AdminUpdateModifierGroup(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.Error(w, "ID inválido", http.StatusBadRequest)
		return
	}
	g, ok := decodeGroup(w, r)
	if !ok {
		return
	}
	g.ID = id
	h.adminWrite(w, r, "MODIFIER_GROUP_UPDATED", func(tx *sql.Tx) (map[string]any, error) {
		res, err := tx.ExecContext(r.Context(), `
			UPDATE modifier_groups SET name = ?, min_selectable = ?, max_selectable = ?, sort_order = ?, is_active = ?
			WHERE id = ?`, g.Name, g.Min, g.Max, g.SortOrder, g.IsActive, id)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil, errNotFound
		}
		if err := saveGroupOptions(r, tx, g); err != nil {
			return nil, err
		}
		return map[string]any{"group": g}, nil
	})
}

// --- Escritura común del panel ---

var errNotFound = errors.New("No encontrado")

type badRequest string

func (e badRequest) Error() string { return string(e) }

// adminWrite corre fn en una transacción y registra la acción en la bitácora
// firmada por el administrador en sesión. fn regresa los detalles a registrar.
// Con action vacía no se registra (p. ej. acomodar el plano del salón).
func (h *POSHandler) adminWrite(w http.ResponseWriter, r *http.Request, action string, fn func(tx *sql.Tx) (map[string]any, error)) {
	ctx := r.Context()
	admin, _ := currentUser(ctx)
	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		serverError(w, "Error al iniciar transacción", err)
		return
	}
	defer tx.Rollback()

	details, err := fn(tx)
	var bad badRequest
	switch {
	case errors.Is(err, errNotFound):
		http.Error(w, "No encontrado", http.StatusNotFound)
		return
	case errors.As(err, &bad):
		http.Error(w, bad.Error(), http.StatusConflict)
		return
	case err != nil:
		serverError(w, "Error guardando cambios", err)
		return
	}
	if action != "" {
		if err := auditBy(ctx, tx, admin, r, action, details); err != nil {
			serverError(w, "Error registrando bitácora", err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "Error guardando cambios", err)
		return
	}
	writeJSON(w, http.StatusOK, details)
}
