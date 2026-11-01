package handler

import (
	"math"
	"regexp"
	"strconv"
	"strings"
)

// --- Presentaciones y precio por kilo ---
//
// Un mismo insumo se vende distinto en cada lugar: fresas en burbuja de 400 g
// en Costco y de 200 g en Fresh Market, o aguacate a granel por kilo. Con el
// contenido de cada presentación se compara el precio por kg, L o pieza.

// comprasUnits convierte cada unidad a su base (kg, L o pieza).
var comprasUnits = map[string]struct {
	base   string
	factor float64
}{
	"g": {"kg", 0.001}, "kg": {"kg", 1}, "ml": {"L", 0.001}, "L": {"L", 1}, "pz": {"pz", 1},
}

// comprasUnitOptions son las unidades del formulario.
var comprasUnitOptions = []string{"g", "kg", "ml", "L", "pz"}

// unitAliases reconoce cómo se escribe cada unidad en una presentación.
var unitAliases = map[string]string{
	"g": "g", "gr": "g", "grs": "g", "gramo": "g", "gramos": "g",
	"kg": "kg", "kgs": "kg", "kilo": "kg", "kilos": "kg",
	"ml": "ml", "mililitros": "ml",
	"l": "L", "lt": "L", "lts": "L", "litro": "L", "litros": "L",
	"pz": "pz", "pza": "pz", "pzas": "pz", "pieza": "pz", "piezas": "pz",
}

var (
	contentPackRe = regexp.MustCompile(`(?i)(\d+)\s*[x×]\s*(\d+(?:[.,]\d+)?)\s*([a-z]+)\b`)
	contentRe     = regexp.MustCompile(`(?i)(\d+(?:[.,]\d+)?)\s*([a-z]+)\b`)
)

// parseContent lee el contenido de una presentación: "Burbuja 400 g" →
// 400 g; "Caja 12 × 1 L" → 12 L. ok=false si no trae una cantidad clara.
func parseContent(label string) (qty float64, unit string, ok bool) {
	num := func(s string) float64 {
		f, _ := strconv.ParseFloat(strings.Replace(s, ",", ".", 1), 64)
		return f
	}
	if m := contentPackRe.FindStringSubmatch(label); m != nil {
		if u, known := unitAliases[strings.ToLower(m[3])]; known {
			return num(m[1]) * num(m[2]), u, true
		}
	}
	for _, m := range contentRe.FindAllStringSubmatch(label, -1) {
		if u, known := unitAliases[strings.ToLower(m[2])]; known && num(m[1]) > 0 {
			return num(m[1]), u, true
		}
	}
	return 0, "", false
}

// UnitPrice es el precio por kg, L o pieza (en centavos) y su base.
func (s *comprasSource) UnitPrice() (cents float64, base string, ok bool) {
	if s.PriceCents == nil {
		return 0, "", false
	}
	if s.ByWeight {
		return float64(*s.PriceCents), "kg", true
	}
	u, known := comprasUnits[s.ContentUnit]
	if !known || s.ContentQty == nil || *s.ContentQty <= 0 {
		return 0, "", false
	}
	return float64(*s.PriceCents) / (*s.ContentQty * u.factor), u.base, true
}

// UnitPriceLabel es "$175.00/kg" (vacío si no se puede calcular).
func (s *comprasSource) UnitPriceLabel() string {
	cents, base, ok := s.UnitPrice()
	if !ok {
		return ""
	}
	return formatCents(int64(math.Round(cents))) + "/" + base
}

// PriceLabel es el precio como se paga: "$70.00" o "$75.00/kg" a granel.
func (s *comprasSource) PriceLabel() string {
	if s.PriceCents == nil {
		return ""
	}
	if s.ByWeight {
		return formatCents(*s.PriceCents) + "/kg"
	}
	return formatCents(*s.PriceCents)
}

// Pack es la presentación en ese lugar (o la del insumo si no tiene propia).
func (s *comprasSource) Pack(def string) string {
	switch {
	case s.ByWeight:
		return "a granel"
	case s.PackLabel != "":
		return s.PackLabel
	}
	return def
}

// comprasCheapest es el lugar más barato: por kg/L/pieza cuando se puede
// comparar con el de siempre y, si no, por el precio de la presentación.
// -1 si no hay con qué comparar.
func comprasCheapest(sources []comprasSource) int {
	best, bestVal := -1, 0.0
	baseOf := ""
	if len(sources) > 0 {
		if _, b, ok := sources[0].UnitPrice(); ok {
			baseOf = b
		}
	}
	for i := range sources {
		var v float64
		if baseOf != "" {
			c, b, ok := sources[i].UnitPrice()
			if !ok || b != baseOf {
				continue
			}
			v = c
		} else {
			if sources[i].PriceCents == nil {
				continue
			}
			v = float64(*sources[i].PriceCents)
		}
		if best < 0 || v < bestVal {
			best, bestVal = i, v
		}
	}
	return best
}

// fillContent completa el contenido de una presentación a partir de su texto
// (o del de el insumo) cuando no se capturó.
func (s *comprasSource) fillContent(supplyUnit string) {
	if s.ByWeight || (s.ContentQty != nil && *s.ContentQty > 0 && s.ContentUnit != "") {
		return
	}
	label := s.PackLabel
	if label == "" {
		label = supplyUnit
	}
	if q, u, ok := parseContent(label); ok {
		s.ContentQty, s.ContentUnit = &q, u
	}
}

// --- Cantidades ---

// parseQty lee una cantidad: entera para presentaciones, con decimales (kg)
// para lo que se compra a granel.
func parseQty(s string) (float64, error) {
	f, err := strconv.ParseFloat(strings.Replace(strings.TrimSpace(s), ",", ".", 1), 64)
	if err != nil || f <= 0 || f > 9999 {
		return 0, badRequest("La cantidad debe ser mayor a cero.")
	}
	return math.Round(f*1000) / 1000, nil
}

// formatQty es "2", "1.5" o "0.25".
func formatQty(q float64) string {
	return strconv.FormatFloat(q, 'f', -1, 64)
}

// QtyLabel es cómo se lee la cantidad del renglón: "1.5 kg" a granel, "×2"
// por presentación (vacío si es una sola).
func (it *comprasItem) QtyLabel() string {
	if p := it.Preferred(); p != nil && p.ByWeight {
		return formatQty(it.Quantity) + " kg"
	}
	if it.Quantity == 1 {
		return ""
	}
	return "×" + formatQty(it.Quantity)
}

// ByWeight indica si el renglón se compra a granel (por kg).
func (it *comprasItem) ByWeight() bool {
	p := it.Preferred()
	return p != nil && p.ByWeight
}

// PackLabel es la presentación del lugar de siempre.
func (it *comprasItem) PackLabel() string {
	if p := it.Preferred(); p != nil {
		return p.Pack(it.Unit)
	}
	return it.Unit
}

// CheaperAlt indica si la alternativa i (0 = la primera después del lugar de
// siempre) sale más barata que el lugar de siempre.
func (it *comprasItem) CheaperAlt(i int) bool {
	return i+1 < len(it.Sources) && comprasCheapest(it.Sources) == i+1
}

// QtyInput es la cantidad para el campo del formulario.
func (it *comprasItem) QtyInput() string { return formatQty(it.Quantity) }
