package handler

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/calebpyn/open-pos/internal/database"
)

// fakePrinter es una impresora de red que guarda cada trabajo recibido.
type fakePrinter struct {
	ln   net.Listener
	mu   sync.Mutex
	jobs [][]byte
}

func newFakePrinter(t *testing.T) *fakePrinter {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fp := &fakePrinter{ln: ln}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			data, _ := io.ReadAll(conn)
			conn.Close()
			fp.mu.Lock()
			fp.jobs = append(fp.jobs, data)
			fp.mu.Unlock()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return fp
}

func (fp *fakePrinter) port() int { return fp.ln.Addr().(*net.TCPAddr).Port }

// jobCount espera a que lleguen want trabajos y los regresa.
func (fp *fakePrinter) jobCount(t *testing.T, want int) [][]byte {
	t.Helper()
	for i := 0; i < 800; i++ { // hasta 4 s: tras abrir el cajón la cola espera 1.5 s
		fp.mu.Lock()
		n := len(fp.jobs)
		fp.mu.Unlock()
		if n >= want {
			break
		}
		sleepMS(5)
	}
	fp.mu.Lock()
	defer fp.mu.Unlock()
	if len(fp.jobs) != want {
		t.Fatalf("se esperaban %d trabajos, llegaron %d", want, len(fp.jobs))
	}
	return append([][]byte(nil), fp.jobs...)
}

func setupComandaDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := database.InitDB(filepath.Join(t.TempDir(), "pos.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	// Las impresoras semilla se llaman como colas reales de CUPS: se pasan a
	// un puerto de red cerrado para que ninguna prueba imprima de verdad.
	exec(t, db, `UPDATE printers SET connection_type = 'NETWORK', ip_address = '127.0.0.1', port = 9, system_name = '127.0.0.1:9'`)
	return db
}

func exec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func TestPrintComandasRouting(t *testing.T) {
	db := setupComandaDB(t)
	ctx := context.Background()
	barra, cocina := newFakePrinter(t), newFakePrinter(t)

	// Datos semilla: categorías 1 Cafetería, 2 Cocina Caliente, 3 Postres;
	// productos 1 Espresso (1), 3 Chilaquiles (2), 4 Cheesecake (3).
	exec(t, db, `UPDATE printers SET connection_type = 'NETWORK', ip_address = '127.0.0.1', port = ?, system_name = ?, paper_width = 58 WHERE id = 1`,
		barra.port(), "127.0.0.1:"+strconv.Itoa(barra.port()))
	exec(t, db, `UPDATE printers SET connection_type = 'NETWORK', ip_address = '127.0.0.1', port = ?, system_name = ?, paper_width = 80 WHERE id = 2`,
		cocina.port(), "127.0.0.1:"+strconv.Itoa(cocina.port()))
	exec(t, db, `DELETE FROM printer_areas`)
	// Barra: Cafetería y Cocina Caliente (para expo). Cocina: Cocina Caliente. Postres: sin impresora.
	// Áreas creadas por la migración desde el ruteo anterior: 1 (Barra) y 2
	// (Cocina). Cafetería va a Barra, Cocina Caliente a Cocina y postres sin área.
	exec(t, db, `INSERT INTO printer_areas (printer_id, area_id) VALUES (1, 1), (1, 2), (2, 2)`)
	exec(t, db, `UPDATE products SET area_id = CASE category_id WHEN 1 THEN 1 WHEN 2 THEN 2 ELSE NULL END`)

	exec(t, db, `INSERT INTO table_sessions (id, table_id, status) VALUES (100, 1, 'OPEN')`)
	exec(t, db, `INSERT INTO orders (id, session_id, order_type, status) VALUES (100, 100, 'DINE_IN', 'ACTIVE')`)
	exec(t, db, `INSERT INTO order_items (id, order_id, product_id, unit_price, quantity, notes) VALUES
		(1, 100, 1, 45, 2, 'Extra caliente'), (2, 100, 3, 120, 1, ''), (3, 100, 4, 85, 1, '')`)

	out, err := printComandas(ctx, db, 100, false)
	if err != nil {
		t.Fatal(err)
	}
	// La barra recibe las áreas Barra y Cocina (una comanda por área), pero
	// el resultado no repite impresoras.
	if len(out.Failed) != 0 || len(out.Printed) != 2 {
		t.Fatalf("resultado inesperado: %+v", out)
	}

	bJobs, cJobs := barra.jobCount(t, 2), cocina.jobCount(t, 1)
	b, c := append(append([]byte{}, bJobs[0]...), bJobs[1]...), cJobs[0]
	for _, want := range []string{"COMANDA", "2 x Espresso Doble", "Extra caliente", "1 x Chilaquiles Verdes", "Mesa 1"} {
		if !bytes.Contains(b, []byte(want)) {
			t.Errorf("la barra debía recibir %q", want)
		}
	}
	if bytes.Contains(bJobs[0], []byte("Chilaquiles")) || !bytes.Contains(bJobs[1], []byte("COCINA")) {
		t.Error("cada área debe salir en su propia comanda, con el nombre del área")
	}
	if !bytes.Contains(c, []byte("1 x Chilaquiles Verdes")) || bytes.Contains(c, []byte("Espresso")) {
		t.Error("cocina solo debe recibir los chilaquiles")
	}
	if bytes.Contains(b, []byte("Cheesecake")) || bytes.Contains(c, []byte("Cheesecake")) {
		t.Error("postres no tiene impresora asignada y no debe imprimirse")
	}

	// Todo quedó como enviado, incluido el postre sin impresora.
	var pending int
	db.QueryRow(`SELECT COUNT(*) FROM order_items WHERE order_id = 100 AND printed_quantity < quantity`).Scan(&pending)
	if pending != 0 {
		t.Fatalf("quedaron %d productos pendientes", pending)
	}

	// Un adicional imprime solo lo nuevo y se marca como tal.
	exec(t, db, `INSERT INTO order_items (id, order_id, product_id, unit_price, quantity) VALUES (4, 100, 1, 45, 1)`)
	if _, err := printComandas(ctx, db, 100, false); err != nil {
		t.Fatal(err)
	}
	add := barra.jobCount(t, 3)[2]
	if !bytes.Contains(add, []byte("ADICIONAL")) || !bytes.Contains(add, []byte("1 x Espresso Doble")) || bytes.Contains(add, []byte("Chilaquiles")) {
		t.Errorf("el adicional debe traer solo el espresso nuevo: %q", add)
	}
	cocina.jobCount(t, 1) // cocina no recibe nada nuevo

	// Si una impresora falla, sus productos quedan pendientes para reintentar.
	cocina.ln.Close()
	exec(t, db, `INSERT INTO order_items (id, order_id, product_id, unit_price, quantity) VALUES (5, 100, 3, 120, 1)`)
	out, err = printComandas(ctx, db, 100, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Failed) != 1 {
		t.Fatalf("se esperaba una impresora fallida: %+v", out)
	}
	var printed int
	db.QueryRow(`SELECT printed_quantity FROM order_items WHERE id = 5`).Scan(&printed)
	if printed != 0 {
		t.Error("el producto de la impresora caída debe quedar pendiente")
	}

	// La reimpresión manda la orden completa sin tocar lo pendiente.
	if _, err := printComandas(ctx, db, 100, true); err != nil {
		t.Fatal(err)
	}
	// Barra: comanda (2 áreas), adicional, chilaquiles 5 y la reimpresión (2 áreas).
	jobs := barra.jobCount(t, 6)
	re := jobs[4]
	if !bytes.Contains(re, []byte("REIMPRESI")) || !bytes.Contains(re, []byte("2 x Espresso Doble")) || !bytes.Contains(re, []byte("1 x Espresso Doble")) {
		t.Errorf("la reimpresión debe traer la orden completa: %q", re)
	}
	db.QueryRow(`SELECT printed_quantity FROM order_items WHERE id = 5`).Scan(&printed)
	if printed != 0 {
		t.Error("la reimpresión no debe marcar pendientes como enviados")
	}
}

func sleepMS(ms int) { time.Sleep(time.Duration(ms) * time.Millisecond) }
