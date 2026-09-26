package printer

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
)

// DotsFor regresa los puntos imprimibles por línea para un ancho de papel
// (203 dpi): 384 en 58 mm y 576 en 80 mm.
func DotsFor(paperWidthMM int) int {
	if paperWidthMM >= 80 {
		return 576
	}
	return 384
}

// Dither convierte una imagen a blanco y negro de widthDots puntos de ancho
// (manteniendo la proporción) con difusión de error Floyd–Steinberg, para
// que los tonos intermedios se vean como tramas en una térmica. Las zonas
// transparentes quedan blancas.
func Dither(src image.Image, widthDots int) *image.Gray {
	b := src.Bounds()
	if b.Dx() == 0 || b.Dy() == 0 || widthDots < 8 {
		return image.NewGray(image.Rect(0, 0, 0, 0))
	}
	w := widthDots
	h := b.Dy() * w / b.Dx()
	if h < 1 {
		h = 1
	}

	// Escala por promedio de área: cada punto destino promedia los píxeles
	// origen que cubre (se ve mucho mejor que tomar el vecino más cercano).
	lum := make([]float64, w*h)
	for y := 0; y < h; y++ {
		sy0, sy1 := b.Min.Y+y*b.Dy()/h, b.Min.Y+(y+1)*b.Dy()/h
		if sy1 <= sy0 {
			sy1 = sy0 + 1
		}
		for x := 0; x < w; x++ {
			sx0, sx1 := b.Min.X+x*b.Dx()/w, b.Min.X+(x+1)*b.Dx()/w
			if sx1 <= sx0 {
				sx1 = sx0 + 1
			}
			var sum float64
			n := 0
			for sy := sy0; sy < sy1; sy++ {
				for sx := sx0; sx < sx1; sx++ {
					r, g, bl, a := src.At(sx, sy).RGBA()
					// Sobre fondo blanco: lo transparente cuenta como blanco.
					l := (0.299*float64(r) + 0.587*float64(g) + 0.114*float64(bl)) / 65535
					alpha := float64(a) / 65535
					sum += l*alpha + (1 - alpha)
					n++
				}
			}
			lum[y*w+x] = sum / float64(n)
		}
	}

	out := image.NewGray(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := y*w + x
			old := lum[i]
			var v float64
			if old >= 0.5 {
				v = 1
			}
			if v == 1 {
				out.SetGray(x, y, color.Gray{Y: 255})
			} else {
				out.SetGray(x, y, color.Gray{Y: 0})
			}
			e := old - v
			if x+1 < w {
				lum[i+1] += e * 7 / 16
			}
			if y+1 < h {
				if x > 0 {
					lum[i+w-1] += e * 3 / 16
				}
				lum[i+w] += e * 5 / 16
				if x+1 < w {
					lum[i+w+1] += e * 1 / 16
				}
			}
		}
	}
	return out
}

// Image imprime una imagen ya en blanco y negro (ver Dither), centrada en el
// papel. pct es el ancho relativo al papel, solo para la vista previa.
//
// Se usa el modo de imagen por columnas (ESC * de 24 puntos) en franjas de
// 24 renglones: lo soportan prácticamente todas las térmicas ESC/POS. El modo
// raster (GS v 0) es más compacto, pero las impresoras genéricas (58 mm tipo
// H58/POS-58) lo imprimen a medias o como texto basura. El centrado se hace
// rellenando con blanco porque varias ignoran la alineación en imágenes.
func (t *Ticket) Image(img *image.Gray, pct int) *Ticket {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	if w == 0 || h == 0 {
		return t
	}
	left, total := 0, w
	if t.dots > w {
		total = t.dots
		switch t.align {
		case AlignCenter:
			left = (t.dots - w) / 2
		case AlignRight:
			left = t.dots - w
		}
	}
	black := func(x, y int) bool {
		x -= left
		return x >= 0 && x < w && y < h && img.GrayAt(x, y).Y < 128
	}

	t.buf.Write([]byte{0x1b, '3', 24}) // interlineado de 24 puntos: franjas sin huecos
	for y0 := 0; y0 < h; y0 += 24 {
		t.buf.Write([]byte{0x1b, '*', 33, byte(total), byte(total >> 8)})
		for x := 0; x < total; x++ {
			for k := 0; k < 3; k++ { // 3 bytes por columna, de arriba hacia abajo
				var b byte
				for bit := 0; bit < 8; bit++ {
					if black(x, y0+k*8+bit) {
						b |= 0x80 >> bit
					}
				}
				t.buf.WriteByte(b)
			}
		}
		t.buf.WriteByte('\n')
	}
	t.buf.Write([]byte{0x1b, '2'}) // interlineado normal

	var png64 bytes.Buffer
	png.Encode(&png64, img)
	t.preview = append(t.preview, PreviewLine{
		Kind: "image", Align: alignNames[t.align], Pct: pct,
		Image: "data:image/png;base64," + base64.StdEncoding.EncodeToString(png64.Bytes()),
	})
	return t
}

// FitWidth regresa el ancho en puntos para imprimir src a lo más maxWidth de
// ancho y maxHeight de alto, conservando la proporción.
func FitWidth(src image.Image, maxWidth, maxHeight int) int {
	b := src.Bounds()
	if b.Dx() == 0 || b.Dy() == 0 {
		return 0
	}
	w := maxWidth
	if h := b.Dy() * w / b.Dx(); h > maxHeight {
		w = maxHeight * b.Dx() / b.Dy()
	}
	return w
}
