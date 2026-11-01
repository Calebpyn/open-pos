package handler

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/calebpyn/open-pos/internal/ocr"
)

// --- Tickets de compra ---
//
// Quien surte toma foto del ticket; el OCR de macOS lo lee (sin internet) y
// se sugieren tienda, fecha, total y a qué insumo corresponde cada renglón.
// La persona revisa y confirma: lo que estaba en la lista se tacha como
// comprado, los precios se actualizan y la relación renglón→insumo se
// aprende para el siguiente ticket.

const comprasTicketMax = 15 << 20 // fotos de teléfono

var comprasImageExts = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".heic": true, ".heif": true, ".webp": true}

func (h *POSHandler) ticketsDir() string {
	dir := h.DataDir
	if dir == "" {
		dir = "."
	}
	return filepath.Join(dir, "compras-tickets")
}

// recognize corre el OCR (en pruebas se reemplaza con h.OCR).
func (h *POSHandler) recognize(ctx context.Context, path string) ([]ocr.Box, error) {
	if h.OCR != nil {
		return h.OCR(ctx, path)
	}
	return ocr.Recognize(ctx, path)
}

type comprasReceipt struct {
	ID          int64
	Status      string
	PlaceID     int64
	Place       string
	PurchasedOn string
	TotalCents  *int64
	Image       string
	OCRText     string
	OCRError    string
	UploadedBy  string
	CreatedAt   time.Time
	Lines       int
	LinesCents  int64
}

func (r *comprasReceipt) DateLabel() string {
	if t, err := time.Parse("2006-01-02", r.PurchasedOn); err == nil {
		return t.Format("02/01/2006")
	}
	return r.CreatedAt.Local().Format("02/01/2006")
}

// --- Relacionar renglones con insumos ---

type comprasMatcher struct {
	aliases  map[string]int64
	supplies []matchSupply
	places   []matchPlace
}

type matchSupply struct {
	id    int64
	words []string
	brand []string
}

type matchPlace struct {
	id  int64
	key string
}

// receiptKey identifica un renglón de ticket para aprenderlo.
func receiptKey(desc string) string { return strings.Join(ocr.Words(desc), " ") }

// compact quita todo lo que no es letra o número: "SAM'S CLUB" → "samsclub".
func compact(s string) string { return strings.Join(ocr.Words(strings.ReplaceAll(s, "'", "")), "") }

// usefulWords son las palabras que sirven para comparar (sin números ni
// palabras de una o dos letras).
func usefulWords(s string) []string {
	var out []string
	for _, w := range ocr.Words(s) {
		if len([]rune(w)) < 3 {
			continue
		}
		if _, err := strconv.Atoi(w); err == nil {
			continue
		}
		out = append(out, w)
	}
	return out
}

func loadMatcher(ctx context.Context, q queryer) (*comprasMatcher, error) {
	m := &comprasMatcher{aliases: map[string]int64{}}
	if err := eachRow(ctx, q, `SELECT text_key, supply_id FROM compras_receipt_aliases`, nil, func(scan func(...any) error) error {
		var k string
		var id int64
		if err := scan(&k, &id); err != nil {
			return err
		}
		m.aliases[k] = id
		return nil
	}); err != nil {
		return nil, err
	}
	if err := eachRow(ctx, q, `
		SELECT s.id, s.name, COALESCE((SELECT GROUP_CONCAT(brand, ' ') FROM compras_sources x WHERE x.supply_id = s.id), '')
		FROM compras_supplies s WHERE s.is_active = 1`, nil, func(scan func(...any) error) error {
		var id int64
		var name, brands string
		if err := scan(&id, &name, &brands); err != nil {
			return err
		}
		m.supplies = append(m.supplies, matchSupply{id: id, words: usefulWords(name), brand: usefulWords(brands)})
		return nil
	}); err != nil {
		return nil, err
	}
	err := eachRow(ctx, q, `SELECT id, name FROM compras_places WHERE is_active = 1`, nil, func(scan func(...any) error) error {
		var p matchPlace
		var name string
		if err := scan(&p.id, &name); err != nil {
			return err
		}
		if p.key = compact(name); p.key != "" {
			m.places = append(m.places, p)
		}
		return nil
	})
	return m, err
}

