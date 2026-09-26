package handler

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// --- Terminales ---
//
// La compu central (donde corre el servidor) tiene acceso completo. Cualquier
// otro dispositivo (iPad, teléfono) se identifica con una cookie propia y por
// omisión es terminal de mesero: vista simplificada, se entra con PIN y solo
// puede tomar órdenes, ver mesas y Expo, marcar entregas, reimprimir comandas
// e imprimir la pre-cuenta. Cobrar, caja, cancelar/editar y el panel de admin
// quedan fuera, y se bloquean aquí, en el servidor. El admin puede dar acceso
// completo a un dispositivo (una segunda caja).

const (
	TerminalWaiter = "WAITER"
	TerminalFull   = "FULL"
	TerminalRemote = "REMOTE" // por Tailscale: solo el panel de admin

	terminalCookie  = "opos_term"
	terminalSession = 12 * time.Hour // un turno; se renueva con el uso
)

// Terminal es quién está usando el POS en esta petición.
type Terminal struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Central  bool   `json:"central"` // la compu donde corre el servidor
	UserID   int64  `json:"-"`
	UserName string `json:"user"`
	UserRole string `json:"role"`
}

const ctxTerminal ctxKey = 100

// terminalFrom regresa la terminal de la petición. Sin terminal en el
// contexto (pruebas, llamadas internas) se considera la compu central.
func terminalFrom(ctx context.Context) Terminal {
	if t, ok := ctx.Value(ctxTerminal).(Terminal); ok {
		return t
	}
	return Terminal{Kind: TerminalFull, Central: true}
}

// isWaiterTerminal indica si la petición viene de una terminal de mesero.
func isWaiterTerminal(ctx context.Context) bool { return terminalFrom(ctx).Kind == TerminalWaiter }

// terminalUser regresa el id del usuario que tiene la sesión en la terminal
// (nil en la compu central), para guardar quién hizo qué.
func terminalUser(ctx context.Context) any {
	if t := terminalFrom(ctx); t.UserID > 0 {
		return t.UserID
	}
	return nil
}

type route struct {
	method string
	re     *regexp.Regexp
}

func routes(method string, patterns ...string) []route {
	out := make([]route, len(patterns))
	for i, p := range patterns {
		out[i] = route{method, regexp.MustCompile("^" + p + "$")}
	}
	return out
}

// Lo único que puede hacer una terminal de mesero.
var waiterAllowed = append(append(
	routes("GET", `/`, `/static/.*`, `/favicon\.ico`, `/api/terminal/me`,
		`/api/menu/tree`, `/api/menu/modifiers`, `/api/tables`, `/api/tables/floor`, `/api/tables/\d+/guests`,
		`/api/expo/html`),
	routes("POST", `/api/terminal/login`, `/api/terminal/logout`,
		`/api/orders/add`, `/api/order-items/\d+/(deliver|undeliver)`,
		`/api/orders/\d+/(deliver|reprint|prebill)`)...),
	routes("HEAD", `/`, `/static/.*`)...)

// Acceso remoto por Tailscale (direcciones 100.64.0.0/10 y fd7a:115c:a1e0::/48):
// solo el panel de administración, que pide su propio PIN de admin.
var remoteAllowed = append(
	routes("GET", `/admin(/.*)?`, `/api/admin/.*`, `/static/.*`, `/favicon\.ico`, `/tickets/\d+`,
		`/api/printers(/detect)?`, `/api/areas`),
	routes("POST", `/api/admin/.*`)...)

var tailscaleNets = func() []*net.IPNet {
	var out []*net.IPNet
	for _, c := range []string{"100.64.0.0/10", "fd7a:115c:a1e0::/48"} {
		_, n, _ := net.ParseCIDR(c)
		out = append(out, n)
	}
	return out
}()

