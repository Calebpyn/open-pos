package ocr

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// testdata/costco.json es la salida real de open-pos-ocr con la foto de un
// ticket (un poco chueca): el importe queda un poco más arriba que su
// descripción y aun así deben ir en el mismo renglón.
func TestRowsAndParseRealOCR(t *testing.T) {
	b, err := os.ReadFile("testdata/costco.json")
	if err != nil {
		t.Fatal(err)
	}
	var out struct{ Lines []Box }
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	rows := Rows(out.Lines)
	joined := strings.Join(rows, "\n")
	for _, want := range []string{"1234567 FRESA 400G 69.90", "7654321 LECHE ENT LALA 12PK 289.00", "TOTAL 792.65", "FECHA: 06/10/2026 14:32"} {
		if !strings.Contains(joined, want) {
			t.Errorf("falta el renglón %q en:\n%s", want, joined)
		}
	}

	r := Parse(rows)
	if r.Date != "2026-10-06" || r.TotalCents != 79265 || r.Header[0] != "COSTCO WHOLESALE" {
		t.Fatalf("fecha %q total %d encabezado %v", r.Date, r.TotalCents, r.Header)
	}
	want := []Line{
		{Text: "FRESA 400G", Qty: 1, AmountCents: 6990},
		{Text: "LECHE ENT LALA 12PK", Qty: 1, AmountCents: 28900},
		{Text: "VASOS TERMICOS 120Z", Qty: 2, AmountCents: 18500},
		{Text: "AGUACATE HASS", Qty: 0.85, ByWeight: true, AmountCents: 6375},
	}
	if len(r.Lines) != len(want) {
		t.Fatalf("productos: %+v", r.Lines)
	}
	for i, w := range want {
		if r.Lines[i] != w {
			t.Errorf("renglón %d: %+v, quiero %+v", i, r.Lines[i], w)
		}
	}
}

func TestParseVariants(t *testing.T) {
	r := Parse([]string{
		"SUPER FRESH MARKET",
		"06-OCT-26 18:02",
		"FRESA BURBUJA 200G 45.00 A",
		"LECHE ALMENDRA 1L",
		"55.00",
		"3 @ $12.00",
		"BOLILLO 36.00",
		"DESCUENTO -5.00",
		"TOTAL DE ARTICULOS 5",
		"SUBTOTAL 1,131.00",
		"TOTAL A PAGAR $1,131.00",
		"EFECTIVO 1,200.00",
		"CAMBIO 69.00",
	})
	if r.Date != "2026-10-06" || r.TotalCents != 113100 {
		t.Fatalf("fecha %q total %d", r.Date, r.TotalCents)
	}
	if len(r.Lines) != 3 || r.Lines[0].Text != "FRESA BURBUJA 200G" || r.Lines[1].Text != "LECHE ALMENDRA 1L" ||
		r.Lines[1].AmountCents != 5500 || r.Lines[2].Text != "BOLILLO" || r.Lines[2].Qty != 3 {
		t.Fatalf("productos: %+v", r.Lines)
	}

	// Cantidad después del producto (otras tiendas lo imprimen así).
	r = Parse([]string{"VASOS 12OZ 185.00", "2 X 92.50", "TOTAL 185.00"})
	if len(r.Lines) != 1 || r.Lines[0].Qty != 2 {
		t.Fatalf("cantidad posterior: %+v", r.Lines)
	}
	// Sin TOTAL usa el subtotal; fecha inválida no se toma.
	r = Parse([]string{"31/02/2026", "CAFE 1KG 380.00", "SUBTOTAL 380.00"})
	if r.Date != "" || r.TotalCents != 38000 {
		t.Fatalf("fecha %q total %d", r.Date, r.TotalCents)
	}
}

func TestWords(t *testing.T) {
	if got := strings.Join(Words("Leche ENT. Lala-12pk (Ñ)"), "|"); got != "leche|ent|lala|12pk|ñ" {
		t.Fatal(got)
	}
}