// sameWord: una palabra del ticket y otra del catálogo coinciden si una
// empieza con la otra ("ENT" ↔ "entera", "FRESA" ↔ "fresas").
func sameWord(a, b string) bool {
	if len([]rune(a)) < 3 || len([]rune(b)) < 3 {
		return a == b
	}
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

// supply sugiere el insumo de un renglón: primero lo aprendido; si no, el que
// tenga más palabras en común (al menos la mitad de su nombre).
func (m *comprasMatcher) supply(desc string) int64 {
	if id, ok := m.aliases[receiptKey(desc)]; ok {
		return id
	}
	words := usefulWords(desc)
	var best int64
	bestScore := 0.0
	for _, s := range m.supplies {
		if len(s.words) == 0 {
			continue
		}
		hit := 0
		for _, sw := range s.words {
			for _, w := range words {
				if sameWord(w, sw) {
					hit++
					break
				}
			}
		}
		if hit == 0 {
			continue
		}
		score := float64(hit) / float64(len(s.words))
		for _, b := range s.brand {
			for _, w := range words {
				if sameWord(w, b) {
					score += 0.1
				}
			}
		}
		if score >= 0.5 && score > bestScore {
			best, bestScore = s.id, score
		}
	}
	return best
}

// place busca la tienda en el encabezado del ticket.
func (m *comprasMatcher) place(header []string) int64 {
	text := compact(strings.Join(header, " "))
	var best int64
	bestLen := 0
	for _, p := range m.places {
		if strings.Contains(text, p.key) && len(p.key) > bestLen {
			best, bestLen = p.id, len(p.key)
		}
	}
	return best
}

// BackupTicketImages copia a <backupDir>/compras-tickets/ las fotos de
// tickets que aún no estén ahí (no van dentro de pos.db). Solo agrega: una
// foto ya copiada no se vuelve a copiar.
func BackupTicketImages(dataDir, backupDir string) {
	src := filepath.Join(orDefault(dataDir, "."), "compras-tickets")
	entries, err := os.ReadDir(src)
	if err != nil {
		return // aún no hay tickets
	}
	dst := filepath.Join(backupDir, "compras-tickets")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		log.Printf("Respaldo de tickets: %v", err)
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		to := filepath.Join(dst, e.Name())
		if _, err := os.Stat(to); err == nil {
			continue
		}
		if err := copyFile(filepath.Join(src, e.Name()), to); err != nil {
			log.Printf("Respaldo de tickets: %v", err)
		}
	}
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(to + ".tmp")
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(to + ".tmp")
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(to+".tmp", to)
}

// --- Subir y leer ---

