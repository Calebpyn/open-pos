// Package ocr lee tickets de compra: corre el OCR de macOS (open-pos-ocr,
// Vision, sin internet), junta los textos en renglones e interpreta tienda,
// fecha, total y productos.
package ocr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Box es un texto que encontró el OCR, con su posición (0 a 1 desde la
// esquina superior izquierda).
type Box struct {
	Text string  `json:"text"`
	X    float64 `json:"x"`
	Y    float64 `json:"y"`
	W    float64 `json:"w"`
	H    float64 `json:"h"`
	Conf float64 `json:"conf"`
}

// ErrUnavailable: este equipo no tiene el lector (no es Mac o no se compiló).
var ErrUnavailable = errors.New("el lector de tickets no está disponible en este equipo")

// binPath busca open-pos-ocr: POS_OCR_BIN, junto al ejecutable (en la app de
// Mac) o en bin/ (desarrollo, scripts/build-ocr.sh).
func binPath() string {
	if p := os.Getenv("POS_OCR_BIN"); p != "" {
		return p
	}
	var candidates []string
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "open-pos-ocr"))
	}
	candidates = append(candidates, filepath.Join("bin", "open-pos-ocr"))
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c
		}
	}
	return ""
}

// Available indica si se pueden leer tickets en este equipo.
func Available() bool { return binPath() != "" }

// Recognize lee el texto de una imagen.
func Recognize(ctx context.Context, imagePath string) ([]Box, error) {
	bin := binPath()
	if bin == "" {
		return nil, ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var out, errOut bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, imagePath)
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("OCR: %v %s", err, strings.TrimSpace(errOut.String()))
	}
	var res struct {
		Lines []Box `json:"lines"`
	}
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		return nil, fmt.Errorf("OCR: respuesta inválida: %w", err)
	}
	return res.Lines, nil
}

// Rows junta los textos que van a la misma altura (el producto a la
// izquierda y su precio a la derecha) en renglones, de arriba abajo. Tolera
// fotos un poco chuecas.
func Rows(boxes []Box) []string {
	type row struct {
		cy, h float64
		boxes []Box
	}
	sorted := append([]Box(nil), boxes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Y+sorted[i].H/2 < sorted[j].Y+sorted[j].H/2 })
	var rows []*row
	for _, b := range sorted {
		if strings.TrimSpace(b.Text) == "" {
			continue
		}
		cy := b.Y + b.H/2
		var best *row
		for _, r := range rows {
			tol := 0.6 * max(r.h, b.H)
			if d := abs(cy - r.cy); d <= tol && (best == nil || d < abs(cy-best.cy)) {
				best = r
			}
		}
		if best == nil {
			rows = append(rows, &row{cy: cy, h: b.H, boxes: []Box{b}})
			continue
		}
		n := float64(len(best.boxes))
		best.cy = (best.cy*n + cy) / (n + 1)
		best.h = max(best.h, b.H)
		best.boxes = append(best.boxes, b)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].cy < rows[j].cy })
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		sort.Slice(r.boxes, func(i, j int) bool { return r.boxes[i].X < r.boxes[j].X })
		parts := make([]string, len(r.boxes))
		for i, b := range r.boxes {
			parts[i] = strings.TrimSpace(b.Text)
		}
		out = append(out, strings.Join(parts, " "))
	}
	return out
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
