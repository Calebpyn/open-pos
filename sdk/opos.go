// Package opos sirve para escribir plugins de Open POS en Go.
//
// Un plugin es un ejecutable con un plugin.json en su carpeta. Open POS lo
// arranca, le pasa por variables de entorno cómo comunicarse y le habla por un
// socket Unix. Lo mínimo:
//
//	p, err := opos.New()
//	if err != nil { log.Fatal(err) }
//	p.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
//		opos.Page(w, "Hola", "<p>Hola, "+html.EscapeString(opos.UserFrom(r).Name)+"</p>")
//	})
//	go p.Consume(ctx, loadCursor, apply)
//	log.Fatal(p.Run(ctx))
//
// Para desarrollar, arranca Open POS con POS_PLUGIN_DEV=/ruta/a/tu/plugin y
// corre `go run .` en esa carpeta: el SDK toma la conexión de .opos-dev.env.
package opos

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Plugin es el proceso del plugin.
type Plugin struct {
	ID         string
	DataDir    string // carpeta de datos propia (la base del plugin va aquí)
	POSVersion string
	API        *Client

	token      string
	socket     string
	supervised bool
	mux        *http.ServeMux
	backup     func(ctx context.Context, dir string) error
}

// New lee la conexión del entorno (o de .opos-dev.env en desarrollo).
func New() (*Plugin, error) {
	env := map[string]string{}
	for _, k := range []string{"OPOS_PLUGIN_ID", "OPOS_SOCKET", "OPOS_TOKEN", "OPOS_API_URL", "OPOS_DATA_DIR", "OPOS_POS_VERSION", "OPOS_SUPERVISED"} {
		env[k] = os.Getenv(k)
	}
	if env["OPOS_TOKEN"] == "" {
		if err := readDevEnv(".opos-dev.env", env); err != nil {
			return nil, fmt.Errorf("sin conexión con Open POS: arráncalo con POS_PLUGIN_DEV=%s y corre el plugin desde esta carpeta (%w)", cwd(), err)
		}
	}
	for _, k := range []string{"OPOS_PLUGIN_ID", "OPOS_SOCKET", "OPOS_TOKEN", "OPOS_API_URL", "OPOS_DATA_DIR"} {
		if env[k] == "" {
			return nil, fmt.Errorf("falta %s", k)
		}
	}
	if err := os.MkdirAll(env["OPOS_DATA_DIR"], 0o700); err != nil {
		return nil, err
	}
	return &Plugin{
		ID:         env["OPOS_PLUGIN_ID"],
		DataDir:    env["OPOS_DATA_DIR"],
		POSVersion: env["OPOS_POS_VERSION"],
		API:        NewClient(env["OPOS_API_URL"], env["OPOS_TOKEN"]),
		token:      env["OPOS_TOKEN"],
		socket:     env["OPOS_SOCKET"],
		supervised: env["OPOS_SUPERVISED"] == "1",
		mux:        http.NewServeMux(),
	}, nil
}

func readDevEnv(path string, env map[string]string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			env[k] = v
		}
	}
	return sc.Err()
}

func cwd() string {
	d, _ := os.Getwd()
	return d
}

// Handle registra una página o API del plugin. Las rutas son relativas a
// /admin/plugins/<id>/ en el POS: "GET /{$}" es la portada del plugin.
func (p *Plugin) Handle(pattern string, h http.Handler) { p.mux.Handle(pattern, h) }

func (p *Plugin) HandleFunc(pattern string, h func(http.ResponseWriter, *http.Request)) {
	p.mux.HandleFunc(pattern, h)
}

// OnBackup registra cómo respaldar los datos del plugin: Open POS lo llama
// con cada respaldo diario y fn debe dejar una copia consistente en dir (con
// SQLite: VACUUM INTO dir/datos.db).
func (p *Plugin) OnBackup(fn func(ctx context.Context, dir string) error) { p.backup = fn }

