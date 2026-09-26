package printer

import (
	"bytes"
	"image"
	"image/color"
	"testing"
)

func TestEncodePC850(t *testing.T) {
	got := encodePC850("Café Ñ ¿¡ €")
	want := []byte{'C', 'a', 'f', 0x82, ' ', 0xa5, ' ', 0xa8, 0xad, ' ', '?'}
	if !bytes.Equal(got, want) {
		t.Errorf("got % x, want % x", got, want)
	}
}

func TestColumns(t *testing.T) {
	tk := &Ticket{cols: 20}
	tk.Columns("Latte", "$65.00")
	if got := tk.buf.String(); got != "Latte         $65.00\n" {
		t.Errorf("got %q", got)
	}

	tk = &Ticket{cols: 16}
	tk.Columns("Chilaquiles Verdes", "$120.00")
	if got := tk.buf.String(); got != "Chilaqui $120.00\n" {
		t.Errorf("la línea debe medir exactamente 16 columnas, got %q", got)
	}
}

func TestColumnsFor(t *testing.T) {
	if ColumnsFor(58) != 32 || ColumnsFor(80) != 48 {
		t.Errorf("columnas incorrectas: 58→%d 80→%d", ColumnsFor(58), ColumnsFor(80))
	}
}

func TestValidDeviceURI(t *testing.T) {
	valid := []string{"usb://Thermal%20Printer/H58%20Printer%20USB?serial=Printer", "usb://EPSON/TM-T20"}
	invalid := []string{"", "usb://", "socket://192.168.1.5", "usb://a b", "usb://x\n", "file:///etc/passwd"}
	for _, u := range valid {
		if !ValidDeviceURI(u) {
			t.Errorf("debería ser válida: %q", u)
		}
	}
	for _, u := range invalid {
		if ValidDeviceURI(u) {
			t.Errorf("debería ser inválida: %q", u)
		}
	}
}

func TestImageRaster(t *testing.T) {
	// Imagen de 16x2: mitad izquierda negra, derecha blanca.
	src := image.NewGray(image.Rect(0, 0, 16, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 16; x++ {
			if x >= 8 {
				src.SetGray(x, y, color.Gray{Y: 255})
			}
		}
	}
	img := Dither(src, 16)
	tk := &Ticket{cols: 32}
	tk.Image(img, 50)
	// ESC 3 24, luego una franja ESC * 33 de 16 columnas × 3 bytes: las 8
	// primeras con los dos renglones de arriba negros (0xC0), el resto en blanco.
	want := []byte{0x1b, '3', 24, 0x1b, '*', 33, 16, 0}
	for x := 0; x < 16; x++ {
		if x < 8 {
			want = append(want, 0xc0, 0, 0)
		} else {
			want = append(want, 0, 0, 0)
		}
	}
	want = append(want, '\n', 0x1b, '2')
	if !bytes.Equal(tk.buf.Bytes(), want) {
		t.Fatalf("imagen por columnas: % x", tk.buf.Bytes())
	}
	if p := tk.Preview(); len(p) != 1 || p[0].Kind != "image" || p[0].Pct != 50 {
		t.Fatalf("vista previa: %+v", p)
	}
}

func TestPreviewTracksStyle(t *testing.T) {
	tk := NewTicket(58)
	tk.Align(AlignCenter).Bold(true).Size(2, 2).Line("HOLA").Size(1, 1).Bold(false).Align(AlignLeft)
	tk.Size(2, 1).Columns("A", "$1")
	p := tk.Preview()
	if p[0].Text != "HOLA" || p[0].W != 2 || !p[0].Bold || p[0].Align != "center" {
		t.Fatalf("línea 1: %+v", p[0])
	}
	// A doble ancho caben 16 columnas en papel de 58 mm.
	if len(p[1].Text) != 16 || p[1].W != 2 {
		t.Fatalf("columnas a doble ancho: %q", p[1].Text)
	}
}

func TestImageCenteredAndBanded(t *testing.T) {
	// 50 renglones = 3 franjas de 24; en 58 mm cada franja ocupa los 384 puntos.
	img := Dither(image.NewGray(image.Rect(0, 0, 100, 50)), 100)
	tk := NewTicket(58)
	start := tk.buf.Len()
	tk.Align(AlignCenter).Image(img, 26)
	out := tk.buf.Bytes()[start+3:]
	if n := bytes.Count(out, []byte{0x1b, '*', 33, 0x80, 0x01}); n != 3 {
		t.Fatalf("se esperaban 3 franjas de 384 puntos, hubo %d", n)
	}
	if FitWidth(img, 384, 240) != 384 || FitWidth(image.NewGray(image.Rect(0, 0, 100, 400)), 384, 240) != 60 {
		t.Fatal("FitWidth debe limitar la altura conservando la proporción")
	}
}
