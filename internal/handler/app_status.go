package handler

import (
	"net"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Terminales conectadas: IP de cada dispositivo de la red que usó el POS
// recientemente (el iPad, otra compu). La app de Mac lo consulta antes de
// cerrarse para avisar que esas terminales se quedarían sin sistema.
var terminals = struct {
	sync.Mutex
	seen map[string]time.Time
}{seen: map[string]time.Time{}}

// terminalWindow: una terminal con el POS abierto consulta mesas o Expo cada
// pocos segundos; si no se ha visto en este tiempo, ya no está conectada.
const terminalWindow = 90 * time.Second

// TrackTerminals registra la IP de cada petición que no viene de esta misma
// computadora.
func TrackTerminals(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			if ip := net.ParseIP(host); ip != nil && !ip.IsLoopback() {
				terminals.Lock()
				terminals.seen[ip.String()] = time.Now()
				terminals.Unlock()
			}
		}
		next.ServeHTTP(w, r)
	})
}

func activeTerminals() []string {
	terminals.Lock()
	defer terminals.Unlock()
	out := []string{}
	for ip, at := range terminals.seen {
		if time.Since(at) < terminalWindow {
			out = append(out, ip)
		} else {
			delete(terminals.seen, ip)
		}
	}
	sort.Strings(out)
	return out
}

func isLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(host)
	return err == nil && ip != nil && ip.IsLoopback()
}

// GET /api/app/status - Para la app de Mac: qué se interrumpiría al cerrarla.
// Solo responde a esta misma computadora.
func (h *POSHandler) AppStatus(w http.ResponseWriter, r *http.Request) {
	if !isLoopback(r) {
		http.Error(w, "Solo disponible en el servidor", http.StatusForbidden)
		return
	}
	var tables, orders int
	if err := h.DB.QueryRowContext(r.Context(), `
		SELECT (SELECT COUNT(*) FROM dining_tables WHERE status = 'OCCUPIED' AND COALESCE(is_active, 1) = 1),
		       (SELECT COUNT(*) FROM orders WHERE status = 'ACTIVE')`).Scan(&tables, &orders); err != nil {
		serverError(w, "Error consultando el estado", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"open_tables":   tables,
		"active_orders": orders,
		"terminals":     activeTerminals(),
	})
}