// Run atiende al POS por el socket hasta que se cancela ctx, llega SIGINT o
// SIGTERM, o (si lo arrancó el POS) el POS deja de existir.
func (p *Plugin) Run(ctx context.Context) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if p.supervised {
		go watchParent(ctx, cancel)
	}

	os.Remove(p.socket) // de una corrida anterior
	ln, err := net.Listen("unix", p.socket)
	if err != nil {
		return fmt.Errorf("no se pudo abrir el socket %s: %w", p.socket, err)
	}
	os.Chmod(p.socket, 0o600)
	defer os.Remove(p.socket)

	internal := http.NewServeMux()
	internal.HandleFunc("GET /_opos/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true, "id": p.ID})
	})
	internal.HandleFunc("POST /_opos/backup", p.serveBackup)
	internal.Handle("/", p.mux)

	srv := &http.Server{Handler: p.auth(internal), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancelShutdown := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancelShutdown()
	srv.Shutdown(shutdown)
	return nil
}

// auth deja pasar solo al POS (lleva el token del plugin).
func (p *Plugin) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(token), []byte(p.token)) != 1 {
			http.Error(w, "no autorizado", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (p *Plugin) serveBackup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Dir string `json:"dir"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Dir == "" {
		http.Error(w, "falta dir", http.StatusBadRequest)
		return
	}
	if p.backup == nil {
		writeJSON(w, map[string]any{"ok": true, "skipped": true}) // no guarda datos
		return
	}
	if err := p.backup(r.Context(), req.Dir); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// watchParent termina el plugin si el POS que lo arrancó desaparece (p. ej.
// se cerró a la fuerza), para no dejarlo huérfano.
func watchParent(ctx context.Context, cancel context.CancelFunc) {
	parent := os.Getppid()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if os.Getppid() != parent {
				log.Println("Open POS ya no está; terminando")
				cancel()
				return
			}
		}
	}
}

// Consume procesa los eventos del POS en orden, en lotes, hasta que se
// cancela ctx. load da el último seq procesado (0 = desde lo más viejo que
// guarda el POS; usa API.LatestSeq para empezar desde ahora). apply debe
// guardar el efecto del lote Y el nuevo cursor (el Seq del último evento) en
// una sola transacción: así, si el plugin se cae a medias, al volver repite
// el lote completo y nada se cuenta dos veces. Si apply falla, se reintenta
// el mismo lote.
func (p *Plugin) Consume(ctx context.Context, load func(context.Context) (int64, error), apply func(context.Context, []Event) error) {
	backoff := time.Second
	pause := func(err error, what string) {
		log.Printf("%s: %v (reintento en %s)", what, err, backoff)
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}

	var after int64
	for {
		var err error
		if after, err = load(ctx); err == nil {
			break
		}
		if ctx.Err() != nil {
			return
		}
		pause(err, "no se pudo leer el cursor")
	}
	for ctx.Err() == nil {
		events, err := p.API.Events(ctx, after, 100, 25*time.Second)
		if err != nil {
			if ctx.Err() == nil {
				pause(err, "no se pudieron leer eventos")
			}
			continue
		}
		if len(events) == 0 {
			continue
		}
		if err := apply(ctx, events); err != nil {
			if ctx.Err() == nil {
				pause(err, fmt.Sprintf("no se pudo procesar el lote %d-%d", events[0].Seq, events[len(events)-1].Seq))
			}
			continue
		}
		after = events[len(events)-1].Seq
		backoff = time.Second
	}
}

// --- Para las páginas ---

// User es quien usa el panel (el POS ya validó su sesión de administrador).
type User struct {
	ID   int64
	Name string
	Role string
}

// UserFrom lee el usuario de la petición.
func UserFrom(r *http.Request) User {
	id, _ := strconv.ParseInt(r.Header.Get("X-Opos-User-Id"), 10, 64)
	return User{ID: id, Name: r.Header.Get("X-Opos-User-Name"), Role: r.Header.Get("X-Opos-User-Role")}
}

// BasePath es donde el POS muestra el plugin (/admin/plugins/<id>): úsalo
// para armar enlaces y formularios.
func BasePath(r *http.Request) string { return r.Header.Get("X-Opos-Base-Path") }

// Page responde un fragmento HTML que el POS pone dentro de su panel, con la
// barra de navegación, app.css, Alpine y htmx ya cargados. El HTML va tal
// cual: escapa lo que venga de usuarios (html/template lo hace solo).
func Page(w http.ResponseWriter, title, html string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Opos-Layout", "admin")
	w.Header().Set("X-Opos-Title", title)
	w.Write([]byte(html))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
