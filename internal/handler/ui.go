package handler

import (
	"encoding/json"
	"html/template"
	"net/http"
)

type UIHandler struct {
	POS *POSHandler
}

func NewUIHandler(pos *POSHandler) *UIHandler {
	return &UIHandler{POS: pos}
}

// GET / - Servir la página principal
// En una terminal de mesero se muestra la vista simplificada (sin cobro, caja
// ni admin) y se pide PIN.
func (h *UIHandler) ServeIndex(w http.ResponseWriter, r *http.Request) {
	t := terminalFrom(r.Context())
	renderPage(w, map[string]any{"Terminal": t, "Waiter": t.Kind == TerminalWaiter}, "index.html")
}

// esc escapa texto para insertarlo en HTML.
func esc(s string) string { return template.HTMLEscapeString(s) }

// jsAttr convierte s en un literal de JavaScript seguro dentro de un atributo
// HTML (p. ej. @click de Alpine): "Café d'Olla" no rompe la expresión.
func jsAttr(s string) string {
	b, _ := json.Marshal(s)
	return template.HTMLEscapeString(string(b))
}
