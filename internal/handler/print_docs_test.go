package handler

import (
	"bytes"
	"testing"
	"time"
)

func TestBuildReceipt(t *testing.T) {
	tk := &TicketData{
		Settings:       Settings{BusinessName: "Open POS", PricesIncludeTax: true, TicketFooter: "¡Gracias!"},
		OrderID:        7,
		BillNumber:     1,
		CustomerName:   "Ana",
		PaidAt:         time.Date(2026, 9, 22, 17, 5, 0, 0, time.UTC),
		Items:          []TicketItem{{Name: "Latte 12oz", Quantity: 1, Amount: 65}, {Name: "Chilaquiles Verdes", Quantity: 1, Amount: 120, Notes: "Sin cebolla"}},
		Subtotal:       185,
		Discount:       18.5,
		DiscountType:   "PERCENT",
		DiscountValue:  10,
		DiscountReason: "Cliente frecuente",
		Tax:            22.97,
		Total:          166.5,
		Tip:            16.65,
		Grand:          183.15,
		Payments: []TicketPayment{
			{Method: "Tarjeta de débito", Amount: 91.58, Reference: "1234"},
			{Method: "Efectivo", Amount: 91.57, Received: 100, Change: 8.43},
		},
		Change: 8.43,
	}

	out := receiptDoc(tk, 58, defaultReceiptFormat(tk.Settings), nil).Bytes()

	// En 58 mm caben 32 columnas; cada renglón con importe debe llenarlas exactamente.
	for _, line := range []string{
		"Orden #7                Cuenta 1\n",
		"1 Latte 12oz              $65.00\n",
		"1 Chilaquiles Verdes     $120.00\n",
		"Descuento (10%)          -$18.50\n",
		"TOTAL PAGADO             $183.15\n",
		"Cambio                     $8.43\n",
	} {
		if len(line) != 33 {
			t.Fatalf("renglón de prueba mal medido: %q", line)
		}
		if !bytes.Contains(out, []byte(line)) {
			t.Errorf("falta el renglón %q", line)
		}
	}
	// "Tarjeta de débito (1234)" lleva é codificada en PC850 (0x82).
	if !bytes.Contains(out, []byte("Tarjeta de d\x82bito (1234)")) {
		t.Error("falta el pago con tarjeta codificado en PC850")
	}
	if !bytes.HasSuffix(out, []byte{0x1d, 'V', 1}) {
		t.Error("el ticket debe terminar con el comando de corte")
	}
}