// POST /compras/tickets - Sube la foto, la lee y deja el ticket por revisar.
func (h *POSHandler) ComprasUploadTicket(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := comprasFrom(ctx)
	r.Body = http.MaxBytesReader(w, r.Body, comprasTicketMax)
	file, header, err := r.FormFile("photo")
	if err != nil {
		http.Redirect(w, r, "/compras/tickets?error="+url.QueryEscape("Elige o toma la foto del ticket (máximo 15 MB)."), http.StatusSeeOther)
		return
	}
	defer file.Close()
	ext := strings.ToLower(filepath.Ext(header.Filename))
	if !comprasImageExts[ext] {
		// Algunas cámaras no mandan nombre: se adivina por el contenido.
		head := make([]byte, 512)
		n, _ := io.ReadFull(file, head)
		switch http.DetectContentType(head[:n]) {
		case "image/jpeg":
			ext = ".jpg"
		case "image/png":
			ext = ".png"
		case "image/webp":
			ext = ".webp"
		default:
			http.Redirect(w, r, "/compras/tickets?error="+url.QueryEscape("El archivo no parece una foto (JPG, PNG o HEIC)."), http.StatusSeeOther)
			return
		}
		file.Seek(0, io.SeekStart)
	}
	dir := h.ticketsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		serverError(w, "No se pudo crear la carpeta de tickets", err)
		return
	}
	raw := make([]byte, 6)
	rand.Read(raw)
	name := time.Now().Format("20060102-150405") + "-" + hex.EncodeToString(raw) + ext
	path := filepath.Join(dir, name)
	out, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		serverError(w, "No se pudo guardar la foto", err)
		return
	}
	if _, err := io.Copy(out, file); err != nil {
		out.Close()
		os.Remove(path)
		serverError(w, "No se pudo guardar la foto", err)
		return
	}
	out.Close()

	// Leer el ticket (un par de segundos). Si no se puede, se captura a mano.
	var parsed ocr.Receipt
	var rows []string
	var ocrErr string
	boxes, err := h.recognize(ctx, path)
	switch {
	case errors.Is(err, ocr.ErrUnavailable):
		ocrErr = "Este equipo no puede leer tickets automáticamente; captura los productos a mano."
	case err != nil:
		log.Printf("OCR de ticket %s: %v", name, err)
		ocrErr = "No se pudo leer el ticket; captura los productos a mano."
	default:
		rows = ocr.Rows(boxes)
		parsed = ocr.Parse(rows)
		if len(parsed.Lines) == 0 {
			ocrErr = "No se encontraron productos con precio; revisa la foto o captúralos a mano."
		}
	}

	matcher, err := loadMatcher(ctx, h.DB)
	if err != nil {
		serverError(w, "Error leyendo el catálogo", err)
		return
	}
	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		serverError(w, "Error al iniciar transacción", err)
		return
	}
	defer tx.Rollback()
	var place, total any
	if id := matcher.place(parsed.Header); id > 0 {
		place = id
	}
	if parsed.TotalCents > 0 {
		total = parsed.TotalCents
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO compras_receipts (place_id, purchased_on, total_cents, image, ocr_text, ocr_error, uploaded_by)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, place, parsed.Date, total, name, strings.Join(rows, "\n"), ocrErr, u.userArg())
	if err != nil {
		os.Remove(path)
		serverError(w, "Error guardando el ticket", err)
		return
	}
	id, _ := res.LastInsertId()
	for i, l := range parsed.Lines {
		var supply any
		if s := matcher.supply(l.Text); s > 0 {
			supply = s
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO compras_receipt_lines (receipt_id, description, quantity, by_weight, amount_cents, supply_id, sort_order)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, id, l.Text, l.Qty, l.ByWeight, l.AmountCents, supply, i); err != nil {
			os.Remove(path)
			serverError(w, "Error guardando el ticket", err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		os.Remove(path)
		serverError(w, "Error guardando el ticket", err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/compras/tickets/%d", id), http.StatusSeeOther)
}

// --- Pantallas ---

func comprasLoadReceipt(ctx context.Context, q queryer, id int64) (*comprasReceipt, error) {
	rc := &comprasReceipt{}
	var place sql.NullInt64
	err := q.QueryRowContext(ctx, `
		SELECT r.id, r.status, r.place_id, COALESCE(p.name, ''), r.purchased_on, r.total_cents, r.image, r.ocr_text,
		       r.ocr_error, COALESCE(u.name, 'Caja'), r.created_at
		FROM compras_receipts r LEFT JOIN compras_places p ON p.id = r.place_id LEFT JOIN users u ON u.id = r.uploaded_by
		WHERE r.id = ?`, id).Scan(&rc.ID, &rc.Status, &place, &rc.Place, &rc.PurchasedOn, &rc.TotalCents, &rc.Image,
		&rc.OCRText, &rc.OCRError, &rc.UploadedBy, &rc.CreatedAt)
	rc.PlaceID = place.Int64
	return rc, err
}

// GET /compras/tickets - Subir un ticket y ver los registrados.
func (h *POSHandler) ComprasTicketsPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var receipts []*comprasReceipt
	err := eachRow(ctx, h.DB, `
		SELECT r.id, r.status, COALESCE(p.name, ''), r.purchased_on, r.total_cents, r.created_at, COALESCE(u.name, 'Caja'),
		       (SELECT COUNT(*) FROM compras_receipt_lines l WHERE l.receipt_id = r.id),
		       (SELECT COALESCE(SUM(amount_cents), 0) FROM compras_receipt_lines l WHERE l.receipt_id = r.id)
		FROM compras_receipts r LEFT JOIN compras_places p ON p.id = r.place_id LEFT JOIN users u ON u.id = r.uploaded_by
		ORDER BY r.status = 'CONFIRMED', COALESCE(NULLIF(r.purchased_on, ''), date(r.created_at)) DESC, r.id DESC
		LIMIT 60`, nil, func(scan func(...any) error) error {
		rc := &comprasReceipt{}
		if err := scan(&rc.ID, &rc.Status, &rc.Place, &rc.PurchasedOn, &rc.TotalCents, &rc.CreatedAt, &rc.UploadedBy,
			&rc.Lines, &rc.LinesCents); err != nil {
			return err
		}
		receipts = append(receipts, rc)
		return nil
	})
	if err != nil {
		serverError(w, "Error cargando tickets", err)
		return
	}
	// Lo registrado en el mes, en total y por lugar.
	type byPlace struct {
		Place string
		Cents int64
		Count int
	}
	var month []byPlace
	var monthTotal int64
	monthStart := time.Now().Format("2006-01") + "-01"
	err = eachRow(ctx, h.DB, `
		SELECT COALESCE(p.name, 'Sin lugar'), SUM(COALESCE(r.total_cents, 0)), COUNT(*)
		FROM compras_receipts r LEFT JOIN compras_places p ON p.id = r.place_id
		WHERE r.status = 'CONFIRMED' AND COALESCE(NULLIF(r.purchased_on, ''), date(r.created_at, 'localtime')) >= ?
		GROUP BY 1 ORDER BY 2 DESC`, []any{monthStart}, func(scan func(...any) error) error {
		var b byPlace
		if err := scan(&b.Place, &b.Cents, &b.Count); err != nil {
			return err
		}
		monthTotal += b.Cents
		month = append(month, b)
		return nil
	})
	if err != nil {
		serverError(w, "Error cargando tickets", err)
		return
	}
	comprasRender(w, r, "tickets.html", "tickets", map[string]any{
		"Receipts": receipts, "Month": month, "MonthTotal": monthTotal, "MonthName": comprasMonthName(time.Now()),
		"Error": r.URL.Query().Get("error"), "OCR": ocr.Available() || h.OCR != nil,
	})
}

func comprasMonthName(t time.Time) string {
	return [...]string{"enero", "febrero", "marzo", "abril", "mayo", "junio", "julio", "agosto", "septiembre",
		"octubre", "noviembre", "diciembre"}[t.Month()-1]
}

type ticketLineJSON struct {
	Item     bool   `json:"item"` // se tachó en la lista al registrar
	Desc     string `json:"desc"`
	Qty      string `json:"qty"`
	ByWeight bool   `json:"byWeight"`
	Amount   string `json:"amount"`
	Supply   string `json:"supply"`
}

type ticketSupplyJSON struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	InList bool   `json:"inList"` // pendiente en la lista activa
}

// GET /compras/tickets/{id} - Revisar lo leído y registrar la compra.
func (h *POSHandler) ComprasTicketPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	rc, err := comprasLoadReceipt(ctx, h.DB, id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		serverError(w, "Error cargando el ticket", err)
		return
	}
	lines := []ticketLineJSON{}
	err = eachRow(ctx, h.DB, `
		SELECT description, quantity, by_weight, amount_cents, COALESCE(supply_id, 0), item_id IS NOT NULL
		FROM compras_receipt_lines WHERE receipt_id = ? ORDER BY sort_order, id`, []any{id}, func(scan func(...any) error) error {
		var l ticketLineJSON
		var qty float64
		var amount, supply int64
		if err := scan(&l.Desc, &qty, &l.ByWeight, &amount, &supply, &l.Item); err != nil {
			return err
		}
		l.Qty, l.Amount = formatQty(qty), strings.TrimPrefix(formatCents(amount), "$")
		if supply > 0 {
			l.Supply = strconv.FormatInt(supply, 10)
		}
		lines = append(lines, l)
		return nil
	})
	if err != nil {
		serverError(w, "Error cargando el ticket", err)
		return
	}
	supplies := []ticketSupplyJSON{}
	err = eachRow(ctx, h.DB, `
		SELECT s.id, s.name, EXISTS (SELECT 1 FROM compras_items i JOIN compras_lists l ON l.id = i.list_id
		                             WHERE i.supply_id = s.id AND l.status <> 'CLOSED' AND i.status <> 'BOUGHT')
		FROM compras_supplies s WHERE s.is_active = 1 ORDER BY s.name_key`, nil, func(scan func(...any) error) error {
		var s ticketSupplyJSON
		if err := scan(&s.ID, &s.Name, &s.InList); err != nil {
			return err
		}
		supplies = append(supplies, s)
		return nil
	})
	if err != nil {
		serverError(w, "Error cargando insumos", err)
		return
	}
	places, err := comprasLoadPlaces(ctx, h.DB, true)
	if err != nil {
		serverError(w, "Error cargando lugares", err)
		return
	}
	total := ""
	if rc.TotalCents != nil {
		total = strings.TrimPrefix(formatCents(*rc.TotalCents), "$")
	}
	comprasRender(w, r, "ticket.html", "tickets", map[string]any{
		"Receipt": rc, "Lines": lines, "Supplies": supplies, "Places": places, "Total": total,
		"Error": r.URL.Query().Get("error"),
	})
}

