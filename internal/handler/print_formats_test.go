package handler

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReceiptFormatOptions(t *testing.T) {
	s := Settings{BusinessName: "Café Luna", TaxRate: 0.16, PricesIncludeTax: true}
	f := defaultReceiptFormat(s)
	f.ShowOrderInfo, f.ShowPayments, f.ShowTax = false, false, false
	f.HeaderLines = "Av. Juárez 12\nTel. 555 123 4567"
	f.FooterLines = "¡Gracias!\nVuelve pronto"
	f.ItemSize = "grande"

	tk := receiptDoc(sampleTicket(s, f), 58, f, nil)
	out := string(tk.Bytes())
	for _, want := range []string{"Tel. 555 123 4567", "Vuelve pronto"} {
		if !strings.Contains(out, want) {
			t.Errorf("falta %q", want)
		}
	}
	for _, unwanted := range []string{"Orden #", "Efectivo", "IVA incluido"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("no debía imprimirse %q", unwanted)
		}
	}
	// Productos a tamaño grande (2x2): caben 16 columnas en 58 mm.
	var item PreviewLineCheck
	for _, l := range tk.Preview() {
		if strings.HasPrefix(l.Text, "2 Latte") {
			item = PreviewLineCheck{l.W, l.H, len([]rune(l.Text))}
		}
	}
	if item != (PreviewLineCheck{2, 2, 16}) {
		t.Errorf("renglón del producto: %+v", item)
	}
}

type PreviewLineCheck struct{ W, H, Len int }

func TestComandaFormatCopies(t *testing.T) {
	f := defaultComandaFormat()
	f.Copies, f.ItemSize, f.ShowOrderInfo = 2, "enorme", false
	head, lines := sampleComanda()
	out := comandaDoc(head, lines, comandaNew, "Cocina", 80, f).Bytes()
	if n := bytes.Count(out, []byte{0x1d, 'V', 1}); n != 2 {
		t.Fatalf("2 copias = 2 cortes, hubo %d", n)
	}
	if !bytes.Contains(out, []byte("Copia 2 de 2")) || bytes.Contains(out, []byte("Orden #")) || !bytes.Contains(out, []byte("COCINA")) {
		t.Fatal("contenido de la comanda")
	}
	// ESC/POS "GS ! 0x22" = triple ancho y alto para los productos.
	if !bytes.Contains(out, []byte{0x1d, '!', 0x22}) {
		t.Fatal("los productos deben ir en tamaño enorme")
	}
}

func TestPrintFormatsAPI(t *testing.T) {
	db, h := setupCashDB(t)
	cookie := adminLogin(t, h, "0001")

	// Guardar y leer el formato del ticket.
	f := defaultReceiptFormat(Settings{})
	f.HeaderLines, f.NameSize = "RFC XAXX010101000", "enorme"
	body, _ := json.Marshal(f)
	if rec := call(h.RequireAdmin(h.AdminSaveReceiptFormat), "PUT", "/", string(body), cookie, 0); rec.Code != http.StatusOK {
		t.Fatalf("guardar ticket: %d %s", rec.Code, rec.Body)
	}
	got, _ := loadReceiptFormat(t.Context(), db)
	if got.HeaderLines != "RFC XAXX010101000" || got.NameSize != "enorme" {
		t.Fatalf("formato guardado: %+v", got)
	}
	f.NameSize = "gigante"
	body, _ = json.Marshal(f)
	if rec := call(h.RequireAdmin(h.AdminSaveReceiptFormat), "PUT", "/", string(body), cookie, 0); rec.Code != http.StatusBadRequest {
		t.Fatalf("un tamaño inválido debe rechazarse, dio %d", rec.Code)
	}

	// Formato por área de comanda.
	if rec := call(h.RequireAdmin(h.AdminSaveComandaFormat), "PUT", "/", `{"title_size":"normal","location_size":"grande","item_size":"grande","modifier_size":"alta","note_size":"alta","order_note_size":"normal","copies":2,"feed_lines":4,"show_area_name":true,"show_order_info":true}`, cookie, 1); rec.Code != http.StatusOK {
		t.Fatalf("guardar comanda: %d %s", rec.Code, rec.Body)
	}
	if cf, _ := loadComandaFormat(t.Context(), db, 1); cf.Copies != 2 || cf.ItemSize != "grande" {
		t.Fatalf("formato de comanda: %+v", cf)
	}
	if cf, _ := loadComandaFormat(t.Context(), db, 2); cf != defaultComandaFormat() {
		t.Fatal("otra área conserva el formato por omisión")
	}

	// Logo: se sube, aparece en la vista previa como imagen y se borra.
	img := image.NewRGBA(image.Rect(0, 0, 40, 20))
	for x := 0; x < 20; x++ {
		for y := 0; y < 20; y++ {
			img.Set(x, y, color.Black)
		}
	}
	var buf, pngBytes bytes.Buffer
	png.Encode(&pngBytes, img)
	mw := multipart.NewWriter(&buf)
	part, _ := mw.CreateFormFile("logo", "logo.png")
	part.Write(pngBytes.Bytes())
	mw.Close()
	req := httptest.NewRequest("POST", "/", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.RequireAdmin(h.AdminUploadLogo)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("subir logo: %d %s", rec.Code, rec.Body)
	}

	body, _ = json.Marshal(map[string]any{"kind": "receipt", "paper_width": 80, "receipt": got})
	rec = call(h.RequireAdmin(h.AdminPreviewFormat), "POST", "/", string(body), cookie, 0)
	var prev struct {
		Cols  int
		Lines []struct{ Kind, Text string }
	}
	json.Unmarshal(rec.Body.Bytes(), &prev)
	if rec.Code != http.StatusOK || prev.Cols != 48 || len(prev.Lines) == 0 || prev.Lines[0].Kind != "image" {
		t.Fatalf("vista previa: %d %+v", rec.Code, prev)
	}
	call(h.RequireAdmin(h.AdminDeleteLogo), "DELETE", "/", "", cookie, 0)
	if loadLogo(t.Context(), db) != nil {
		t.Fatal("el logo debía borrarse")
	}
}
