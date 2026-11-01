package handler

import (
	"context"
	"html/template"
	"net/http"
	"strings"
)

type adminSection struct {
	Key   string
	Label string
	File  string
}

var adminSectionList = []adminSection{
	{"reportes", "📊 Reportes", "admin/reportes.html"},
	{"historial", "📜 Historial", "admin/historial.html"},
	{"nomina", "🧑‍🍳 Nómina", "admin/nomina.html"},
	{"menu", "🍽️ Menú", "admin/menu.html"},
	{"mesas", "🪑 Mesas", "admin/mesas.html"},
	{"usuarios", "👥 Usuarios", "admin/usuarios.html"},
	{"dispositivos", "🖨️ Dispositivos", "devices.html"},
	{"impresiones", "🧾 Impresiones", "admin/impresiones.html"},
	{"configuracion", "🏪 Configuración", "admin/configuracion.html"},
	{"plugins", "🧩 Plugins", "admin/plugins.html"},
}

var adminFuncs = template.FuncMap{}

// navSections son las pestañas del panel: las propias y, antes de
// Configuración, las de los plugins instalados.
func (h *POSHandler) navSections(ctx context.Context) []adminSection {
	out := make([]adminSection, 0, len(adminSectionList)+4)
	for _, s := range adminSectionList {
		if s.Key == "configuracion" {
			for _, p := range h.Plugins.List() {
				if p.Admin != nil {
					out = append(out, adminSection{Key: "plugins/" + p.ID, Label: p.Admin.Label})
				}
			}
		}
		out = append(out, s)
	}
	return out
}

// adminPageData son los datos que usa la barra del panel.
func (h *POSHandler) adminPageData(r *http.Request, section string) map[string]any {
	u, _ := currentUser(r.Context())
	return map[string]any{
		"Section":    section,
		"Sections":   h.navSections(r.Context()),
		"User":       u,
		"DefaultPIN": defaultPINActive(r.Context(), h.DB),
		"Remote":     terminalFrom(r.Context()).Kind == TerminalRemote,
	}
}

// GET /admin/{section} - Páginas del panel (protegidas con RequireAdmin).
func (h *UIHandler) ServeAdminPage(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("section")
	for _, s := range adminSectionList {
		if s.Key != key {
			continue
		}
		renderPage(w, h.POS.adminPageData(r, s.Key), s.File, "admin/_nav.html")
		return
	}
	http.NotFound(w, r)
}

// GET /admin - Entra a reportes, la sección de uso diario.
func (h *UIHandler) ServeAdminHome(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/admin/reportes", http.StatusSeeOther)
}

// GET /admin/login
func (h *UIHandler) ServeAdminLogin(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.POS.adminSessionUser(r); ok {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	renderPage(w, map[string]any{"Remote": terminalFrom(r.Context()).Kind == TerminalRemote}, "admin/login.html")
}

// safeNext evita redirigir fuera del panel después del inicio de sesión.
func safeNext(next string) string {
	if strings.HasPrefix(next, "/admin") && !strings.HasPrefix(next, "//") {
		return next
	}
	return "/admin"
}
