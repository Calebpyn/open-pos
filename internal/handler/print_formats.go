package handler

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif" // formatos aceptados para el logo
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/calebpyn/open-pos/internal/printer"
)

// Tamaños de letra que ofrece el editor, como (ancho, alto) de ESC/POS.
var textSizes = map[string][2]int{
	"normal": {1, 1},
	"alta":   {1, 2}, // doble alto: más legible sin perder columnas
	"ancha":  {2, 1},
	"grande": {2, 2},
	"enorme": {3, 3},
}

func sizeOf(name string) (int, int) {
	if s, ok := textSizes[name]; ok {
		return s[0], s[1]
	}
	return 1, 1
}

// ReceiptFormat es cómo se ve el ticket de compra.
type ReceiptFormat struct {
	ShowLogo      bool   `json:"show_logo"`
	LogoWidthPct  int    `json:"logo_width_pct"` // % del ancho del papel
	ShowName      bool   `json:"show_name"`
	NameSize      string `json:"name_size"`
	HeaderLines   string `json:"header_lines"` // dirección, teléfono, RFC... una por renglón
	ShowDate      bool   `json:"show_date"`
	ShowOrderInfo bool   `json:"show_order_info"` // orden, cuenta, mesa y cliente
	ItemSize      string `json:"item_size"`
	ShowItemNotes bool   `json:"show_item_notes"` // modificadores y notas bajo cada producto
	TotalSize     string `json:"total_size"`
	ShowTax       bool   `json:"show_tax"`
	ShowPayments  bool   `json:"show_payments"`
	FooterLines   string `json:"footer_lines"`
	FeedLines     int    `json:"feed_lines"` // avance antes del corte
}

// ComandaFormat es cómo se ven las comandas de un área de producción.
type ComandaFormat struct {
	TitleSize     string `json:"title_size"` // COMANDA / ADICIONAL
	ShowAreaName  bool   `json:"show_area_name"`
	LocationSize  string `json:"location_size"`   // mesa o cliente
	ShowOrderInfo bool   `json:"show_order_info"` // número de orden y hora
	ItemSize      string `json:"item_size"`
	ModifierSize  string `json:"modifier_size"`
	NoteSize      string `json:"note_size"`
	OrderNoteSize string `json:"order_note_size"` // nota general de la orden
	Copies        int    `json:"copies"`
	FeedLines     int    `json:"feed_lines"`
	// Cuentas por persona: una comanda por persona en vez de una con
	// secciones "— Juan —".
	GuestTickets bool `json:"guest_tickets"`
}

// Valores por omisión: reproducen lo que se imprimía antes del editor.
func defaultReceiptFormat(s Settings) ReceiptFormat {
	return ReceiptFormat{
		ShowLogo: true, LogoWidthPct: 60, ShowName: true, NameSize: "grande",
		ShowDate: true, ShowOrderInfo: true, ItemSize: "normal", ShowItemNotes: true,
		TotalSize: "normal", ShowTax: true, ShowPayments: true,
		FooterLines: s.TicketFooter, FeedLines: 3,
	}
}

func defaultComandaFormat() ComandaFormat {
	return ComandaFormat{
		TitleSize: "grande", ShowAreaName: true, LocationSize: "alta", ShowOrderInfo: true,
		ItemSize: "alta", ModifierSize: "normal", NoteSize: "normal", OrderNoteSize: "normal",
		Copies: 1, FeedLines: 3,
	}
}

func checkSize(names ...string) error {
	for _, n := range names {
		if _, ok := textSizes[n]; !ok {
			return fmt.Errorf("Tamaño de letra inválido: %q", n)
		}
	}
	return nil
}

func checkLines(label, text string) error {
	lines := strings.Split(text, "\n")
	if len(lines) > 10 {
		return fmt.Errorf("%s: máximo 10 renglones", label)
	}
	for _, l := range lines {
		if len([]rune(l)) > 64 {
			return fmt.Errorf("%s: cada renglón puede tener hasta 64 caracteres", label)
		}
	}
	return nil
}

