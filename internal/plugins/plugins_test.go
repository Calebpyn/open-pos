package plugins

import (
	"context"
	"database/sql"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/calebpyn/open-pos/internal/database"
)

// El binario de pruebas hace también de plugin falso: el Manager lo arranca
// con FAKE_PLUGIN_MODE en el entorno.
func TestMain(m *testing.M) {
	switch os.Getenv("FAKE_PLUGIN_MODE") {
	case "serve":
		fakePlugin()
		os.Exit(0)
	case "crash":
		os.Exit(3)
	}
	os.Exit(m.Run())
}

func fakePlugin() {
	token := os.Getenv("OPOS_TOKEN")
	ln, err := net.Listen("unix", os.Getenv("OPOS_SOCKET"))
	if err != nil {
		os.Exit(2)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_opos/health", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("POST /_opos/backup", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Dir string }
		json.NewDecoder(r.Body).Decode(&req)
		os.WriteFile(filepath.Join(req.Dir, "datos.txt"), []byte("hola"), 0o600)
	})
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
}

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := database.InitDB(filepath.Join(t.TempDir(), "pos.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func writePlugin(t *testing.T, root, id string, extra map[string]any) string {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(exe, filepath.Join(dir, "fake")); err != nil {
		t.Fatal(err)
	}
	m := map[string]any{
		"id": id, "name": "Plugin " + id, "version": "1.0.0", "api_version": 1,
		"executable": "fake", "permissions": []string{PermEvents},
	}
	for k, v := range extra {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestManifestValidation(t *testing.T) {
	ok := Manifest{ID: "inventario", Name: "Inventario", APIVersion: 1, Executable: "inventario",
		Permissions: []string{PermEvents, PermCatalog}, MinPOSVersion: "0.8.0"}
	if err := ok.validate("0.8.1-beta"); err != nil {
		t.Fatalf("manifiesto válido rechazado: %v", err)
	}
	if err := ok.validate("dev"); err != nil {
		t.Fatalf("una versión de desarrollo no debe bloquear: %v", err)
	}
	cases := map[string]func(m *Manifest){
		"id con mayúsculas":   func(m *Manifest) { m.ID = "Inventario" },
		"id con diagonal":     func(m *Manifest) { m.ID = "../x" },
		"sin nombre":          func(m *Manifest) { m.Name = " " },
		"otra API":            func(m *Manifest) { m.APIVersion = 2 },
		"ejecutable afuera":   func(m *Manifest) { m.Executable = "../../bin/sh" },
		"ejecutable absoluto": func(m *Manifest) { m.Executable = "/bin/sh" },
		"permiso inventado":   func(m *Manifest) { m.Permissions = []string{"db:write"} },
		"POS muy viejo":       func(m *Manifest) { m.MinPOSVersion = "1.0.0" },
		"admin sin etiqueta":  func(m *Manifest) { m.Admin = &AdminEntry{} },
	}
	for name, change := range cases {
		m := ok
		change(&m)
		if err := m.validate("0.8.1-beta"); err == nil {
			t.Errorf("%s: debió rechazarse", name)
		}
	}
}

func TestEventsEmitReadAndWait(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	emit := func(typ string) {
		t.Helper()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if err := Emit(ctx, tx, typ, map[string]any{"n": 1}); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}

	// Un evento de una transacción que se deshace no existe.
	tx, _ := db.BeginTx(ctx, nil)
	Emit(ctx, tx, "fantasma", nil)
	tx.Rollback()

	emit("uno")
	emit("dos")
	evs, err := ReadEvents(ctx, db, 0, 10, 0)
	if err != nil || len(evs) != 2 || evs[0].Type != "uno" || evs[1].Seq <= evs[0].Seq {
		t.Fatalf("eventos: %v %+v", err, evs)
	}
	if evs, _ := ReadEvents(ctx, db, evs[0].Seq, 10, 0); len(evs) != 1 || evs[0].Type != "dos" {
		t.Fatalf("después del primero debe venir solo el segundo: %+v", evs)
	}
	last := evs[1].Seq

	// Sin eventos nuevos, espera y despierta cuando se confirma uno.
	got := make(chan []Event)
	go func() {
		evs, _ := ReadEvents(ctx, db, last, 10, 10*time.Second)
		got <- evs
	}()
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	emit("tres")
	select {
	case evs := <-got:
		if len(evs) != 1 || evs[0].Type != "tres" {
			t.Fatalf("al despertar: %+v", evs)
		}
		if time.Since(start) > 2*time.Second {
			t.Fatalf("tardó %s en despertar", time.Since(start))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("la espera no despertó con el evento nuevo")
	}

	// Sin nada nuevo, respeta el tiempo de espera.
	start = time.Now()
	if evs, _ := ReadEvents(ctx, db, last+1, 10, 300*time.Millisecond); len(evs) != 0 || time.Since(start) < 250*time.Millisecond {
		t.Fatalf("sin eventos debe esperar y regresar vacío: %+v en %s", evs, time.Since(start))
	}
	if seq, err := LatestSeq(ctx, db); err != nil || seq != last+1 {
		t.Fatalf("último seq: %d %v", seq, err)
	}
}

func TestPrune(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	for _, q := range []string{
		`INSERT INTO plugin_events (seq, type, payload, created_at) VALUES
			(1, 'viejo', '{}', datetime('now', '-100 days')),
			(2, 'mes', '{}', datetime('now', '-40 days')),
			(3, 'mes', '{}', datetime('now', '-35 days')),
			(4, 'hoy', '{}', datetime('now'))`,
		`INSERT INTO plugins (id, enabled, cursor) VALUES ('lento', 1, 2), ('apagado', 0, 0)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := Prune(ctx, db); err != nil || n != 2 {
		t.Fatalf("debe borrar el de 100 días y el ya leído de 40: %d %v", n, err)
	}
	var left []int64
	rows, _ := db.Query(`SELECT seq FROM plugin_events ORDER BY seq`)
	for rows.Next() {
		var s int64
		rows.Scan(&s)
		left = append(left, s)
	}
	rows.Close()
	if len(left) != 2 || left[0] != 3 || left[1] != 4 {
		t.Fatalf("quedaron %v; el 3 no lo ha leído el plugin activo", left)
	}
}

func waitState(t *testing.T, m *Manager, id, want string) Status {
	t.Helper()
	p, _ := m.Get(id)
	deadline := time.Now().Add(10 * time.Second)
	for {
		s := m.Status(context.Background(), p)
		if s.State == want {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: estado %q (error %q), se esperaba %q", id, s.State, s.Error, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestManagerLifecycle(t *testing.T) {
	db := testDB(t)
	root := t.TempDir()
	t.Setenv("FAKE_PLUGIN_MODE", "serve")
	writePlugin(t, filepath.Join(root, "plugins"), "eco", map[string]any{"admin": map[string]string{"label": "Eco"}})
	writePlugin(t, filepath.Join(root, "plugins"), "roto", map[string]any{"api_version": 9})
	os.MkdirAll(filepath.Join(root, "plugins", ".oculta"), 0o755)

	ctx, cancel := context.WithCancel(context.Background())
	m := New(Config{
		Dir: filepath.Join(root, "plugins"), DataRoot: filepath.Join(root, "datos"),
		RunDir: shortTempDir(t), APIURL: "http://127.0.0.1:1", POSVersion: "dev", DB: db,
	})
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cancel()
		m.Wait()
	}()

	if len(m.List()) != 1 || len(m.Broken()) != 1 || !strings.Contains(m.Broken()[0].Error, "versión 9") {
		t.Fatalf("plugins %d, con error %+v", len(m.List()), m.Broken())
	}
	s := waitState(t, m, "eco", StateRunning)
	if s.AdminLabel != "Eco" || !s.Enabled || s.DataDir != filepath.Join(root, "datos", "eco") {
		t.Fatalf("estado: %+v", s)
	}
	p, _ := m.Get("eco")
	if got, ok := m.ByToken(p.Token()); !ok || got != p {
		t.Fatal("el token debe identificar al plugin")
	}
	if _, ok := m.ByToken("otro"); ok {
		t.Fatal("un token inventado no debe servir")
	}

	// Respaldo: el plugin escribe en su carpeta y se conservan los últimos.
	backups := filepath.Join(root, "respaldos")
	for _, stamp := range []string{"20260901-010000", "20260902-010000", "20260903-010000"} {
		m.Backup(ctx, backups, stamp, 2)
	}
	if _, err := os.Stat(filepath.Join(backups, "plugins", "eco", "20260903-010000", "datos.txt")); err != nil {
		t.Fatalf("respaldo del plugin: %v", err)
	}
	if _, err := os.Stat(filepath.Join(backups, "plugins", "eco", "20260901-010000")); !os.IsNotExist(err) {
		t.Fatal("el respaldo más viejo debió borrarse")
	}

	// Desactivar lo apaga y queda guardado; activar lo arranca de nuevo.
	if err := m.SetEnabled(ctx, "eco", false); err != nil {
		t.Fatal(err)
	}
	waitState(t, m, "eco", StateDisabled)
	var enabled bool
	db.QueryRow(`SELECT enabled FROM plugins WHERE id = 'eco'`).Scan(&enabled)
	if enabled {
		t.Fatal("desactivado debe quedar en la base")
	}
	if err := m.SetEnabled(ctx, "eco", true); err != nil {
		t.Fatal(err)
	}
	waitState(t, m, "eco", StateRunning)
	if err := m.Restart("eco"); err != nil {
		t.Fatal(err)
	}
	waitState(t, m, "eco", StateRunning)
}

// Con la base en una ruta relativa (POS_DB=pos.db, como en desarrollo) las
// carpetas de plugins también lo son; el plugin debe arrancar igual.
func TestManagerRelativeDirs(t *testing.T) {
	db := testDB(t)
	t.Chdir(t.TempDir())
	t.Setenv("FAKE_PLUGIN_MODE", "serve")
	writePlugin(t, "plugins", "rel", nil)
	ctx, cancel := context.WithCancel(context.Background())
	m := New(Config{Dir: "plugins", DataRoot: "plugins-datos", RunDir: shortTempDir(t), POSVersion: "dev", DB: db})
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cancel()
		m.Wait()
	}()
	if s := waitState(t, m, "rel", StateRunning); !filepath.IsAbs(s.Dir) || !filepath.IsAbs(s.DataDir) {
		t.Fatalf("rutas: %+v", s)
	}
}

func TestManagerRestartsCrashingPlugin(t *testing.T) {
	db := testDB(t)
	root := t.TempDir()
	t.Setenv("FAKE_PLUGIN_MODE", "crash")
	writePlugin(t, root, "falla", nil)

	ctx, cancel := context.WithCancel(context.Background())
	m := New(Config{Dir: root, DataRoot: t.TempDir(), RunDir: shortTempDir(t), POSVersion: "dev", DB: db})
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	s := waitState(t, m, "falla", StateRestarting)
	if s.Restarts < 1 || !strings.Contains(s.Error, "exit status 3") {
		t.Fatalf("debe registrar la caída: %+v", s)
	}
	cancel()
	m.Wait() // no se queda ciclado esperando para reiniciar
}

func TestDevPluginWritesEnv(t *testing.T) {
	db := testDB(t)
	dev := writePlugin(t, t.TempDir(), "mio", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := New(Config{Dir: filepath.Join(t.TempDir(), "no-existe"), DevDirs: []string{dev}, RunDir: shortTempDir(t),
		APIURL: "http://127.0.0.1:8080", POSVersion: "dev", DB: db})
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dev, ".opos-dev.env"))
	if err != nil {
		t.Fatal(err)
	}
	p, _ := m.Get("mio")
	env := string(b)
	for _, want := range []string{"OPOS_PLUGIN_ID=mio", "OPOS_TOKEN=" + p.Token(), "OPOS_API_URL=http://127.0.0.1:8080",
		"OPOS_DATA_DIR=" + filepath.Join(dev, ".opos-data")} {
		if !strings.Contains(env, want) {
			t.Errorf(".opos-dev.env sin %q:\n%s", want, env)
		}
	}
	if strings.Contains(env, "OPOS_SUPERVISED") {
		t.Error("en desarrollo el plugin no lo supervisa el POS")
	}
	if s := m.Status(ctx, p); s.State != StateDevWaiting || !s.Dev {
		t.Fatalf("sin correr, el de desarrollo espera: %+v", s)
	}
	if err := m.SetEnabled(ctx, "mio", false); err == nil {
		t.Fatal("uno en desarrollo no se desactiva desde el panel")
	}
}

// shortTempDir: los sockets Unix tienen un límite de ~104 caracteres y las
// carpetas temporales de las pruebas en macOS son largas.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "opos")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}
