package plugins

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Config dice dónde viven los plugins y cómo llegan a la API del POS.
type Config struct {
	Dir        string   // carpeta de plugins instalados (una subcarpeta por plugin)
	DataRoot   string   // datos de cada plugin (<DataRoot>/<id>), aparte para que sobrevivan a una actualización
	DevDirs    []string // plugins en desarrollo: el POS no los arranca, se conecta al que corre el programador
	RunDir     string   // sockets; vacío = carpeta temporal privada del usuario
	APIURL     string   // http://127.0.0.1:<puerto>
	POSVersion string
	DB         *sql.DB
}

// Estados de un plugin (se muestran en Administración → Plugins).
const (
	StateStarting   = "iniciando"
	StateRunning    = "corriendo"
	StateRestarting = "reiniciando"
	StateStopped    = "detenido"
	StateDisabled   = "desactivado"
	StateDevWaiting = "esperando (desarrollo)"
)

// Plugin es un plugin instalado (o en desarrollo).
type Plugin struct {
	Manifest
	Dir     string
	DataDir string
	Dev     bool

	socket string
	token  string
	client *http.Client

	mu        sync.Mutex
	enabled   bool
	state     string
	lastErr   string
	restarts  int
	since     time.Time
	stop      context.CancelFunc // detiene al supervisor
	done      chan struct{}      // se cierra cuando el supervisor termina
	cancelRun context.CancelFunc // termina el proceso actual (reinicio)
}

// Broken es una carpeta de plugins que no se pudo cargar.
type Broken struct {
	Dir   string `json:"dir"`
	Error string `json:"error"`
}

type Manager struct {
	cfg     Config
	ctx     context.Context
	wg      sync.WaitGroup
	mu      sync.RWMutex
	plugins map[string]*Plugin
	broken  []Broken
}

func New(cfg Config) *Manager {
	return &Manager{cfg: cfg, plugins: map[string]*Plugin{}}
}

// Start carga los plugins y arranca los activos. Se detienen al cancelar ctx
// (Wait espera a que terminen).
func (m *Manager) Start(ctx context.Context) error {
	m.ctx = ctx
	// El plugin corre con su carpeta como directorio de trabajo: con rutas
	// relativas (p. ej. POS_DB=pos.db) no encontraría su ejecutable.
	for _, dir := range []*string{&m.cfg.Dir, &m.cfg.DataRoot} {
		if *dir != "" {
			abs, err := filepath.Abs(*dir)
			if err != nil {
				return err
			}
			*dir = abs
		}
	}
	if m.cfg.RunDir == "" {
		m.cfg.RunDir = filepath.Join(os.TempDir(), fmt.Sprintf("opos-%d", os.Getuid()))
	}
	if err := privateDir(m.cfg.RunDir); err != nil {
		return fmt.Errorf("carpeta de sockets de plugins: %w", err)
	}
	m.load()

	for _, p := range m.List() {
		if _, err := m.cfg.DB.ExecContext(ctx, `INSERT OR IGNORE INTO plugins (id) VALUES (?)`, p.ID); err != nil {
			return err
		}
		var enabled bool
		if err := m.cfg.DB.QueryRowContext(ctx, `SELECT enabled FROM plugins WHERE id = ?`, p.ID).Scan(&enabled); err != nil {
			return err
		}
		p.enabled = enabled || p.Dev // en desarrollo siempre está activo
		if p.Dev {
			if err := m.writeDevEnv(p); err != nil {
				log.Printf("[plugin %s] no se pudo escribir .opos-dev.env: %v", p.ID, err)
			}
			log.Printf("[plugin %s] modo desarrollo: córrelo con `go run .` en %s", p.ID, p.Dir)
			continue
		}
		if p.enabled {
			m.startSupervisor(p)
		} else {
			p.setState(StateDisabled, "")
		}
	}

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.pruneLoop(ctx)
	}()
	return nil
}

// Wait espera a que se detengan todos los plugins.
func (m *Manager) Wait() { m.wg.Wait() }

