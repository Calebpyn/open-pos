package handler

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Roles de usuario, de mayor a menor permiso.
const (
	RoleAdmin   = "ADMIN"
	RoleManager = "MANAGER"
	RoleCashier = "CASHIER"
	RoleWaiter  = "WAITER"
)

var roleLabels = map[string]string{
	RoleAdmin:   "Administrador",
	RoleManager: "Encargado",
	RoleCashier: "Cajero",
	RoleWaiter:  "Mesero",
}

// canOperateCash: abrir/cerrar turno, movimientos y cajón.
func canOperateCash(role string) bool { return role != RoleWaiter }

const (
	adminCookie      = "opos_admin"
	adminIdleTimeout = 30 * time.Minute
	adminMaxAge      = 12 * time.Hour
)

// --- Límite de intentos de PIN ---

// loginLimiter frena a quien prueba PINs: un PIN de 4 dígitos se adivina en
// minutos si no hay límite. Se lleva por IP y vive en memoria.
type loginLimiter struct {
	mu    sync.Mutex
	now   func() time.Time
	state map[string]*loginState
}

type loginState struct {
	fails       int
	windowStart time.Time
	lockedUntil time.Time
}

const (
	loginMaxFails = 5
	loginWindow   = 5 * time.Minute
	loginLockout  = 2 * time.Minute
)

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{now: time.Now, state: map[string]*loginState{}}
}

// wait regresa cuánto falta para poder intentar de nuevo (0 si ya puede).
func (l *loginLimiter) wait(ip string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if s := l.state[ip]; s != nil {
		if d := s.lockedUntil.Sub(l.now()); d > 0 {
			return d
		}
	}
	return 0
}

func (l *loginLimiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	s := l.state[ip]
	if s == nil || now.Sub(s.windowStart) > loginWindow {
		s = &loginState{windowStart: now}
		l.state[ip] = s
	}
	s.fails++
	if s.fails >= loginMaxFails {
		s.lockedUntil = now.Add(loginLockout)
		s.fails, s.windowStart = 0, now
	}
}

func (l *loginLimiter) success(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.state, ip)
}

// --- Sesiones ---

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

type ctxKey int

const ctxUser ctxKey = iota

// currentUser regresa el usuario autenticado por RequireAdmin.
func currentUser(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(ctxUser).(User)
	return u, ok
}

// adminSessionUser valida la cookie de administración y renueva su actividad.
func (h *POSHandler) adminSessionUser(r *http.Request) (User, bool) {
	c, err := r.Cookie(adminCookie)
	if err != nil || c.Value == "" {
		return User{}, false
	}
	hash := hashToken(c.Value)
	var u User
	err = h.DB.QueryRowContext(r.Context(), `
		SELECT u.id, u.name, u.role FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = ? AND s.scope = 'ADMIN' AND s.revoked_at IS NULL
		  AND s.last_seen_at >= datetime('now', ?)
		  AND s.created_at >= datetime('now', ?)
		  AND u.is_active = 1 AND u.role = ?`,
		hash, sqliteAgo(adminIdleTimeout), sqliteAgo(adminMaxAge), RoleAdmin).
		Scan(&u.ID, &u.Name, &u.Role)
	if err != nil {
		return User{}, false
	}
	h.DB.ExecContext(r.Context(), `UPDATE sessions SET last_seen_at = CURRENT_TIMESTAMP WHERE token_hash = ?`, hash)
	return u, true
}

// sqliteAgo da el modificador de datetime() para "hace d".
func sqliteAgo(d time.Duration) string { return fmt.Sprintf("-%d seconds", int(d.Seconds())) }

// RequireAdmin protege una ruta: sin sesión de administrador, las páginas
// mandan al inicio de sesión y la API responde 401.
func (h *POSHandler) RequireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := h.adminSessionUser(r)
		if !ok {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				http.Error(w, "Tu sesión de administración expiró. Vuelve a ingresar tu PIN.", http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/admin/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxUser, u)))
	}
}

// POST /api/admin/login - Inicia sesión de administración con PIN.
func (h *POSHandler) AdminLogin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ip := terminalOf(r)["ip"]
	if d := h.logins.wait(ip); d > 0 {
		http.Error(w, fmt.Sprintf("Demasiados intentos. Espera %d segundos.", int(d.Seconds())+1), http.StatusTooManyRequests)
		return
	}
	var req pinRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "JSON inválido", http.StatusBadRequest)
		return
	}

	u, err := userByPIN(ctx, h.DB, req.PIN)
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
	if u.Role != RoleAdmin {
		http.Error(w, "Tu usuario no tiene acceso a administración", http.StatusForbidden)
		return
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		serverError(w, "Error creando la sesión", err)
		return
	}
	token := hex.EncodeToString(raw)

	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		serverError(w, "Error al iniciar transacción", err)
		return
	}
	defer tx.Rollback()
	term := terminalOf(r)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO sessions (token_hash, user_id, scope, ip, user_agent) VALUES (?, ?, 'ADMIN', ?, ?)`,
		hashToken(token), u.ID, term["ip"], term["user_agent"]); err != nil {
		serverError(w, "Error creando la sesión", err)
		return
	}
	if err := auditBy(ctx, tx, u, r, "ADMIN_LOGIN", map[string]any{}); err != nil {
		serverError(w, "Error registrando bitácora", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "Error creando la sesión", err)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name: adminCookie, Value: token, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
		MaxAge: int(adminMaxAge.Seconds()),
	})
	writeJSON(w, http.StatusOK, u)
}

// POST /api/admin/logout
func (h *POSHandler) AdminLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(adminCookie); err == nil {
		h.DB.ExecContext(r.Context(),
			`UPDATE sessions SET revoked_at = CURRENT_TIMESTAMP WHERE token_hash = ? AND revoked_at IS NULL`,
			hashToken(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: adminCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// revokeUserSessions cierra las sesiones de un usuario (al desactivarlo o
// cambiar su PIN).
func revokeUserSessions(ctx context.Context, tx *sql.Tx, userID int64) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE sessions SET revoked_at = CURRENT_TIMESTAMP WHERE user_id = ? AND revoked_at IS NULL`, userID)
	return err
}
