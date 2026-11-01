package ocr

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Receipt es lo que se pudo leer de un ticket. Todo es una sugerencia: la
// persona lo revisa antes de registrar la compra.
type Receipt struct {
	Header     []string // primeros renglones (nombre de la tienda, sucursal)
	Date       string   // AAAA-MM-DD, vacío si no se encontró
	TotalCents int64    // 0 si no se encontró
	Lines      []Line
}

// Line es un producto del ticket.
type Line struct {
	Text        string  // descripción como viene (sin código ni importe)
	Qty         float64 // piezas, o kg si ByWeight
	ByWeight    bool
	AmountCents int64 // importe del renglón
}

var (
	// Importe al final del renglón: "69.90", "$1,234.50", "289.00 A".
	amountRe = regexp.MustCompile(`(-?)\$?\s?(\d{1,3}(?:,\d{3})+|\d+)[.,](\d{2})\s*[A-Z*]{0,2}$`)
	// Renglón de cantidad: "2 X 92.50", "0.850 KG X 75.00", "3 @ $12.00".
	qtyRe = regexp.MustCompile(`(?i)^(\d+(?:[.,]\d+)?)\s*(kg|kgs|k|pz|pza|pzas)?\s*[x@×*]\s*\$?(\d+(?:,\d{3})*[.,]\d{2})$`)
	// Código de producto al inicio ("1234567 FRESA").
	codeRe   = regexp.MustCompile(`^\d{4,}\s+`)
	dateRes  = []*regexp.Regexp{regexp.MustCompile(`\b(\d{4})-(\d{2})-(\d{2})\b`), regexp.MustCompile(`\b(\d{1,2})[/.-](\d{1,2})[/.-](\d{2,4})\b`)}
	monthRe  = regexp.MustCompile(`(?i)\b(\d{1,2})[\s/.-]*(ene|feb|mar|abr|may|jun|jul|ago|sep|set|oct|nov|dic)[a-z]*[\s/.-]*(\d{2,4})\b`)
	months   = map[string]int{"ene": 1, "feb": 2, "mar": 3, "abr": 4, "may": 5, "jun": 6, "jul": 7, "ago": 8, "sep": 9, "set": 9, "oct": 10, "nov": 11, "dic": 12}
	wordSepR = regexp.MustCompile(`[^a-z0-9ñ]+`)
)

// Palabras de renglones que no son productos (totales, pagos, datos fiscales).
var notItem = map[string]bool{
	"total": true, "subtotal": true, "sub": true, "iva": true, "ieps": true, "impuesto": true, "impuestos": true,
	"cambio": true, "efectivo": true, "tarjeta": true, "visa": true, "mastercard": true, "amex": true,
	"debito": true, "credito": true, "pago": true, "pagado": true, "propina": true, "saldo": true,
	"ahorro": true, "ahorraste": true, "descuento": true, "articulos": true, "rfc": true, "folio": true,
	"cajero": true, "autorizacion": true, "aprobado": true, "recibido": true, "redondeo": true, "monedero": true,
	"puntos": true, "transaccion": true, "referencia": true,
}

func norm(s string) string {
	s = strings.NewReplacer("á", "a", "é", "e", "í", "i", "ó", "o", "ú", "u", "ü", "u",
		"Á", "a", "É", "e", "Í", "i", "Ó", "o", "Ú", "u", "Ü", "u").Replace(s)
	return strings.ToLower(s)
}

// Words son las palabras de un texto, sin acentos ni signos.
func Words(s string) []string {
	return strings.Fields(wordSepR.ReplaceAllString(norm(s), " "))
}

func cents(neg, whole, dec string) int64 {
	w, _ := strconv.ParseInt(strings.ReplaceAll(whole, ",", ""), 10, 64)
	d, _ := strconv.ParseInt(dec, 10, 64)
	c := w*100 + d
	if neg == "-" {
		c = -c
	}
	return c
}