func (f *ReceiptFormat) validate() error {
	f.HeaderLines, f.FooterLines = strings.TrimSpace(f.HeaderLines), strings.TrimSpace(f.FooterLines)
	if err := checkSize(f.NameSize, f.ItemSize, f.TotalSize); err != nil {
		return err
	}
	if f.LogoWidthPct < 20 || f.LogoWidthPct > 100 {
		return errors.New("El ancho del logo debe estar entre 20% y 100%")
	}
	if f.FeedLines < 0 || f.FeedLines > 10 {
		return errors.New("El avance antes del corte debe estar entre 0 y 10 renglones")
	}
	if err := checkLines("Encabezado", f.HeaderLines); err != nil {
		return err
	}
	return checkLines("Pie del ticket", f.FooterLines)
}

func (f *ComandaFormat) validate() error {
	if err := checkSize(f.TitleSize, f.LocationSize, f.ItemSize, f.ModifierSize, f.NoteSize, f.OrderNoteSize); err != nil {
		return err
	}
	if f.Copies < 1 || f.Copies > 3 {
		return errors.New("Las copias deben ser de 1 a 3")
	}
	if f.FeedLines < 0 || f.FeedLines > 10 {
		return errors.New("El avance antes del corte debe estar entre 0 y 10 renglones")
	}
	return nil
}

// loadFormat aplica sobre dst lo guardado en key (lo que falte conserva el
// valor por omisión que ya trae dst).
func loadFormat(ctx context.Context, q queryer, key string, dst any) error {
	var raw string
	err := q.QueryRowContext(ctx, `SELECT config FROM print_formats WHERE key = ?`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(raw), dst)
}

func loadReceiptFormat(ctx context.Context, q queryer) (ReceiptFormat, error) {
	s, err := loadSettings(ctx, q)
	if err != nil {
		return ReceiptFormat{}, err
	}
	f := defaultReceiptFormat(s)
	return f, loadFormat(ctx, q, "receipt", &f)
}

func comandaKey(areaID int64) string { return "comanda:" + strconv.FormatInt(areaID, 10) }

func loadComandaFormat(ctx context.Context, q queryer, areaID int64) (ComandaFormat, error) {
	f := defaultComandaFormat()
	return f, loadFormat(ctx, q, comandaKey(areaID), &f)
}

// loadLogo regresa el logo del ticket o nil si no hay.
func loadLogo(ctx context.Context, q queryer) image.Image {
	var data []byte
	if err := q.QueryRowContext(ctx, `SELECT data FROM print_formats WHERE key = 'logo'`).Scan(&data); err != nil || len(data) == 0 {
		return nil
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	return img
}

// --- Muestras para la vista previa y la impresión de prueba ---

func sampleTicket(s Settings, f ReceiptFormat) *TicketData {
	t := &TicketData{
		Settings: s, BillNumber: 1, OrderID: 128, OrderType: "DINE_IN", TableName: "Mesa 3",
		CustomerName: "", PaidAt: time.Now(),
		Items: []TicketItem{
			{Name: "Latte", Quantity: 2, Amount: 164, Modifiers: "Leche de avena · Vainilla"},
			{Name: "Chilaquiles Verdes", Quantity: 1, Amount: 120, Notes: "Sin crema"},
		},
		Subtotal: 284, Total: 284, Tip: 28,
		Payments: []TicketPayment{{Method: "Efectivo", Amount: 312, Received: 500, Change: 188}},
		Change:   188,
	}
	t.TicketFooter = f.FooterLines
	t.Tax = fromCents(toCents(t.Total) - int64(float64(toCents(t.Total))/(1+s.TaxRate)+0.5))
	t.Grand = fromCents(toCents(t.Total) + toCents(t.Tip))
	return t
}

func sampleComanda() (comandaHeader, []comandaLine) {
	return comandaHeader{OrderID: 128, OrderType: "DINE_IN", TableName: "Mesa 3", Notes: "Cumpleaños: sacar el postre con vela"},
		[]comandaLine{
			{Name: "Latte", Quantity: 2, Modifiers: "Leche de avena · Vainilla", Notes: "Extra caliente"},
			{Name: "Chilaquiles Verdes", Quantity: 1, Notes: "Sin crema"},
		}
}

// --- API del editor ---

type formatArea struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	ColorHex string `json:"color_hex"`
}

