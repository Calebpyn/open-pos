package handler

import (
	"errors"
	"fmt"
	"math"
	"strconv"
)

// Todo el cálculo de cobro se hace en centavos (int64) para evitar errores de
// redondeo con float64. checkout.html replica exactamente estas fórmulas.

func toCents(v float64) int64 { return int64(math.Round(v * 100)) }

func fromCents(c int64) float64 { return float64(c) / 100 }

// formatMoney da formato $1,234.56
func formatMoney(v float64) string {
	c := toCents(v)
	sign := ""
	if c < 0 {
		sign, c = "-", -c
	}
	s := strconv.FormatInt(c/100, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return fmt.Sprintf("%s$%s.%02d", sign, s, c%100)
}

type BillTotals struct {
	Items    int64 `json:"items"`    // suma de productos
	Discount int64 `json:"discount"` // descuento aplicado
	Tax      int64 `json:"tax"`      // IVA (incluido o agregado)
	Total    int64 `json:"total"`    // a pagar sin propina
	Tip      int64 `json:"tip"`
	Grand    int64 `json:"grand"` // total + propina
}

func computeTotals(itemsCents int64, discountType string, discountValue float64, tipCents int64, s Settings) (BillTotals, error) {
	t := BillTotals{Items: itemsCents, Tip: tipCents}

	switch discountType {
	case "":
	case "PERCENT":
		if discountValue < 0 || discountValue > 100 {
			return t, errors.New("El descuento debe estar entre 0% y 100%")
		}
		t.Discount = int64(math.Round(float64(itemsCents) * discountValue / 100))
	case "AMOUNT":
		if discountValue < 0 {
			return t, errors.New("El descuento no puede ser negativo")
		}
		t.Discount = toCents(discountValue)
		if t.Discount > itemsCents {
			return t, errors.New("El descuento no puede ser mayor al importe")
		}
	default:
		return t, fmt.Errorf("Tipo de descuento inválido: %q", discountType)
	}

	if tipCents < 0 {
		return t, errors.New("La propina no puede ser negativa")
	}

	net := itemsCents - t.Discount
	if s.PricesIncludeTax {
		t.Total = net
		t.Tax = net - int64(math.Round(float64(net)/(1+s.TaxRate)))
	} else {
		t.Tax = int64(math.Round(float64(net) * s.TaxRate))
		t.Total = net + t.Tax
	}
	t.Grand = t.Total + t.Tip
	return t, nil
}
