package handler

import (
	"database/sql"
	"net/http"
)

type menuProduct struct {
	ID        int64   `json:"id"`
	Name      string  `json:"name"`
	Price     float64 `json:"price"`
	Available bool    `json:"available"`
	HasMods   bool    `json:"has_mods"` // abre el selector de modificadores al agregarlo
}

type menuCategory struct {
	ID       int64           `json:"id"`
	Name     string          `json:"name"`
	Color    string          `json:"color"`
	Products []menuProduct   `json:"products"`
	Children []*menuCategory `json:"children"`
}

// prune quita las ramas sin productos: en el POS no sirve entrar a una
// categoría vacía.
func prune(cats []*menuCategory) []*menuCategory {
	out := []*menuCategory{}
	for _, c := range cats {
		c.Children = prune(c.Children)
		if len(c.Products) > 0 || len(c.Children) > 0 {
			out = append(out, c)
		}
	}
	return out
}

// GET /api/menu/tree - Catálogo para el POS como árbol: categorías
// principales, sus subcategorías y los productos de cada una. El POS lo
// recorre nodo por nodo. Los agotados van marcados (se ven en gris); los
// archivados no aparecen.
func (h *POSHandler) GetMenuTree(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	byID := map[int64]*menuCategory{}
	var top []*menuCategory
	type pending struct {
		cat    *menuCategory
		parent int64
	}
	var children []pending
	if err := eachRow(ctx, h.DB, `
		SELECT id, parent_id, name, COALESCE(color_hex, '#9ca3af') FROM categories
		WHERE is_active = 1 ORDER BY sort_order, id`, nil, func(scan func(...any) error) error {
		c := &menuCategory{Products: []menuProduct{}, Children: []*menuCategory{}}
		var parent sql.NullInt64
		if err := scan(&c.ID, &parent, &c.Name, &c.Color); err != nil {
			return err
		}
		byID[c.ID] = c
		if parent.Valid {
			children = append(children, pending{c, parent.Int64})
		} else {
			top = append(top, c)
		}
		return nil
	}); err != nil {
		serverError(w, "Error consultando categorías", err)
		return
	}
	// Una subcategoría cuya categoría principal está oculta también se oculta.
	for _, ch := range children {
		if p, ok := byID[ch.parent]; ok {
			p.Children = append(p.Children, ch.cat)
		} else {
			delete(byID, ch.cat.ID)
		}
	}

	if err := eachRow(ctx, h.DB, `
		SELECT p.id, p.category_id, p.name, p.price, p.is_available,
		       EXISTS (SELECT 1 FROM product_modifier_groups pmg JOIN modifier_groups g ON g.id = pmg.modifier_group_id
		               WHERE pmg.product_id = p.id AND COALESCE(g.is_active, 1) = 1)
		FROM products p WHERE COALESCE(p.is_active, 1) = 1 ORDER BY p.sort_order, p.id`, nil, func(scan func(...any) error) error {
		var p menuProduct
		var catID int64
		if err := scan(&p.ID, &catID, &p.Name, &p.Price, &p.Available, &p.HasMods); err != nil {
			return err
		}
		if c, ok := byID[catID]; ok {
			c.Products = append(c.Products, p)
		}
		return nil
	}); err != nil {
		serverError(w, "Error consultando productos", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"categories": prune(top)})
}