// GET /api/admin/print-formats
func (h *POSHandler) AdminGetPrintFormats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	receipt, err := loadReceiptFormat(ctx, h.DB)
	if err != nil {
		serverError(w, "Error cargando el formato del ticket", err)
		return
	}
	areas := []formatArea{}
	comandas := map[string]ComandaFormat{}
	if err := eachRow(ctx, h.DB, `SELECT id, name, color_hex FROM production_areas WHERE is_active = 1 ORDER BY sort_order, id`, nil,
		func(scan func(...any) error) error {
			var a formatArea
			if err := scan(&a.ID, &a.Name, &a.ColorHex); err != nil {
				return err
			}
			areas = append(areas, a)
			return nil
		}); err != nil {
		serverError(w, "Error cargando áreas", err)
		return
	}
	for _, a := range areas {
		f, err := loadComandaFormat(ctx, h.DB, a.ID)
		if err != nil {
			serverError(w, "Error cargando formatos de comanda", err)
			return
		}
		comandas[strconv.FormatInt(a.ID, 10)] = f
	}
	printers, err := loadPrinters(ctx, h.DB)
	if err != nil {
		serverError(w, "Error cargando impresoras", err)
		return
	}
	type printerOpt struct {
		ID         int64  `json:"id"`
		Name       string `json:"name"`
		PaperWidth int    `json:"paper_width"`
	}
	opts := []printerOpt{}
	for _, p := range printers {
		if p.IsActive {
			opts = append(opts, printerOpt{p.ID, p.Name, p.PaperWidth})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"receipt": receipt, "comandas": comandas, "areas": areas, "printers": opts,
		"has_logo": loadLogo(ctx, h.DB) != nil, "sizes": []string{"normal", "alta", "ancha", "grande", "enorme"},
		"receipt_default": defaultReceiptFormat(Settings{}), "comanda_default": defaultComandaFormat(),
	})
}

func (h *POSHandler) saveFormat(w http.ResponseWriter, r *http.Request, key, action string, f any) {
	raw, _ := json.Marshal(f)
	h.adminWrite(w, r, action, func(tx *sql.Tx) (map[string]any, error) {
		_, err := tx.ExecContext(r.Context(), `
			INSERT INTO print_formats (key, config, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
			ON CONFLICT(key) DO UPDATE SET config = excluded.config, updated_at = CURRENT_TIMESTAMP`, key, string(raw))
		return map[string]any{"key": key, "format": f}, err
	})
}

// PUT /api/admin/print-formats/receipt
func (h *POSHandler) AdminSaveReceiptFormat(w http.ResponseWriter, r *http.Request) {
	var f ReceiptFormat
	if err := json.NewDecoder(r.Body).Decode(&f); err != nil {
		http.Error(w, "JSON inválido", http.StatusBadRequest)
		return
	}
	if err := f.validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	h.saveFormat(w, r, "receipt", "PRINT_FORMAT_UPDATED", f)
}

