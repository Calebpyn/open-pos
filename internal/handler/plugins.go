package handler

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"time"

	"github.com/calebpyn/open-pos/internal/plugins"
)

// --- API para plugins (/api/plugin/v1) ---
//
// Solo responde en esta computadora y con el token que el POS le dio al
// plugin al arrancarlo. Cada ruta exige el permiso que el plugin declaró en su
// plugin.json.

const ctxPlugin ctxKey = 200

func pluginFrom(ctx context.Context) *plugins.Plugin {
	p, _ := ctx.Value(ctxPlugin).(*plugins.Plugin)
	return p
}

// pluginAPI protege una ruta de la API de plugins.
func (h *POSHandler) pluginAPI(perm string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isLoopback(r) {
			http.Error(w, "La API de plugins solo responde en el servidor", http.StatusForbidden)
			return
		}
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		p, ok := h.Plugins.ByToken(token)
		if !ok || !p.Ready() {
			http.Error(w, "Token de plugin inválido", http.StatusUnauthorized)
			return
		}
		if perm != "" && !p.Can(perm) {
			http.Error(w, "El plugin no pidió el permiso "+perm+" en su plugin.json", http.StatusForbidden)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxPlugin, p)))
	}
}

// RegisterPluginRoutes agrega la API de plugins, su administración y el
// proxy a sus páginas.
func (h *POSHandler) RegisterPluginRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/plugin/v1/info", h.pluginAPI("", h.PluginInfo))
	mux.HandleFunc("GET /api/plugin/v1/events", h.pluginAPI(plugins.PermEvents, h.PluginEvents))
	mux.HandleFunc("GET /api/plugin/v1/events/latest", h.pluginAPI(plugins.PermEvents, h.PluginLatestEvent))
	mux.HandleFunc("GET /api/plugin/v1/catalog", h.pluginAPI(plugins.PermCatalog, h.AdminGetMenu))
	mux.HandleFunc("POST /api/plugin/v1/products/{id}/availability", h.pluginAPI(plugins.PermAvailability, h.PluginSetAvailability))

	admin := h.RequireAdmin
	mux.HandleFunc("GET /api/admin/plugins", admin(h.AdminListPlugins))
	mux.HandleFunc("POST /api/admin/plugins/{id}/{action}", admin(h.AdminPluginAction))
	mux.HandleFunc("GET /admin/plugins/{id}", admin(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/plugins/"+r.PathValue("id")+"/", http.StatusSeeOther)
	}))
	// Un patrón por método: uno sin método chocaría con "GET /" del POS.
	for _, m := range []string{"GET", "POST", "PUT", "PATCH", "DELETE"} {
		mux.HandleFunc(m+" /admin/plugins/{id}/{path...}", admin(h.PluginProxy))
	}
}

// GET /api/plugin/v1/info - Quién soy y qué versión de POS hay.
func (h *POSHandler) PluginInfo(w http.ResponseWriter, r *http.Request) {
	p := pluginFrom(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"plugin_id": p.ID, "permissions": p.Permissions,
		"pos_version": Version, "api_version": plugins.APIVersion,
	})
}