// load busca los plugins instalados y los de desarrollo.
func (m *Manager) load() {
	found := map[string]*Plugin{}
	var broken []Broken
	add := func(dir string, dev bool) {
		man, err := ReadManifest(dir, m.cfg.POSVersion)
		if err != nil {
			log.Printf("Plugin en %s no se cargó: %v", dir, err)
			broken = append(broken, Broken{Dir: dir, Error: err.Error()})
			return
		}
		if old, dup := found[man.ID]; dup {
			if old.Dev {
				return // el de desarrollo reemplaza al instalado
			}
			if !dev {
				broken = append(broken, Broken{Dir: dir, Error: "id repetido: " + man.ID})
				return
			}
		}
		p := &Plugin{Manifest: man, Dir: dir, Dev: dev, token: newToken(), state: StateStopped}
		p.socket = filepath.Join(m.cfg.RunDir, man.ID+".sock")
		p.DataDir = filepath.Join(m.cfg.DataRoot, man.ID)
		if dev {
			p.DataDir = filepath.Join(dir, ".opos-data")
		}
		p.client = &http.Client{Transport: p.Transport()}
		found[man.ID] = p
	}

	for _, dir := range m.cfg.DevDirs {
		if abs, err := filepath.Abs(dir); err == nil {
			add(abs, true)
		}
	}
	entries, err := os.ReadDir(m.cfg.Dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("No se pudo leer la carpeta de plugins %s: %v", m.cfg.Dir, err)
	}
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		add(filepath.Join(m.cfg.Dir, name), false)
	}

	m.mu.Lock()
	m.plugins, m.broken = found, broken
	m.mu.Unlock()
}