// GET /compras/tickets/{id}/imagen
func (h *POSHandler) ComprasTicketImage(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	var name string
	if err := h.DB.QueryRowContext(r.Context(), `SELECT image FROM compras_receipts WHERE id = ?`, id).Scan(&name); err != nil || name == "" ||
		filepath.Base(name) != name {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.ServeFile(w, r, filepath.Join(h.ticketsDir(), name))
}

// POST /compras/tickets/{id}/{confirmar|descartar}
func (h *POSHandler) ComprasTicketAction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	back := func(msg string) {
		http.Redirect(w, r, fmt.Sprintf("/compras/tickets/%d?error=%s", id, url.QueryEscape(msg)), http.StatusSeeOther)
	}
	switch r.PathValue("action") {
	case "descartar":
		var image string
		err := h.DB.QueryRowContext(ctx, `DELETE FROM compras_receipts WHERE id = ? AND status = 'DRAFT' RETURNING image`, id).Scan(&image)
		if errors.Is(err, sql.ErrNoRows) {
			back("Solo se descartan tickets sin registrar.")
			return
		}
		if err != nil {
			serverError(w, "Error descartando el ticket", err)
			return
		}
		if image != "" && filepath.Base(image) == image {
			os.Remove(filepath.Join(h.ticketsDir(), image))
		}
		http.Redirect(w, r, "/compras/tickets", http.StatusSeeOther)
	case "confirmar":
		err := h.comprasConfirmTicket(r, id)
		var bad badRequest
		if errors.As(err, &bad) {
			back(bad.Error())
			return
		}
		if err != nil {
			serverError(w, "Error registrando la compra", err)
			return
		}
		http.Redirect(w, r, fmt.Sprintf("/compras/tickets/%d", id), http.StatusSeeOther)
	default:
		http.NotFound(w, r)
	}
}