// parseFloat lee "92.50", "0,850" o "1,234.50".
func parseFloat(s string) float64 {
	if strings.Contains(s, ".") {
		s = strings.ReplaceAll(s, ",", "")
	} else {
		s = strings.Replace(s, ",", ".", 1)
	}
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

// Parse interpreta los renglones de un ticket.
func Parse(rows []string) Receipt {
	var r Receipt
	for i, row := range rows {
		if i >= 6 {
			break
		}
		if t := strings.TrimSpace(row); t != "" {
			r.Header = append(r.Header, t)
		}
	}
	r.Date = findDate(rows)

	// Un renglón "2 X 92.50" da la cantidad del producto que va junto (antes
	// o después, según la tienda): se asigna al vecino cuyo importe cuadra.
	type pendingQty struct {
		qty, unit float64
		weight    bool
		row       int
	}
	var pending *pendingQty
	var subtotal int64
	for i, raw := range rows {
		row := strings.TrimSpace(raw)
		if m := qtyRe.FindStringSubmatch(row); m != nil {
			q := pendingQty{qty: parseFloat(m[1]), unit: parseFloat(m[3]),
				weight: m[2] != "" && strings.HasPrefix(strings.ToLower(m[2]), "k"), row: i}
			// ¿Es del producto anterior?
			if n := len(r.Lines); n > 0 && matches(r.Lines[n-1].AmountCents, q.qty, q.unit) && r.Lines[n-1].Qty == 1 {
				r.Lines[n-1].Qty, r.Lines[n-1].ByWeight = q.qty, q.weight
				continue
			}
			pending = &q
			continue
		}
		m := amountRe.FindStringSubmatchIndex(row)
		if m == nil {
			continue
		}
		amount := cents(row[m[2]:m[3]], row[m[4]:m[5]], row[m[6]:m[7]])
		desc := strings.TrimSpace(codeRe.ReplaceAllString(strings.TrimSpace(row[:m[0]]), ""))
		// Solo el importe: la descripción quedó en el renglón de arriba.
		if desc == "" && i > 0 && amountRe.FindStringIndex(strings.TrimSpace(rows[i-1])) == nil &&
			qtyRe.FindStringIndex(strings.TrimSpace(rows[i-1])) == nil && i-1 > 0 {
			desc = strings.TrimSpace(codeRe.ReplaceAllString(strings.TrimSpace(rows[i-1]), ""))
		}
		words := Words(desc)
		if isTotal(words) {
			if r.TotalCents == 0 && amount > 0 {
				r.TotalCents = amount
			}
			continue
		}
		if len(words) > 0 && words[0] == "subtotal" {
			subtotal = amount
			continue
		}
		if skip(words) || amount <= 0 || !hasLetters(desc) {
			continue
		}
		line := Line{Text: desc, Qty: 1, AmountCents: amount}
		if pending != nil && i-pending.row <= 2 && matches(amount, pending.qty, pending.unit) {
			line.Qty, line.ByWeight = pending.qty, pending.weight
		}
		pending = nil
		r.Lines = append(r.Lines, line)
	}
	if r.TotalCents == 0 {
		r.TotalCents = subtotal
	}
	return r
}

// matches: cantidad × precio unitario ≈ importe (±2 centavos).
func matches(amount int64, qty, unit float64) bool {
	return math.Abs(float64(amount)-math.Round(qty*unit*100)) <= 2
}

func isTotal(words []string) bool {
	if len(words) == 0 || words[0] != "total" {
		return false
	}
	// "TOTAL DE ARTICULOS 5" no es dinero.
	for _, w := range words {
		if w == "articulos" || w == "piezas" || w == "ahorro" {
			return false
		}
	}
	return true
}

func skip(words []string) bool {
	for _, w := range words {
		if notItem[w] {
			return true
		}
	}
	return false
}

func hasLetters(s string) bool {
	for _, c := range s {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c > 127 {
			return true
		}
	}
	return false
}

// findDate busca la fecha de compra (formato mexicano día/mes/año).
func findDate(rows []string) string {
	valid := func(y, m, d int) string {
		if y < 100 {
			y += 2000
		}
		t := time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC)
		if y < 2000 || y > 2100 || t.Month() != time.Month(m) || t.Day() != d {
			return ""
		}
		return t.Format("2006-01-02")
	}
	atoi := func(s string) int { n, _ := strconv.Atoi(s); return n }
	for _, row := range rows {
		if m := dateRes[0].FindStringSubmatch(row); m != nil {
			if d := valid(atoi(m[1]), atoi(m[2]), atoi(m[3])); d != "" {
				return d
			}
		}
		if m := dateRes[1].FindStringSubmatch(row); m != nil {
			if d := valid(atoi(m[3]), atoi(m[2]), atoi(m[1])); d != "" {
				return d
			}
		}
		if m := monthRe.FindStringSubmatch(row); m != nil {
			if d := valid(atoi(m[3]), months[strings.ToLower(m[2])], atoi(m[1])); d != "" {
				return d
			}
		}
	}
	return ""
}
