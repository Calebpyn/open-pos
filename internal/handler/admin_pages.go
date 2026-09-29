package handler

import (
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
}

var adminFuncs = template.FuncMap{
	"adminSections": func() []adminSection { return adminSectionList },
}

// GET /admin/{section} - Páginas del panel (protegidas con RequireAdmin).
func (h *UIHandler) ServeAdminPage(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("section")
	for _, s := range adminSectionList {
		if s.Key != key {
			continue
		}
		u, _ := currentUser(r.Context())
		renderPage(w, map[string]any{
			"Section":    s.Key,
			"User":       u,
			"DefaultPIN": defaultPINActive(r.Context(), h.POS.DB),
			"Remote":     terminalFrom(r.Context()).Kind == TerminalRemote,
		}, s.File, "admin/_nav.html")
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