// GET /api/plugin/v1/events?after=N&limit=100&wait=25 - Eventos posteriores
// a N. Con wait (segundos, máx. 55) espera a que haya alguno. Pedir después
// de N confirma que el plugin ya procesó hasta N.
func (h *POSHandler) PluginEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	after, err := strconv.ParseInt(q.Get("after"), 10, 64)
	if err != nil || after < 0 {
		http.Error(w, "after debe ser el último seq procesado (0 para empezar)", http.StatusBadRequest)
		return
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	wait, _ := strconv.Atoi(q.Get("wait"))
	wait = min(max(wait, 0), 55)

	p := pluginFrom(r.Context())
	if err := plugins.Ack(r.Context(), h.DB, p.ID, after); err != nil {
		log.Printf("[plugin %s] no se guardó su avance: %v", p.ID, err)
	}
	events, err := plugins.ReadEvents(r.Context(), h.DB, after, limit, time.Duration(wait)*time.Second)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		serverError(w, "Error leyendo eventos", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

// GET /api/plugin/v1/events/latest - El último seq, para empezar "desde ahora".
func (h *POSHandler) PluginLatestEvent(w http.ResponseWriter, r *http.Request) {
	seq, err := plugins.LatestSeq(r.Context(), h.DB)
	if err != nil {
		serverError(w, "Error leyendo eventos", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"seq": seq})
}

// POST /api/plugin/v1/products/{id}/availability {"available": false, "reason": "..."}
// Un inventario marca agotado lo que ya no tiene: así se bloquea la venta sin
// que el POS le pregunte al plugin en cada orden.
func (h *POSHandler) PluginSetAvailability(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := pathID(r)
	if !ok {
		http.Error(w, "ID inválido", http.StatusBadRequest)
		return
	}
	var req struct {
		Available bool   `json:"available"`
		Reason    string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "JSON inválido", http.StatusBadRequest)
		return
	}
	p := pluginFrom(ctx)
	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		serverError(w, "Error al iniciar transacción", err)
		return
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE products SET is_available = ? WHERE id = ?`, req.Available, id)
	if err != nil {
		serverError(w, "Error guardando", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		http.Error(w, "Producto no encontrado", http.StatusNotFound)
		return
	}
	details := map[string]any{"product_id": id, "available": req.Available, "plugin": p.ID, "reason": req.Reason}
	if err := audit(ctx, tx, "PRODUCT_AVAILABILITY", details); err != nil {
		serverError(w, "Error registrando bitácora", err)
		return
	}
	if err := emitCatalogChanged(ctx, tx, "PRODUCT_AVAILABILITY", details); err != nil {
		serverError(w, "Error registrando evento", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "Error guardando", err)
		return
	}
	writeJSON(w, http.StatusOK, details)
}

// --- Administración de plugins ---

// GET /api/admin/plugins
func (h *POSHandler) AdminListPlugins(w http.ResponseWriter, r *http.Request) {
	list := []plugins.Status{}
	for _, p := range h.Plugins.List() {
		list = append(list, h.Plugins.Status(r.Context(), p))
	}
	broken := h.Plugins.Broken()
	if broken == nil {
		broken = []plugins.Broken{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"plugins": list, "broken": broken, "api_version": plugins.APIVersion})
}

// POST /api/admin/plugins/{id}/{enable|disable|restart}
func (h *POSHandler) AdminPluginAction(w http.ResponseWriter, r *http.Request) {
	id, action := r.PathValue("id"), r.PathValue("action")
	if _, ok := h.Plugins.Get(id); !ok {
		http.Error(w, "No existe ese plugin", http.StatusNotFound)
		return
	}
	var err error
	switch action {
	case "enable", "disable":
		err = h.Plugins.SetEnabled(r.Context(), id, action == "enable")
	case "restart":
		err = h.Plugins.Restart(id)
	default:
		http.Error(w, "Acción desconocida", http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	h.adminWrite(w, r, "PLUGIN_"+strings.ToUpper(action), func(tx *sql.Tx) (map[string]any, error) {
		return map[string]any{"plugin": id}, nil
	})
}

// --- Páginas de los plugins ---
//
// /admin/plugins/<id>/<ruta> llega al plugin como /<ruta>, ya con la sesión
// de administrador validada: el plugin recibe quién es el usuario en
// encabezados X-Opos-* y nunca ve la cookie de sesión. Si responde HTML con
// "X-Opos-Layout: admin", el POS lo pone dentro del panel (barra, estilos,
// Alpine y htmx) para que se vea como una sección más.

const pluginLayoutMax = 4 << 20 // 4 MB de HTML por página

// PluginProxy manda la petición al plugin.
func (h *POSHandler) PluginProxy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, ok := h.Plugins.Get(id)
	if !ok || !p.Ready() {
		http.Error(w, "Ese plugin no está instalado o está desactivado", http.StatusNotFound)
		return
	}
	rest := "/" + r.PathValue("path")
	if strings.HasPrefix(rest, "/_opos/") {
		http.NotFound(w, r) // rutas internas entre el POS y el plugin
		return
	}
	base := "/admin/plugins/" + id
	u, _ := currentUser(r.Context())

	proxy := &httputil.ReverseProxy{
		Transport: p.Transport(),
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host, pr.Out.Host = "http", "plugin", "plugin"
			pr.Out.URL.Path, pr.Out.URL.RawPath = rest, ""
			for k := range pr.Out.Header {
				if strings.HasPrefix(strings.ToLower(k), "x-opos-") {
					pr.Out.Header.Del(k)
				}
			}
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Set("Authorization", "Bearer "+p.Token())
			pr.Out.Header.Set("X-Opos-Base-Path", base)
			pr.Out.Header.Set("X-Opos-User-Id", strconv.FormatInt(u.ID, 10))
			pr.Out.Header.Set("X-Opos-User-Name", u.Name)
			pr.Out.Header.Set("X-Opos-User-Role", u.Role)
		},
		ModifyResponse: func(res *http.Response) error {
			res.Header.Del("Set-Cookie") // el plugin no maneja sesiones del POS
			if res.Header.Get("X-Opos-Layout") != "admin" || res.StatusCode != http.StatusOK ||
				!strings.HasPrefix(res.Header.Get("Content-Type"), "text/html") {
				return nil
			}
			body, err := io.ReadAll(io.LimitReader(res.Body, pluginLayoutMax+1))
			res.Body.Close()
			if err != nil {
				return err
			}
			if len(body) > pluginLayoutMax {
				return errors.New("la página del plugin es demasiado grande")
			}
			title := res.Header.Get("X-Opos-Title")
			if title == "" {
				title = p.Name
			}
			data := h.adminPageData(r, "plugins/"+id)
			data["Title"] = title
			data["Body"] = template.HTML(body) // el plugin instalado es de confianza
			data["Base"] = base
			t, err := page("admin/plugin_page.html", "admin/_nav.html")
			if err != nil {
				return err
			}
			var out bytes.Buffer
			if err := t.Execute(&out, data); err != nil {
				return err
			}
			res.Header.Del("X-Opos-Layout")
			res.Header.Del("X-Opos-Title")
			res.Header.Del("Content-Length")
			res.Header.Set("Content-Type", "text/html; charset=utf-8")
			res.ContentLength = int64(out.Len())
			res.Body = io.NopCloser(&out)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			log.Printf("[plugin %s] %s %s: %v", id, r.Method, rest, err)
			http.Error(w, "El plugin "+p.Name+" no responde. Revisa su estado en Administración → Plugins.", http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}