// PUT /api/admin/print-formats/comanda/{id}
func (h *POSHandler) AdminSaveComandaFormat(w http.ResponseWriter, r *http.Request) {
	areaID, ok := pathID(r)
	if !ok {
		http.Error(w, "Área inválida", http.StatusBadRequest)
		return
	}
	var f ComandaFormat
	if err := json.NewDecoder(r.Body).Decode(&f); err != nil {
		http.Error(w, "JSON inválido", http.StatusBadRequest)
		return
	}
	if err := f.validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var n int
	h.DB.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM production_areas WHERE id = ?`, areaID).Scan(&n)
	if n == 0 {
		http.Error(w, "El área no existe", http.StatusNotFound)
		return
	}
	h.saveFormat(w, r, comandaKey(areaID), "PRINT_FORMAT_UPDATED", f)
}

const maxLogoBytes = 2 << 20

// POST /api/admin/print-formats/logo (multipart, campo "logo")
func (h *POSHandler) AdminUploadLogo(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxLogoBytes+64<<10)
	file, _, err := r.FormFile("logo")
	if err != nil {
		http.Error(w, "Sube una imagen PNG o JPG de hasta 2 MB", http.StatusBadRequest)
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxLogoBytes+1))
	if err != nil || len(data) > maxLogoBytes {
		http.Error(w, "La imagen pesa más de 2 MB", http.StatusBadRequest)
		return
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		http.Error(w, "No se reconoce la imagen; usa PNG o JPG", http.StatusBadRequest)
		return
	}
	if cfg.Width > 4000 || cfg.Height > 4000 {
		http.Error(w, "La imagen es demasiado grande (máximo 4000 × 4000 px)", http.StatusBadRequest)
		return
	}
	h.adminWrite(w, r, "PRINT_LOGO_UPDATED", func(tx *sql.Tx) (map[string]any, error) {
		_, err := tx.ExecContext(r.Context(), `
			INSERT INTO print_formats (key, data, updated_at) VALUES ('logo', ?, CURRENT_TIMESTAMP)
			ON CONFLICT(key) DO UPDATE SET data = excluded.data, updated_at = CURRENT_TIMESTAMP`, data)
		return map[string]any{"width": cfg.Width, "height": cfg.Height, "bytes": len(data)}, err
	})
}

// DELETE /api/admin/print-formats/logo
func (h *POSHandler) AdminDeleteLogo(w http.ResponseWriter, r *http.Request) {
	h.adminWrite(w, r, "PRINT_LOGO_DELETED", func(tx *sql.Tx) (map[string]any, error) {
		_, err := tx.ExecContext(r.Context(), `DELETE FROM print_formats WHERE key = 'logo'`)
		return map[string]any{}, err
	})
}

type previewRequest struct {
	Kind       string         `json:"kind"` // receipt | comanda
	AreaID     int64          `json:"area_id"`
	PaperWidth int            `json:"paper_width"`
	PrinterID  int64          `json:"printer_id"` // solo para imprimir la prueba
	Receipt    *ReceiptFormat `json:"receipt"`
	Comanda    *ComandaFormat `json:"comanda"`
}

// buildSample arma la muestra con el formato que se está editando (aún sin
// guardar) para la vista previa o la prueba impresa.
func (h *POSHandler) buildSample(ctx context.Context, req *previewRequest, paperWidth int) (*printer.Ticket, error) {
	settings, err := loadSettings(ctx, h.DB)
	if err != nil {
		return nil, err
	}
	switch req.Kind {
	case "receipt":
		if req.Receipt == nil {
			return nil, badRequest("Falta el formato del ticket")
		}
		if err := req.Receipt.validate(); err != nil {
			return nil, badRequest(err.Error())
		}
		return receiptDoc(sampleTicket(settings, *req.Receipt), paperWidth, *req.Receipt, loadLogo(ctx, h.DB)), nil
	case "comanda":
		if req.Comanda == nil {
			return nil, badRequest("Falta el formato de la comanda")
		}
		if err := req.Comanda.validate(); err != nil {
			return nil, badRequest(err.Error())
		}
		var area string
		h.DB.QueryRowContext(ctx, `SELECT name FROM production_areas WHERE id = ?`, req.AreaID).Scan(&area)
		head, lines := sampleComanda()
		return comandaDoc(head, lines, comandaNew, area, paperWidth, *req.Comanda), nil
	}
	return nil, badRequest("Tipo de impresión inválido")
}

// POST /api/admin/print-formats/preview
func (h *POSHandler) AdminPreviewFormat(w http.ResponseWriter, r *http.Request) {
	var req previewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "JSON inválido", http.StatusBadRequest)
		return
	}
	width := 58
	if req.PaperWidth >= 80 {
		width = 80
	}
	tk, err := h.buildSample(r.Context(), &req, width)
	var bad badRequest
	if errors.As(err, &bad) {
		http.Error(w, bad.Error(), http.StatusBadRequest)
		return
	}
	if err != nil {
		serverError(w, "Error generando la vista previa", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cols": printer.ColumnsFor(width), "lines": tk.Preview()})
}

// POST /api/admin/print-formats/test - Imprime la muestra en una impresora.
func (h *POSHandler) AdminTestFormat(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req previewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "JSON inválido", http.StatusBadRequest)
		return
	}
	p, err := loadPrinter(ctx, h.DB, req.PrinterID)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "Elige una impresora", http.StatusBadRequest)
		return
	}
	if err != nil {
		serverError(w, "Error consultando impresora", err)
		return
	}
	tk, err := h.buildSample(ctx, &req, p.PaperWidth)
	var bad badRequest
	if errors.As(err, &bad) {
		http.Error(w, bad.Error(), http.StatusBadRequest)
		return
	}
	if err != nil {
		serverError(w, "Error generando la prueba", err)
		return
	}
	if _, err := printer.Send(ctx, targetOf(p), tk.Bytes()); err != nil {
		http.Error(w, "No se pudo imprimir: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"printer": p.Name})
}