// isTailscale indica si la petición llega por la red privada de Tailscale.
func isTailscale(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(host)
	if err != nil || ip == nil {
		return false
	}
	for _, n := range tailscaleNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// Sin sesión solo se puede cargar la página (que pide el PIN) e iniciar sesión.
var waiterPublic = append(
	routes("GET", `/`, `/static/.*`, `/favicon\.ico`, `/api/terminal/me`),
	routes("POST", `/api/terminal/login`, `/api/terminal/logout`)...)

func matches(list []route, r *http.Request) bool {
	for _, rt := range list {
		if rt.method == r.Method && rt.re.MatchString(r.URL.Path) {
			return true
		}
	}
	return false
}

// Para no escribir en la base en cada consulta de cada iPad (cada 4-5 s).
var terminalSeen = struct {
	sync.Mutex
	at map[int64]time.Time
}{at: map[int64]time.Time{}}

// TerminalGuard identifica la terminal de cada petición y aplica sus permisos.
func (h *POSHandler) TerminalGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isLoopback(r) {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxTerminal,
				Terminal{Kind: TerminalFull, Central: true, Name: "Compu central"})))
			return
		}
		if isTailscale(r) {
			ctx := context.WithValue(r.Context(), ctxTerminal, Terminal{Kind: TerminalRemote, Name: "Admin remoto"})
			switch {
			case matches(remoteAllowed, r) || (r.Method != http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/admin/")):
				next.ServeHTTP(w, r.WithContext(ctx))
			case r.Method == http.MethodGet && !strings.HasPrefix(r.URL.Path, "/api/"):
				http.Redirect(w, r, "/admin", http.StatusSeeOther)
			default:
				http.Error(w, "El acceso remoto es solo para el panel de administración.", http.StatusForbidden)
			}
			return
		}
		t, err := h.resolveTerminal(w, r)
		if err != nil {
			serverError(w, "Error identificando la terminal", err)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), ctxTerminal, t))
		if t.Kind == TerminalFull {
			next.ServeHTTP(w, r)
			return
		}
		if !matches(waiterAllowed, r) {
			if r.Method == http.MethodGet && !strings.HasPrefix(r.URL.Path, "/api/") {
				http.Redirect(w, r, "/", http.StatusSeeOther)
				return
			}
			http.Error(w, "Esta terminal no tiene permiso para esto: se hace desde caja.", http.StatusForbidden)
			return
		}
		if t.UserID == 0 && !matches(waiterPublic, r) {
			http.Error(w, "Inicia sesión con tu PIN.", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// resolveTerminal busca (o registra) la terminal por su cookie.
func (h *POSHandler) resolveTerminal(w http.ResponseWriter, r *http.Request) (Terminal, error) {
	ctx := r.Context()
	ip := terminalOf(r)["ip"]
	if c, err := r.Cookie(terminalCookie); err == nil && c.Value != "" {
		var t Terminal
		var userID sql.NullInt64
		var userName, userRole sql.NullString
		var expires sql.NullTime
		err := h.DB.QueryRowContext(ctx, `
			SELECT t.id, t.name, t.kind, t.user_id, u.name, u.role, t.session_expires_at
			FROM terminals t LEFT JOIN users u ON u.id = t.user_id AND u.is_active = 1
			WHERE t.token_hash = ?`, hashToken(c.Value)).
			Scan(&t.ID, &t.Name, &t.Kind, &userID, &userName, &userRole, &expires)
		if err == nil {
			if userID.Valid && userName.Valid && expires.Valid && time.Now().Before(expires.Time) {
				t.UserID, t.UserName, t.UserRole = userID.Int64, userName.String, userRole.String
			}
			h.touchTerminal(ctx, t, ip, r.UserAgent())
			return t, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return Terminal{}, err
		}
	}
	// Dispositivo nuevo: se registra solo al abrir la página del POS. Otras
	// peticiones sin cookie (vistas previas, miniaturas) no crean terminales.
	if r.Method != http.MethodGet || r.URL.Path != "/" {
		return Terminal{Kind: TerminalWaiter}, nil
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return Terminal{}, err
	}
	token := hex.EncodeToString(raw)
	res, err := h.DB.ExecContext(ctx, `
		INSERT INTO terminals (token_hash, kind, ip, user_agent) VALUES (?, ?, ?, ?)`,
		hashToken(token), TerminalWaiter, ip, r.UserAgent())
	if err != nil {
		return Terminal{}, err
	}
	id, _ := res.LastInsertId()
	name := fmt.Sprintf("Terminal %d", id)
	h.DB.ExecContext(ctx, `UPDATE terminals SET name = ? WHERE id = ?`, name, id)
	http.SetCookie(w, &http.Cookie{Name: terminalCookie, Value: token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: 5 * 365 * 24 * 3600})
	return Terminal{ID: id, Name: name, Kind: TerminalWaiter}, nil
}

// PruneTerminals borra las terminales que nadie ha usado en 30 días y no
// tienen sesión (dispositivos de paso).
func PruneTerminals(ctx context.Context, db *sql.DB) {
	db.ExecContext(ctx, `DELETE FROM terminals WHERE user_id IS NULL AND kind = 'WAITER'
		AND last_seen_at < datetime('now', '-30 days')`)
}

// touchTerminal anota la última actividad (y renueva la sesión) a lo más una
// vez por minuto.
func (h *POSHandler) touchTerminal(ctx context.Context, t Terminal, ip, ua string) {
	terminalSeen.Lock()
	last := terminalSeen.at[t.ID]
	if time.Since(last) < time.Minute {
		terminalSeen.Unlock()
		return
	}
	terminalSeen.at[t.ID] = time.Now()
	terminalSeen.Unlock()
	if t.UserID > 0 {
		h.DB.ExecContext(ctx, `UPDATE terminals SET last_seen_at = CURRENT_TIMESTAMP, ip = ?, user_agent = ?,
			session_expires_at = ? WHERE id = ?`, ip, ua, utcSQL(time.Now().Add(terminalSession)), t.ID)
		return
	}
	h.DB.ExecContext(ctx, `UPDATE terminals SET last_seen_at = CURRENT_TIMESTAMP, ip = ?, user_agent = ? WHERE id = ?`,
		ip, ua, t.ID)
}

// GET /api/terminal/me - Qué terminal es esta y quién la usa.
func (h *POSHandler) TerminalMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, terminalFrom(r.Context()))
}