// List regresa los plugins ordenados por nombre.
func (m *Manager) List() []*Plugin {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Plugin, 0, len(m.plugins))
	for _, p := range m.plugins {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Broken regresa las carpetas que no se pudieron cargar.
func (m *Manager) Broken() []Broken {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.broken
}

// Get busca un plugin por id.
func (m *Manager) Get(id string) (*Plugin, bool) {
	if m == nil {
		return nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.plugins[id]
	return p, ok
}

// ByToken identifica al plugin que llama a la API.
func (m *Manager) ByToken(token string) (*Plugin, bool) {
	if token == "" {
		return nil, false
	}
	for _, p := range m.List() {
		if subtle.ConstantTimeCompare([]byte(p.token), []byte(token)) == 1 {
			return p, true
		}
	}
	return nil, false
}

// SetEnabled activa o desactiva un plugin (queda guardado).
func (m *Manager) SetEnabled(ctx context.Context, id string, enabled bool) error {
	p, ok := m.Get(id)
	if !ok {
		return fmt.Errorf("no existe el plugin %q", id)
	}
	if p.Dev {
		return fmt.Errorf("un plugin en desarrollo no se puede desactivar desde aquí")
	}
	if _, err := m.cfg.DB.ExecContext(ctx,
		`UPDATE plugins SET enabled = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, enabled, id); err != nil {
		return err
	}
	p.mu.Lock()
	was, stop := p.enabled, p.stop
	p.enabled = enabled
	p.mu.Unlock()
	switch {
	case enabled && !was:
		m.startSupervisor(p)
	case !enabled && was && stop != nil:
		stop()
	}
	return nil
}

// Restart termina el proceso del plugin; su supervisor lo arranca de nuevo.
func (m *Manager) Restart(id string) error {
	p, ok := m.Get(id)
	if !ok {
		return fmt.Errorf("no existe el plugin %q", id)
	}
	p.mu.Lock()
	cancel := p.cancelRun
	p.mu.Unlock()
	if p.Dev || cancel == nil {
		return fmt.Errorf("el plugin no está corriendo")
	}
	cancel()
	return nil
}

// --- Supervisión ---

func (m *Manager) startSupervisor(p *Plugin) {
	ctx, stop := context.WithCancel(m.ctx)
	done := make(chan struct{})
	p.mu.Lock()
	prev := p.done
	p.stop, p.done = stop, done
	p.mu.Unlock()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer close(done)
		if prev != nil {
			<-prev // si se reactivó rápido, que termine de apagarse el anterior
		}
		m.supervise(ctx, p)
	}()
}

// supervise mantiene vivo el plugin: si se cae lo vuelve a arrancar, cada
// vez esperando más (hasta un minuto) para no ciclarse si falla al iniciar.
func (m *Manager) supervise(ctx context.Context, p *Plugin) {
	backoff := time.Second
	for {
		runCtx, cancelRun := context.WithCancel(ctx)
		p.mu.Lock()
		p.cancelRun = cancelRun
		p.mu.Unlock()

		started := time.Now()
		err := m.runOnce(runCtx, p)
		restarted := runCtx.Err() != nil && ctx.Err() == nil
		cancelRun()

		if ctx.Err() != nil {
			p.mu.Lock()
			p.cancelRun = nil
			enabled := p.enabled
			p.mu.Unlock()
			if enabled {
				p.setState(StateStopped, "")
			} else {
				p.setState(StateDisabled, "")
			}
			return
		}
		if restarted {
			log.Printf("[plugin %s] reiniciado desde el panel", p.ID)
			backoff = time.Second
			continue
		}
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		msg := "terminó sin error"
		if err != nil {
			msg = err.Error()
		}
		p.mu.Lock()
		p.restarts++
		p.mu.Unlock()
		p.setState(StateRestarting, msg)
		log.Printf("[plugin %s] se detuvo (%s); se reinicia en %s", p.ID, msg, backoff)
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
}

// runOnce corre el plugin hasta que termina o se cancela ctx.
func (m *Manager) runOnce(ctx context.Context, p *Plugin) error {
	if err := os.MkdirAll(p.DataDir, 0o700); err != nil {
		return fmt.Errorf("carpeta de datos: %w", err)
	}
	os.Remove(p.socket) // de una corrida anterior

	cmd := exec.CommandContext(ctx, p.executablePath(p.Dir))
	cmd.Dir = p.Dir
	cmd.Env = append(cleanEnv(), m.env(p, true)...)
	// Al detenerse se le pide terminar (guarda lo pendiente); si en 10 s no
	// sale, se mata.
	cmd.Cancel = func() error {
		if runtime.GOOS == "windows" {
			return cmd.Process.Kill()
		}
		return cmd.Process.Signal(os.Interrupt)
	}
	cmd.WaitDelay = 10 * time.Second
	out := &lineLogger{prefix: "[plugin " + p.ID + "] "}
	cmd.Stdout, cmd.Stderr = out, out

	p.setState(StateStarting, "")
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() {
		if err := p.waitHealthy(ctx, 20*time.Second); err != nil {
			if ctx.Err() == nil {
				log.Printf("[plugin %s] no respondió al arrancar: %v", p.ID, err)
				p.setState(StateStarting, "no responde en su socket")
			}
			return
		}
		p.setState(StateRunning, "")
		log.Printf("[plugin %s] %s %s corriendo", p.ID, p.Name, p.Version)
	}()
	err := cmd.Wait()
	out.flush()
	return err
}

// env son las variables que recibe el plugin (ver sdk/).
func (m *Manager) env(p *Plugin, supervised bool) []string {
	env := []string{
		"OPOS_PLUGIN_ID=" + p.ID,
		"OPOS_SOCKET=" + p.socket,
		"OPOS_TOKEN=" + p.token,
		"OPOS_API_URL=" + m.cfg.APIURL,
		"OPOS_DATA_DIR=" + p.DataDir,
		"OPOS_POS_VERSION=" + m.cfg.POSVersion,
	}
	if supervised {
		env = append(env, "OPOS_SUPERVISED=1")
	}
	return env
}

// writeDevEnv deja las variables en la carpeta del plugin en desarrollo; el
// SDK las lee si no vienen en el entorno. Cambian cada vez que arranca el POS.
func (m *Manager) writeDevEnv(p *Plugin) error {
	var b strings.Builder
	b.WriteString("# Generado por Open POS al arrancar. No lo subas al repositorio.\n")
	for _, kv := range m.env(p, false) {
		b.WriteString(kv + "\n")
	}
	return os.WriteFile(filepath.Join(p.Dir, ".opos-dev.env"), []byte(b.String()), 0o600)
}

// cleanEnv es el entorno del POS sin sus propias variables.
func cleanEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "POS_") && !strings.HasPrefix(kv, "OPOS_") {
			out = append(out, kv)
		}
	}
	return out
}

// --- Comunicación con el plugin ---

// Transport llega al plugin por su socket.
func (p *Plugin) Transport() http.RoundTripper {
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", p.socket)
		},
		MaxIdleConns:    4,
		IdleConnTimeout: time.Minute,
	}
}

// Token autentica al POS ante el plugin (y al plugin ante la API).
func (p *Plugin) Token() string { return p.token }

// call hace una petición interna al plugin (/_opos/...).
func (p *Plugin) call(ctx context.Context, method, path string, body any) error {
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://plugin"+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("Content-Type", "application/json")
	res, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		var msg bytes.Buffer
		msg.ReadFrom(res.Body)
		return fmt.Errorf("%s: %s", res.Status, strings.TrimSpace(msg.String()))
	}
	return nil
}

func (p *Plugin) ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return p.call(ctx, http.MethodGet, "/_opos/health", nil)
}

func (p *Plugin) waitHealthy(ctx context.Context, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	for {
		err := p.ping(ctx)
		if err == nil || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// --- Estado para el panel ---

func (p *Plugin) setState(state, errMsg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state != state {
		p.since = time.Now()
	}
	p.state, p.lastErr = state, errMsg
}

// Status es lo que ve el administrador de cada plugin.
type Status struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Version     string    `json:"version"`
	Description string    `json:"description"`
	Dir         string    `json:"dir"`
	DataDir     string    `json:"data_dir"`
	Dev         bool      `json:"dev"`
	Enabled     bool      `json:"enabled"`
	State       string    `json:"state"`
	Error       string    `json:"error,omitempty"`
	Restarts    int       `json:"restarts"`
	Since       time.Time `json:"since"`
	Permissions []string  `json:"permissions"`
	AdminLabel  string    `json:"admin_label,omitempty"`
	Cursor      int64     `json:"cursor"`
	Pending     int64     `json:"pending"` // eventos que aún no lee
}

// Status junta el estado de un plugin. Para los de desarrollo pregunta si
// hay alguien escuchando en el socket.
func (m *Manager) Status(ctx context.Context, p *Plugin) Status {
	p.mu.Lock()
	s := Status{ID: p.ID, Name: p.Name, Version: p.Version, Description: p.Description, Dir: p.Dir,
		DataDir: p.DataDir, Dev: p.Dev, Enabled: p.enabled, State: p.state, Error: p.lastErr,
		Restarts: p.restarts, Since: p.since, Permissions: p.Permissions}
	p.mu.Unlock()
	if s.Permissions == nil {
		s.Permissions = []string{}
	}
	if p.Admin != nil {
		s.AdminLabel = p.Admin.Label
	}
	if p.Dev {
		s.State = StateDevWaiting
		if p.ping(ctx) == nil {
			s.State = StateRunning
		}
	}
	m.cfg.DB.QueryRowContext(ctx, `
		SELECT cursor, (SELECT COALESCE(MAX(seq), 0) FROM plugin_events) - cursor FROM plugins WHERE id = ?`, p.ID).
		Scan(&s.Cursor, &s.Pending)
	if s.Pending < 0 {
		s.Pending = 0
	}
	return s
}

// Ready indica si vale la pena mandarle peticiones (activo o en desarrollo).
func (p *Plugin) Ready() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.Dev || p.enabled
}

// --- Respaldos ---

// Backup pide a cada plugin activo que respalde sus datos en
// <backupDir>/plugins/<id>/<stamp>/ y conserva los keep más recientes.
func (m *Manager) Backup(ctx context.Context, backupDir, stamp string, keep int) {
	for _, p := range m.List() {
		if !p.Ready() {
			continue
		}
		if err := p.ping(ctx); err != nil {
			log.Printf("[plugin %s] no está corriendo; su respaldo se hará la próxima vez", p.ID)
			continue
		}
		base := filepath.Join(backupDir, "plugins", p.ID)
		dir := filepath.Join(base, stamp)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			log.Printf("[plugin %s] respaldo: %v", p.ID, err)
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		err := p.call(cctx, http.MethodPost, "/_opos/backup", map[string]string{"dir": dir})
		cancel()
		if err != nil {
			log.Printf("[plugin %s] respaldo fallido: %v", p.ID, err)
			os.RemoveAll(dir)
			continue
		}
		pruneDirs(base, keep)
	}
}

func pruneDirs(base string, keep int) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names))) // los nombres son fechas
	for i := keep; i < len(names); i++ {
		os.RemoveAll(filepath.Join(base, names[i]))
	}
}

// --- Varios ---

func (m *Manager) pruneLoop(ctx context.Context) {
	prune := func() {
		if n, err := Prune(ctx, m.cfg.DB); err != nil {
			log.Printf("Error depurando eventos de plugins: %v", err)
		} else if n > 0 {
			log.Printf("Eventos de plugins depurados: %d", n)
		}
	}
	prune()
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			prune()
		}
	}
}

func newToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// privateDir crea una carpeta que solo puede leer este usuario (ahí van los
// sockets: quien pudiera abrirlos hablaría con el plugin).
func privateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s no es una carpeta", dir)
	}
	return os.Chmod(dir, 0o700)
}

// lineLogger pasa la salida del plugin al registro del POS, línea por línea.
type lineLogger struct {
	mu     sync.Mutex
	prefix string
	buf    []byte
}

func (l *lineLogger) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = append(l.buf, b...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			break
		}
		log.Print(l.prefix + string(l.buf[:i]))
		l.buf = l.buf[i+1:]
	}
	if len(l.buf) > 4096 { // una línea enorme sin salto
		log.Print(l.prefix + string(l.buf))
		l.buf = l.buf[:0]
	}
	return len(b), nil
}

func (l *lineLogger) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.buf) > 0 {
		log.Print(l.prefix + string(l.buf))
		l.buf = nil
	}
}
