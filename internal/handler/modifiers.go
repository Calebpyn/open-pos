package handler

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// modifierSep separa los modificadores en order_items.modifiers_text.
const modifierSep = " · "

type ModifierOption struct {
	ID          int64   `json:"id"`
	GroupID     int64   `json:"group_id"`
	Name        string  `json:"name"`
	PriceExtra  float64 `json:"price_extra"`
	IsAvailable bool    `json:"is_available"`
	SortOrder   int     `json:"sort_order"`
}

// ModifierGroup es un conjunto de opciones que se eligen al pedir un
// producto: "Tipo de leche" (elige 1, obligatorio) o "Jarabes" (0 a 3).
type ModifierGroup struct {
	ID        int64            `json:"id"`
	Name      string           `json:"name"`
	Min       int              `json:"min"`
	Max       int              `json:"max"`
	SortOrder int              `json:"sort_order"`
	IsActive  bool             `json:"is_active"`
	Options   []ModifierOption `json:"options"`
}

// loadModifierCatalog regresa los grupos con sus opciones y qué grupos
// tiene cada producto. Con posView solo grupos activos y opciones disponibles.
func loadModifierCatalog(ctx context.Context, q queryer, posView bool) ([]ModifierGroup, map[int64][]int64, error) {
	groups := []ModifierGroup{}
	index := map[int64]int{}
	where := ""
	if posView {
		where = "WHERE COALESCE(is_active, 1) = 1"
	}
	if err := eachRow(ctx, q, `
		SELECT id, name, min_selectable, max_selectable, COALESCE(sort_order, 0), COALESCE(is_active, 1)
		FROM modifier_groups `+where+` ORDER BY sort_order, id`, nil, func(scan func(...any) error) error {
		g := ModifierGroup{Options: []ModifierOption{}}
		if err := scan(&g.ID, &g.Name, &g.Min, &g.Max, &g.SortOrder, &g.IsActive); err != nil {
			return err
		}
		index[g.ID] = len(groups)
		groups = append(groups, g)
		return nil
	}); err != nil {
		return nil, nil, err
	}

	optWhere := ""
	if posView {
		optWhere = "WHERE is_available = 1"
	}
	if err := eachRow(ctx, q, `
		SELECT id, modifier_group_id, name, price_extra, is_available, COALESCE(sort_order, 0)
		FROM modifier_options `+optWhere+` ORDER BY sort_order, id`, nil, func(scan func(...any) error) error {
		var o ModifierOption
		if err := scan(&o.ID, &o.GroupID, &o.Name, &o.PriceExtra, &o.IsAvailable, &o.SortOrder); err != nil {
			return err
		}
		if i, ok := index[o.GroupID]; ok {
			groups[i].Options = append(groups[i].Options, o)
		}
		return nil
	}); err != nil {
		return nil, nil, err
	}

	byProduct := map[int64][]int64{}
	if err := eachRow(ctx, q, `
		SELECT product_id, modifier_group_id FROM product_modifier_groups
		ORDER BY product_id, sort_order, modifier_group_id`, nil, func(scan func(...any) error) error {
		var pid, gid int64
		if err := scan(&pid, &gid); err != nil {
			return err
		}
		if _, ok := index[gid]; ok {
			byProduct[pid] = append(byProduct[pid], gid)
		}
		return nil
	}); err != nil {
		return nil, nil, err
	}
	return groups, byProduct, nil
}

// GET /api/menu/modifiers - Lo que el POS necesita para pedir modificadores.
func (h *POSHandler) GetMenuModifiers(w http.ResponseWriter, r *http.Request) {
	groups, byProduct, err := loadModifierCatalog(r.Context(), h.DB, true)
	if err != nil {
		serverError(w, "Error cargando modificadores", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": groups, "products": byProduct})
}

// chosenModifier es una opción validada para una línea de la orden.
type chosenModifier struct {
	OptionID   int64
	Name       string
	PriceCents int64
}

// resolveModifiers valida las opciones elegidas para un producto contra su
// configuración (grupos asignados, mínimos y máximos, disponibilidad) y
// regresa el extra en centavos y el texto a guardar. Los precios salen del
// catálogo, nunca de la terminal.
func resolveModifiers(ctx context.Context, q queryer, productID int64, optionIDs []int64) (int64, string, []chosenModifier, error) {
	type groupRule struct {
		name     string
		min, max int
		order    int
		picked   int
	}
	rules := map[int64]*groupRule{}
	if err := eachRow(ctx, q, `
		SELECT g.id, g.name, g.min_selectable, g.max_selectable, pmg.sort_order
		FROM product_modifier_groups pmg JOIN modifier_groups g ON g.id = pmg.modifier_group_id
		WHERE pmg.product_id = ? AND COALESCE(g.is_active, 1) = 1`, []any{productID}, func(scan func(...any) error) error {
		var id int64
		g := &groupRule{}
		if err := scan(&id, &g.name, &g.min, &g.max, &g.order); err != nil {
			return err
		}
		rules[id] = g
		return nil
	}); err != nil {
		return 0, "", nil, err
	}

	type picked struct {
		chosenModifier
		groupOrder, optionOrder int
	}
	var list []picked
	seen := map[int64]bool{}
	for _, oid := range optionIDs {
		if seen[oid] {
			continue
		}
		seen[oid] = true
		var gid int64
		var p picked
		var price float64
		var available bool
		err := q.QueryRowContext(ctx, `
			SELECT modifier_group_id, name, price_extra, is_available, COALESCE(sort_order, 0)
			FROM modifier_options WHERE id = ?`, oid).Scan(&gid, &p.Name, &price, &available, &p.optionOrder)
		if err != nil {
			return 0, "", nil, badRequest(fmt.Sprintf("El modificador %d no existe", oid))
		}
		rule := rules[gid]
		if rule == nil {
			return 0, "", nil, badRequest(fmt.Sprintf("“%s” no aplica a este producto", p.Name))
		}
		if !available {
			return 0, "", nil, badRequest(fmt.Sprintf("“%s” está agotado", p.Name))
		}
		rule.picked++
		p.OptionID, p.PriceCents, p.groupOrder = oid, toCents(price), rule.order
		list = append(list, p)
	}
	for _, g := range rules {
		if g.picked < g.min {
			return 0, "", nil, badRequest(fmt.Sprintf("Elige al menos %d en “%s”", g.min, g.name))
		}
		if g.max > 0 && g.picked > g.max {
			return 0, "", nil, badRequest(fmt.Sprintf("Máximo %d en “%s”", g.max, g.name))
		}
	}

	sort.SliceStable(list, func(i, j int) bool {
		if list[i].groupOrder != list[j].groupOrder {
			return list[i].groupOrder < list[j].groupOrder
		}
		return list[i].optionOrder < list[j].optionOrder
	})
	var extra int64
	names := make([]string, 0, len(list))
	chosen := make([]chosenModifier, 0, len(list))
	for _, p := range list {
		extra += p.PriceCents
		names = append(names, p.Name)
		chosen = append(chosen, p.chosenModifier)
	}
	return extra, strings.Join(names, modifierSep), chosen, nil
}
