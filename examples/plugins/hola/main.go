// Hola mundo: el plugin más simple que prueba todo el recorrido.
//
//   - Lee los eventos del POS (cuentas cobradas) y guarda sus números en su
//     propia SQLite, con el cursor en la misma transacción.
//   - Muestra una página dentro del panel de administración.
//   - Lee el catálogo y marca productos agotados/disponibles.
//   - Respalda su base cuando el POS hace su respaldo diario.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	opos "github.com/calebpyn/open-pos/sdk"
	_ "github.com/glebarez/go-sqlite"
)

func main() {
	log.SetFlags(0) // el POS agrega la hora y el prefijo [plugin hola]
	p, err := opos.New()
	if err != nil {
		log.Fatal(err)
	}
	db, err := openDB(filepath.Join(p.DataDir, "hola.db"))
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	app := &app{p: p, db: db}
	p.HandleFunc("GET /{$}", app.home)
	p.HandleFunc("POST /productos/{id}/disponible", app.setAvailability)
	p.OnBackup(func(ctx context.Context, dir string) error {
		_, err := db.ExecContext(ctx, `VACUUM INTO ?`, filepath.Join(dir, "hola.db"))
		return err
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Consume(ctx, app.cursor, app.apply)
	log.Printf("listo (datos en %s)", p.DataDir)
	if err := p.Run(ctx); err != nil {
		log.Fatal(err)
	}
}

func openDB(path string) (*sql.DB, error) {
	params := url.Values{}
	params.Add("_pragma", "journal_mode(WAL)")
	params.Add("_pragma", "busy_timeout(5000)")
	params.Set("_txlock", "immediate")
	db, err := sql.Open("sqlite", path+"?"+params.Encode())
	if err != nil {
		return nil, err
	}
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS cursor (id INTEGER PRIMARY KEY CHECK (id = 1), seq INTEGER NOT NULL);
		INSERT OR IGNORE INTO cursor (id, seq) VALUES (1, 0);
		CREATE TABLE IF NOT EXISTS totals (id INTEGER PRIMARY KEY CHECK (id = 1), bills INTEGER NOT NULL, cents INTEGER NOT NULL);
		INSERT OR IGNORE INTO totals (id, bills, cents) VALUES (1, 0, 0);
		CREATE TABLE IF NOT EXISTS products (product_id INTEGER PRIMARY KEY, name TEXT NOT NULL, sold INTEGER NOT NULL);
		CREATE TABLE IF NOT EXISTS recent (seq INTEGER PRIMARY KEY, type TEXT NOT NULL, summary TEXT NOT NULL, at DATETIME NOT NULL);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

type app struct {
	p  *opos.Plugin
	db *sql.DB
}

func (a *app) cursor(ctx context.Context) (int64, error) {
	var seq int64
	err := a.db.QueryRowContext(ctx, `SELECT seq FROM cursor`).Scan(&seq)
	return seq, err
}

// apply procesa un lote: los números, la lista de recientes y el cursor
// quedan en la misma transacción.
func (a *app) apply(ctx context.Context, events []opos.Event) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, e := range events {
		summary := e.Type
		switch e.Type {
		case opos.EventBillPaid:
			var bill opos.BillPaid
			if err := e.Decode(&bill); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE totals SET bills = bills + 1, cents = cents + ?`,
				int64(bill.Total*100+0.5)); err != nil {
				return err
			}
			for _, it := range bill.Items {
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO products (product_id, name, sold) VALUES (?, ?, ?)
					ON CONFLICT (product_id) DO UPDATE SET name = excluded.name, sold = sold + excluded.sold`,
					it.ProductID, it.Product, it.Quantity); err != nil {
					return err
				}
			}
			summary = fmt.Sprintf("Cuenta cobrada: $%.2f (%d productos)", bill.Total, len(bill.Items))
		case opos.EventItemsAdded:
			var added opos.ItemsAdded
			if err := e.Decode(&added); err != nil {
				return err
			}
			var names []string
			for _, it := range added.Items {
				names = append(names, fmt.Sprintf("%d× %s", it.Quantity, it.Product))
			}
			summary = "Comanda: " + strings.Join(names, ", ")
		case opos.EventOrderClosed:
			var closed opos.OrderClosed
			if err := e.Decode(&closed); err != nil {
				return err
			}
			summary = fmt.Sprintf("Orden %d cerrada (%s)", closed.OrderID, closed.Status)
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO recent (seq, type, summary, at) VALUES (?, ?, ?, ?)`,
			e.Seq, e.Type, summary, e.CreatedAt); err != nil {
			return err
		}
	}
	last := events[len(events)-1].Seq
	if _, err := tx.ExecContext(ctx, `DELETE FROM recent WHERE seq <= ?`, last-30); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE cursor SET seq = ?`, last); err != nil {
		return err
	}
	return tx.Commit()
}

type productRow struct {
	ID        int64
	Name      string
	Sold      int
	Available bool
	Known     bool // sigue en el catálogo
}

type recentRow struct {
	Seq     int64
	Type    string
	Summary string
}

// home es la portada del plugin dentro del panel.
func (a *app) home(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := struct {
		User     opos.User
		Base     string
		Bills    int
		Total    float64
		Seq      int64
		Products []productRow
		Recent   []recentRow
		Err      string
	}{User: opos.UserFrom(r), Base: opos.BasePath(r)}

	var cents int64
	if err := a.db.QueryRowContext(ctx, `SELECT t.bills, t.cents, c.seq FROM totals t, cursor c`).
		Scan(&data.Bills, &cents, &data.Seq); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data.Total = float64(cents) / 100

	available := map[int64]bool{}
	if cat, err := a.p.API.Catalog(ctx); err != nil {
		data.Err = "No se pudo leer el catálogo: " + err.Error()
	} else {
		for _, p := range cat.Products {
			available[p.ID] = p.IsAvailable
		}
	}
	rows, err := a.db.QueryContext(ctx, `SELECT product_id, name, sold FROM products ORDER BY sold DESC, name LIMIT 10`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for rows.Next() {
		var p productRow
		rows.Scan(&p.ID, &p.Name, &p.Sold)
		p.Available, p.Known = available[p.ID]
		data.Products = append(data.Products, p)
	}
	rows.Close()
	rows, err = a.db.QueryContext(ctx, `SELECT seq, type, summary FROM recent ORDER BY seq DESC LIMIT 15`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for rows.Next() {
		var e recentRow
		rows.Scan(&e.Seq, &e.Type, &e.Summary)
		data.Recent = append(data.Recent, e)
	}
	rows.Close()

	var out strings.Builder
	if err := homeTmpl.Execute(&out, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	opos.Page(w, "Hola mundo", out.String())
}

// setAvailability marca un producto agotado o disponible en el POS.
func (a *app) setAvailability(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "id inválido", http.StatusBadRequest)
		return
	}
	available := r.FormValue("available") == "1"
	reason := "Desde el plugin Hola mundo por " + opos.UserFrom(r).Name
	if err := a.p.API.SetAvailability(r.Context(), id, available, reason); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, opos.BasePath(r)+"/", http.StatusSeeOther)
}

// Las clases son las de Tailwind que ya trae app.css del POS.
var homeTmpl = template.Must(template.New("home").Funcs(template.FuncMap{
	"money": func(v float64) string { return fmt.Sprintf("$%.2f", v) },
}).Parse(`
<div class="space-y-4">
  <section class="bg-white rounded-2xl border border-gray-200 shadow-sm p-6">
    <h2 class="text-lg font-black text-gray-900">👋 Hola, {{.User.Name}}</h2>
    <p class="mt-1 text-xs text-gray-500">
      Este plugin corre aparte del POS, guarda sus datos en su propia base y se entera de las ventas por la bitácora de eventos.
      Va en el evento <span class="font-mono">#{{.Seq}}</span>.
    </p>
    {{with .Err}}<p class="mt-2 text-xs font-semibold text-rose-600">{{.}}</p>{{end}}
    <dl class="mt-4 grid grid-cols-2 gap-2">
      <div><dt class="text-xs text-gray-400">Cuentas cobradas</dt><dd class="text-lg font-black font-mono">{{.Bills}}</dd></div>
      <div><dt class="text-xs text-gray-400">Total cobrado</dt><dd class="text-lg font-black font-mono">{{money .Total}}</dd></div>
    </dl>
  </section>

  <section class="bg-white rounded-2xl border border-gray-200 shadow-sm p-6">
    <h2 class="text-lg font-black text-gray-900">Lo más vendido</h2>
    {{if not .Products}}<p class="mt-1 text-xs text-gray-500">Todavía no hay cuentas cobradas.</p>{{end}}
    <div class="mt-2 space-y-2">
      {{range .Products}}
      <div class="flex items-center justify-between gap-3 text-sm">
        <span><span class="font-mono font-bold">{{.Sold}}×</span> {{.Name}}</span>
        {{if .Known}}
        <form method="post" action="{{$.Base}}/productos/{{.ID}}/disponible">
          {{if .Available}}
          <input type="hidden" name="available" value="0" />
          <button class="px-3 py-2 rounded-xl text-xs font-bold bg-gray-100 hover:bg-gray-200">Marcar agotado</button>
          {{else}}
          <input type="hidden" name="available" value="1" />
          <button class="px-3 py-2 rounded-xl text-xs font-bold bg-amber-100 text-amber-800">Agotado · reactivar</button>
          {{end}}
        </form>
        {{end}}
      </div>
      {{end}}
    </div>
  </section>

  <section class="bg-white rounded-2xl border border-gray-200 shadow-sm p-6">
    <h2 class="text-lg font-black text-gray-900">Últimos eventos</h2>
    <div class="mt-2 space-y-1">
      {{range .Recent}}
      <p class="text-xs"><span class="font-mono text-gray-400">#{{.Seq}} {{.Type}}</span> {{.Summary}}</p>
      {{else}}
      <p class="text-xs text-gray-500">Sin eventos todavía. Manda una comanda desde el POS.</p>
      {{end}}
    </div>
  </section>
</div>`))
