package handler

import "testing"

func TestComputeTotals(t *testing.T) {
	withTax := Settings{TaxRate: 0.16, PricesIncludeTax: true}
	plusTax := Settings{TaxRate: 0.16, PricesIncludeTax: false}

	tests := []struct {
		name      string
		items     int64
		discType  string
		discValue float64
		tip       int64
		settings  Settings
		want      BillTotals
		wantErr   bool
	}{
		{
			name: "IVA incluido sin descuento", items: 11600, settings: withTax,
			want: BillTotals{Items: 11600, Tax: 1600, Total: 11600, Grand: 11600},
		},
		{
			name: "descuento 10% con propina", items: 20000, discType: "PERCENT", discValue: 10, tip: 2700, settings: withTax,
			want: BillTotals{Items: 20000, Discount: 2000, Tax: 2483, Total: 18000, Tip: 2700, Grand: 20700},
		},
		{
			name: "descuento fijo", items: 6500, discType: "AMOUNT", discValue: 15.5, settings: withTax,
			want: BillTotals{Items: 6500, Discount: 1550, Tax: 683, Total: 4950, Grand: 4950},
		},
		{
			name: "cortesía 100%", items: 4500, discType: "PERCENT", discValue: 100, settings: withTax,
			want: BillTotals{Items: 4500, Discount: 4500},
		},
		{
			name: "IVA agregado", items: 10000, settings: plusTax,
			want: BillTotals{Items: 10000, Tax: 1600, Total: 11600, Grand: 11600},
		},
		{name: "descuento mayor al importe", items: 1000, discType: "AMOUNT", discValue: 20, settings: withTax, wantErr: true},
		{name: "porcentaje fuera de rango", items: 1000, discType: "PERCENT", discValue: 120, settings: withTax, wantErr: true},
		{name: "propina negativa", items: 1000, tip: -1, settings: withTax, wantErr: true},
		{name: "tipo inválido", items: 1000, discType: "BOGO", settings: withTax, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := computeTotals(tt.items, tt.discType, tt.discValue, tt.tip, tt.settings)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("se esperaba error, se obtuvo %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("error inesperado: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestFormatMoney(t *testing.T) {
	cases := map[float64]string{0: "$0.00", 45: "$45.00", 1234.5: "$1,234.50", 1234567.891: "$1,234,567.89", -12.3: "-$12.30"}
	for in, want := range cases {
		if got := formatMoney(in); got != want {
			t.Errorf("formatMoney(%v) = %q, want %q", in, got, want)
		}
	}
}