// POST /api/terminal/login - Entrar en la terminal con PIN.
func (h *POSHandler) TerminalLogin(w http.ResponseWriter, r *http.Request) {
	t := terminalFrom(r.Context())
	if t.ID == 0 {
		http.Error(w, "La compu central no necesita iniciar sesión", http.StatusBadRequest)
		return
	}
	var req pinRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "JSON inválido", http.StatusBadRequest)
		return
	}
	ip := terminalOf(r)["ip"]
	if d := h.logins.wait(ip); d > 0 {
		http.Error(w, fmt.Sprintf("Demasiados intentos de PIN. Espera %d segundos.", int(d.Seconds())+1), http.StatusTooManyRequests)
		return
	}
	u, err := userByPIN(r.Context(), h.DB, req.PIN)
	if errors.Is(err, errBadPIN) {
		h.logins.fail(ip)
		http.Error(w, "PIN incorrecto", http.StatusUnauthorized)
		return
	}
	if err != nil {
		serverError(w, "Error validando PIN", err)
		return
	}
	h.logins.success(ip)
	if _, err := h.DB.ExecContext(r.Context(), `
		UPDATE terminals SET user_id = ?, session_expires_at = ?, last_seen_at = CURRENT_TIMESTAMP WHERE id = ?`,
		u.ID, utcSQL(time.Now().Add(terminalSession)), t.ID); err != nil {
		serverError(w, "Error iniciando sesión", err)
		return
	}
	tx, err := h.DB.BeginTx(r.Context(), nil)
	if err == nil {
		auditBy(r.Context(), tx, u, r, "TERMINAL_LOGIN", map[string]any{"terminal": t.Name})
		tx.Commit()
	}
	t.UserID, t.UserName, t.UserRole = u.ID, u.Name, u.Role
	writeJSON(w, http.StatusOK, t)
}

