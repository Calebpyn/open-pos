package handler

import (
	"math"
	"net/http"
	"sort"
)

// --- Tiempos de salida ---
//
// Cuánto tarda cada platillo desde que se pide (la comanda) hasta que se
// marca entregado en Expo, por área de producción y por producto. Solo cuenta
// lo que se marcó en Expo; lo demás se reporta como "sin marcar".

// timingMaxMinutes: una entrega marcada después de esto casi siempre es un
// olvido (se marcó al final del turno), no el tiempo real de la cocina.
const timingMaxMinutes = 120

type TimingStat struct {
	Name   string  `json:"name"`
	Area   string  `json:"area,omitempty"`
	Count  int     `json:"count"` // renglones medidos
	Avg    float64 `json:"avg"`   // minutos
	Median float64 `json:"median"`
	P90    float64 `json:"p90"`
	Max    float64 `json:"max"`
}

func summarize(name, area string, mins []float64) TimingStat {
	sort.Float64s(mins)
	var sum float64
	for _, m := range mins {
		sum += m
	}
	pct := func(p float64) float64 {
		i := int(math.Ceil(p*float64(len(mins)))) - 1
		return mins[max(0, min(i, len(mins)-1))]
	}
	r := func(v float64) float64 { return math.Round(v*10) / 10 }
	return TimingStat{Name: name, Area: area, Count: len(mins), Avg: r(sum / float64(len(mins))),
		Median: r(pct(0.5)), P90: r(pct(0.9)), Max: r(mins[len(mins)-1])}
}

// GET /api/admin/reports/timings?from&to
func (h *POSHandler) AdminTimings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rg, err := parseRange(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	byArea := map[string][]float64{}
	type key struct{ product, area string }
	byProduct := map[key][]float64{}
	var total, unmarked, outliers int
	err = eachRow(ctx, h.DB, `
		SELECT p.name, COALESCE(pa.name, 'Sin área'),
		       CASE WHEN oi.delivered_at IS NULL THEN -1
		            ELSE (julianday(oi.delivered_at) - julianday(oi.created_at)) * 1440 END
		FROM order_items oi
		JOIN orders o ON o.id = oi.order_id
		JOIN products p ON p.id = oi.product_id
		LEFT JOIN production_areas pa ON pa.id = p.area_id
		WHERE oi.created_at >= ?1 AND oi.created_at < ?2
		  AND oi.status != 'VOID' AND o.status != 'CANCELLED'`, rg.args(), func(scan func(...any) error) error {
		var product, area string
		var mins float64
		if err := scan(&product, &area, &mins); err != nil {
			return err
		}
		total++
		switch {
		case mins < 0:
			unmarked++
		case mins > timingMaxMinutes:
			outliers++
		default:
			byArea[area] = append(byArea[area], mins)
			byProduct[key{product, area}] = append(byProduct[key{product, area}], mins)
		}
		return nil
	})
	if err != nil {
		serverError(w, "Error calculando tiempos", err)
		return
	}

	areas := []TimingStat{}
	for a, m := range byArea {
		areas = append(areas, summarize(a, "", m))
	}
	sort.Slice(areas, func(i, j int) bool { return areas[i].Avg > areas[j].Avg })

	// Productos más lentos (con al menos 3 medidos, para no juzgar por uno).
	products := []TimingStat{}
	for k, m := range byProduct {
		if len(m) >= 3 {
			products = append(products, summarize(k.product, k.area, m))
		}
	}
	sort.Slice(products, func(i, j int) bool { return products[i].Avg > products[j].Avg })
	if len(products) > 10 {
		products = products[:10]
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"range": rg, "areas": areas, "slowest": products,
		"total": total, "measured": total - unmarked - outliers, "unmarked": unmarked, "outliers": outliers,
		"max_minutes": timingMaxMinutes,
	})
}
