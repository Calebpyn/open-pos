// Package printer genera tickets ESC/POS y los envía a impresoras térmicas,
// ya sea por USB (a través de una cola de CUPS) o por red (TCP 9100).
package printer

import (
	"bytes"
	"strings"
	"unicode/utf8"
)

type Align byte

const (
	AlignLeft   Align = 0
	AlignCenter Align = 1
	AlignRight  Align = 2
)

// Ticket construye un documento ESC/POS y, en paralelo, una vista previa de
// lo que se imprimirá (para el editor de impresiones).
type Ticket struct {
	buf  bytes.Buffer
	cols int // caracteres por línea con la fuente normal
	dots int // puntos por línea (para centrar imágenes)

	align   Align
	bold    bool
	w, h    int
	preview []PreviewLine
}

// PreviewLine es un renglón de la vista previa: texto con su estilo, una
// imagen, un avance de papel o el corte.
type PreviewLine struct {
	Kind  string `json:"kind"` // text | image | feed | cut
	Text  string `json:"text,omitempty"`
	W     int    `json:"w,omitempty"`
	H     int    `json:"h,omitempty"`
	Bold  bool   `json:"bold,omitempty"`
	Align string `json:"align,omitempty"` // left | center | right
	Lines int    `json:"lines,omitempty"` // feed
	Image string `json:"image,omitempty"` // PNG en data URL
	Pct   int    `json:"pct,omitempty"`   // ancho de la imagen, % del papel
}

var alignNames = map[Align]string{AlignLeft: "left", AlignCenter: "center", AlignRight: "right"}

// ColumnsFor regresa los caracteres por línea (fuente A) para un ancho de papel.
func ColumnsFor(paperWidthMM int) int {
	if paperWidthMM >= 80 {
		return 48
	}
	return 32
}

// NewTicket empieza un documento dejando la impresora en un estado conocido.
//
// No se usa ESC @ (reiniciar): las térmicas económicas descartan lo que les
// llega mientras se reinician, y si el documento anterior todavía se está
// imprimiendo se perdían el encabezado y los primeros renglones. En su lugar
// se restablecen explícitamente los estilos que usa el POS.
func NewTicket(paperWidthMM int) *Ticket {
	t := &Ticket{cols: ColumnsFor(paperWidthMM), dots: DotsFor(paperWidthMM), w: 1, h: 1}
	t.buf.Write([]byte{
		0x1b, 't', 2, // ESC t 2: página de códigos PC850 (acentos, ñ, ¿, ¡)
		0x1b, 'a', 0, // alineación izquierda
		0x1b, 'E', 0, // sin negrita
		0x1d, '!', 0, // tamaño normal
		0x1b, '2', // interlineado normal
	})
	return t
}

func (t *Ticket) Cols() int { return t.cols }

// width es cuántas columnas caben con el tamaño de letra actual.
func (t *Ticket) width() int {
	if t.w > 1 {
		return t.cols / t.w
	}
	return t.cols
}

func (t *Ticket) Align(a Align) *Ticket {
	t.align = a
	t.buf.Write([]byte{0x1b, 'a', byte(a)})
	return t
}

func (t *Ticket) Bold(on bool) *Ticket {
	t.bold = on
	t.buf.Write([]byte{0x1b, 'E', boolByte(on)})
	return t
}

// Size ajusta el tamaño de letra; 1 es normal, 2 es doble.
func (t *Ticket) Size(width, height int) *Ticket {
	t.w, t.h = clamp(width, 1, 8), clamp(height, 1, 8)
	t.buf.Write([]byte{0x1d, '!', byte(t.w-1)<<4 | byte(t.h-1)})
	return t
}

// Line escribe texto seguido de salto de línea.
func (t *Ticket) Line(s string) *Ticket {
	t.buf.Write(encodePC850(s))
	t.buf.WriteByte('\n')
	w, h := t.w, t.h
	if w < 1 {
		w, h = 1, 1
	}
	t.preview = append(t.preview, PreviewLine{Kind: "text", Text: s, W: w, H: h, Bold: t.bold, Align: alignNames[t.align]})
	return t
}

// Columns escribe texto a la izquierda y a la derecha en la misma línea.
func (t *Ticket) Columns(left, right string) *Ticket {
	cols := t.width()
	space := cols - utf8.RuneCountInString(left) - utf8.RuneCountInString(right)
	if space < 1 {
		// No cabe: la izquierda se recorta para que el importe siempre se vea.
		keep := cols - utf8.RuneCountInString(right) - 1
		if keep < 0 {
			keep = 0
		}
		left = truncateRunes(left, keep)
		space = cols - utf8.RuneCountInString(left) - utf8.RuneCountInString(right)
	}
	return t.Line(left + strings.Repeat(" ", space) + right)
}

func (t *Ticket) Separator() *Ticket {
	return t.Line(strings.Repeat("-", t.width()))
}

func (t *Ticket) Feed(lines int) *Ticket {
	lines = clamp(lines, 0, 255)
	t.buf.Write([]byte{0x1b, 'd', byte(lines)})
	t.preview = append(t.preview, PreviewLine{Kind: "feed", Lines: lines})
	return t
}

// Cut hace un corte parcial (GS V 1). Se usa la forma de un solo parámetro:
// la de avance (GS V 66 n) no la entienden las impresoras sin cortadora y
// imprimían su parámetro como una "B" suelta. El avance lo pone Feed.
func (t *Ticket) Cut() *Ticket {
	t.buf.Write([]byte{0x1d, 'V', 1})
	t.preview = append(t.preview, PreviewLine{Kind: "cut"})
	return t
}

func (t *Ticket) Bytes() []byte { return t.buf.Bytes() }

// Preview regresa lo que se imprimirá, renglón por renglón.
func (t *Ticket) Preview() []PreviewLine { return t.preview }

// DrawerKick abre el cajón de dinero conectado al puerto RJ11 de la
// impresora. Se manda el pulso a los dos conectores (pin 2 y pin 5) porque
// cada cajón usa uno u otro; el que no tiene cajón lo ignora.
func DrawerKick() []byte {
	return []byte{
		// 50 ms encendido basta para el solenoide; el tiempo apagado se deja
		// corto (100 ms) porque la impresora no lee datos durante el pulso.
		0x1b, 'p', 0, 25, 50, // ESC p 0: pin 2
		0x1b, 'p', 1, 25, 50, // ESC p 1: pin 5
	}
}

// pc850 cubre los caracteres del español que no son ASCII.
var pc850 = map[rune]byte{
	'á': 0xa0, 'é': 0x82, 'í': 0xa1, 'ó': 0xa2, 'ú': 0xa3,
	'Á': 0xb5, 'É': 0x90, 'Í': 0xd6, 'Ó': 0xe0, 'Ú': 0xe9,
	'ñ': 0xa4, 'Ñ': 0xa5, 'ü': 0x81, 'Ü': 0x9a,
	'¿': 0xa8, '¡': 0xad, '°': 0xf8, '·': 0xfa,
}

func encodePC850(s string) []byte {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		switch {
		case r == '\t':
			out = append(out, ' ')
		case r < 0x80:
			out = append(out, byte(r))
		default:
			if b, ok := pc850[r]; ok {
				out = append(out, b)
			} else {
				out = append(out, '?')
			}
		}
	}
	return out
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

func boolByte(b bool) byte {
	if b {
		return 1
	}
	return 0
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