// POST /api/terminal/logout
func (h *POSHandler) TerminalLogout(w http.ResponseWriter, r *http.Request) {
	if t := terminalFrom(r.Context()); t.ID > 0 {
		h.DB.ExecContext(r.Context(), `UPDATE terminals SET user_id = NULL, session_expires_at = NULL WHERE id = ?`, t.ID)
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- Admin: terminales ---

type TerminalRow struct {
	ID       int64      `json:"id"`
	Name     string     `json:"name"`
	Kind     string     `json:"kind"`
	User     string     `json:"user"`
	IP       string     `json:"ip"`
	Device   string     `json:"device"`
	LastSeen *time.Time `json:"last_seen"`
}

// GET /api/admin/terminals
func (h *POSHandler) AdminListTerminals(w http.ResponseWriter, r *http.Request) {
	out := []TerminalRow{}
	err := eachRow(r.Context(), h.DB, `
		SELECT t.id, t.name, t.kind,
		       CASE WHEN t.session_expires_at > CURRENT_TIMESTAMP THEN COALESCE(u.name, '') ELSE '' END,
		       COALESCE(t.ip, ''), COALESCE(t.user_agent, ''), t.last_seen_at
		FROM terminals t LEFT JOIN users u ON u.id = t.user_id
		ORDER BY t.last_seen_at DESC`, nil, func(scan func(...any) error) error {
		var t TerminalRow
		var seen sql.NullTime
		var ua string
		if err := scan(&t.ID, &t.Name, &t.Kind, &t.User, &t.IP, &ua, &seen); err != nil {
			return err
		}
		t.Device = deviceName(ua)
		if seen.Valid {
			t.LastSeen = &seen.Time
		}
		out = append(out, t)
		return nil
	})
	if err != nil {
		serverError(w, "Error consultando terminales", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func deviceName(ua string) string {
	switch {
	case strings.Contains(ua, "iPad"):
		return "iPad"
	case strings.Contains(ua, "iPhone"):
		return "iPhone"
	case strings.Contains(ua, "Android"):
		return "Android"
	case strings.Contains(ua, "Macintosh"):
		// Los iPad recientes se presentan como Mac en Safari.
		return "Mac / iPad"
	case strings.Contains(ua, "Windows"):
		return "Windows"
	}
	return "Navegador"
}

type terminalUpdate struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// PUT /api/admin/terminals/{id} - Nombre y tipo de acceso de una terminal.
func (h *POSHandler) AdminUpdateTerminal(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.Error(w, "ID inválido", http.StatusBadRequest)
		return
	}
	var req terminalUpdate
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "JSON inválido", http.StatusBadRequest)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || (req.Kind != TerminalWaiter && req.Kind != TerminalFull) {
		http.Error(w, "Nombre y tipo de acceso son obligatorios", http.StatusBadRequest)
		return
	}
	h.adminWrite(w, r, "TERMINAL_UPDATED", func(tx *sql.Tx) (map[string]any, error) {
		res, err := tx.ExecContext(r.Context(), `UPDATE terminals SET name = ?, kind = ? WHERE id = ?`, req.Name, req.Kind, id)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil, errNotFound
		}
		return map[string]any{"terminal_id": id, "name": req.Name, "kind": req.Kind}, nil
	})
}

// POST /api/admin/terminals/{id}/logout - Cierra la sesión de quien la usa.
func (h *POSHandler) AdminLogoutTerminal(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.Error(w, "ID inválido", http.StatusBadRequest)
		return
	}
	h.adminWrite(w, r, "TERMINAL_LOGGED_OUT", func(tx *sql.Tx) (map[string]any, error) {
		if _, err := tx.ExecContext(r.Context(), `UPDATE terminals SET user_id = NULL, session_expires_at = NULL WHERE id = ?`, id); err != nil {
			return nil, err
		}
		return map[string]any{"terminal_id": id}, nil
	})
}
