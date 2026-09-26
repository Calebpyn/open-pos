package handler

import (
	"html/template"
	"net/http"
	"path"
	"strings"
	"sync"

	"github.com/calebpyn/open-pos/web"
)

// Las páginas se interpretan una sola vez y se reutilizan. En modo desarrollo
// (POS_WEB_DIR) se vuelven a leer en cada petición para ver los cambios sin
// reiniciar.
var (
	pageCacheMu sync.Mutex
	pageCache   = map[string]*template.Template{}
)

// page carga una plantilla de web/templates (más las que incluye, p. ej. la
// barra del panel) con las funciones de las páginas.
func page(files ...string) (*template.Template, error) {
	key := strings.Join(files, "|")
	if !web.Dev() {
		pageCacheMu.Lock()
		defer pageCacheMu.Unlock()
		if t, ok := pageCache[key]; ok {
			return t, nil
		}
	}
	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = "templates/" + f
	}
	t, err := template.New(path.Base(files[0])).Funcs(pageFuncs).Funcs(adminFuncs).Funcs(devFuncs).ParseFS(web.FS(), paths...)
	if err != nil {
		return nil, err
	}
	if !web.Dev() {
		pageCache[key] = t
	}
	return t, nil
}

// devFuncs: en modo desarrollo se carga además el Tailwind que genera estilos
// en el navegador, para probar clases que aún no están en app.css.
var devFuncs = template.FuncMap{
	"devTailwind": func() template.HTML {
		if web.Dev() {
			return `<script src="/static/vendor/tailwindcss-play.js"></script>`
		}
		return ""
	},
}

// renderPage ejecuta una plantilla y responde HTML (o 500 si falla).
func renderPage(w http.ResponseWriter, data any, files ...string) {
	t, err := page(files...)
	if err != nil {
		serverError(w, "Error cargando plantilla", err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.Execute(w, data); err != nil {
		serverError(w, "Error renderizando plantilla", err)
	}
}