type ticketLine struct {
	desc     string
	qty      float64
	byWeight bool
	amount   int64
	supply   int64
}

// comprasConfirmTicket registra la compra del ticket: tacha lo que estaba en
// la lista, actualiza precios y aprende cómo se llama cada insumo en el
// ticket.
func (h *POSHandler) comprasConfirmTicket(r *http.Request, id int64) error {
	ctx := r.Context()
	u := comprasFrom(ctx)
	if err := r.ParseForm(); err != nil {
		return badRequest("Formulario inválido.")
	}
	field := func(name string, i int) string {
		if v := r.Form[name]; i < len(v) {
			return strings.TrimSpace(v[i])
		}
		return ""
	}
	var lines []ticketLine
	for i := range r.Form["desc"] {
		l := ticketLine{desc: field("desc", i), byWeight: field("by_weight", i) == "1"}
		if l.desc == "" {
			continue
		}
		var err error
		if l.qty, err = parseQty(orDefault(field("qty", i), "1")); err != nil {
			return badRequest(fmt.Sprintf("%s: %v", l.desc, err))
		}
		cents, ok, err := parseCents(field("amount", i))
		if err != nil {
			return err
		}
		if !ok || cents == 0 {
			return badRequest(fmt.Sprintf("Falta el importe de %q (si no costó nada, quita el renglón).", l.desc))
		}
		l.amount = cents
		l.supply, _ = strconv.ParseInt(field("supply", i), 10, 64)
		lines = append(lines, l)
	}
	if len(lines) == 0 {
		return badRequest("Agrega al menos un producto del ticket.")
	}
	placeID, _ := strconv.ParseInt(r.FormValue("place"), 10, 64)
	date := r.FormValue("date")
	if date != "" {
		if _, err := time.Parse("2006-01-02", date); err != nil {
			return badRequest("Fecha inválida.")
		}
	}
	var total any
	if cents, ok, err := parseCents(r.FormValue("total")); err != nil {
		return err
	} else if ok {
		total = cents
	} else {
		var sum int64
		for _, l := range lines {
			sum += l.amount
		}
		total = sum
	}

	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM compras_receipts WHERE id = ?`, id).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return badRequest("El ticket ya no existe.")
		}
		return err
	}
	if status != "DRAFT" {
		return badRequest("Este ticket ya se registró.")
	}
	var place any
	if placeID > 0 {
		var ok bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM compras_places WHERE id = ?)`, placeID).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return badRequest("Lugar inválido.")
		}
		place = placeID
	}
	active, err := comprasActive(ctx, tx)
	if err != nil {
		return err
	}
	var listID any
	if active != nil {
		listID = active.ID
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM compras_receipt_lines WHERE receipt_id = ?`, id); err != nil {
		return err
	}
	bought := 0
	for i, l := range lines {
		var supply, item any
		if l.supply > 0 {
			var ok bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM compras_supplies WHERE id = ?)`, l.supply).Scan(&ok); err != nil {
				return err
			}
			if !ok {
				return badRequest(fmt.Sprintf("El insumo de %q ya no existe.", l.desc))
			}
			supply = l.supply
			itemID, err := comprasTicketItem(ctx, tx, u, active, l, placeID)
			if err != nil {
				return err
			}
			if itemID > 0 {
				item = itemID
				bought++
			} else if err := comprasRecordPrice(ctx, tx, l.supply, placeID, l.qty, l.amount); err != nil {
				return err
			}
			if key := receiptKey(l.desc); key != "" {
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO compras_receipt_aliases (text_key, supply_id) VALUES (?, ?)
					ON CONFLICT (text_key) DO UPDATE SET supply_id = excluded.supply_id, updated_at = CURRENT_TIMESTAMP`,
					key, l.supply); err != nil {
					return err
				}
			}
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO compras_receipt_lines (receipt_id, description, quantity, by_weight, amount_cents, supply_id, item_id, sort_order)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, id, l.desc, l.qty, l.byWeight, l.amount, supply, item, i); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE compras_receipts SET status = 'CONFIRMED', place_id = ?, purchased_on = ?, total_cents = ?, list_id = ?,
		       confirmed_by = ?, confirmed_at = CURRENT_TIMESTAMP
		WHERE id = ?`, place, date, total, listID, u.userArg(), id); err != nil {
		return err
	}
	if err := audit(ctx, tx, "COMPRAS_TICKET_CONFIRMED", map[string]any{
		"receipt_id": id, "lines": len(lines), "bought_from_list": bought, "total": total, "user": u.Name,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// comprasTicketItem tacha en la lista activa el renglón de ese insumo (si lo
// hay y no se había comprado) y regresa su id; 0 si no estaba en la lista.
func comprasTicketItem(ctx context.Context, tx *sql.Tx, u comprasUser, active *comprasList, l ticketLine, placeID int64) (int64, error) {
	if active == nil {
		return 0, nil
	}
	var itemID int64
	var status string
	err := tx.QueryRowContext(ctx, `SELECT id, status FROM compras_items WHERE list_id = ? AND supply_id = ?`,
		active.ID, l.supply).Scan(&itemID, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if status == "BOUGHT" {
		// Ya se había tachado a mano: se completa el precio si faltaba.
		if _, err := tx.ExecContext(ctx, `
			UPDATE compras_items SET bought_price_cents = ?, bought_quantity = ?, bought_place_id = COALESCE(bought_place_id, ?)
			WHERE id = ? AND bought_price_cents IS NULL`, l.amount, l.qty, nullID(placeID), itemID); err != nil {
			return 0, err
		}
		return itemID, comprasRecordPrice(ctx, tx, l.supply, placeID, l.qty, l.amount)
	}
	amount := l.amount
	return itemID, comprasBuyTx(ctx, tx, u, itemID, l.qty, &amount, placeID)
}

func nullID(id int64) any {
	if id <= 0 {
		return nil
	}
	return id
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
