// Package plugins instala y supervisa plugins: programas aparte (p. ej. un
// inventario) que corren junto a Open POS sin tocar su código ni su base.
//
// Cada plugin es un ejecutable en su propia carpeta con un plugin.json. El POS
// lo arranca como proceso hijo y le habla por un socket Unix; el plugin se
// entera de lo que pasa en el POS leyendo la bitácora de eventos
// (plugin_events) y usa la API /api/plugin/v1 con su token. Nunca escribe en
// pos.db: guarda lo suyo en su propia carpeta de datos.
package plugins

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"
	"time"
)

// APIVersion es la versión del contrato con los plugins (plugin.json,
// eventos y /api/plugin/v1). Cambia solo si se rompe la compatibilidad.
const APIVersion = 1

// Event es un registro de la bitácora para plugins.
type Event struct {
	Seq       int64           `json:"seq"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"created_at"`
}

// Emit registra un evento dentro de la transacción de la operación que lo
// causa: si la operación se deshace, el evento también.
func Emit(ctx context.Context, tx *sql.Tx, typ string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO plugin_events (type, payload) VALUES (?, ?)`, typ, string(b)); err != nil {
		return err
	}
	news.notify()
	return nil
}

// news despierta a los plugins que esperan eventos. Cada aviso cierra el
// canal actual y crea otro, así cualquier número de lectores se entera.
var news = &notifier{ch: make(chan struct{})}

type notifier struct {
	mu sync.Mutex
	ch chan struct{}
}

func (n *notifier) wait() <-chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.ch
}

func (n *notifier) notify() {
	n.mu.Lock()
	close(n.ch)
	n.ch = make(chan struct{})
	n.mu.Unlock()
}

// ReadEvents regresa hasta limit eventos posteriores a after. Si no hay y
// wait > 0, espera hasta que llegue alguno o se cumpla el tiempo.
func ReadEvents(ctx context.Context, db *sql.DB, after int64, limit int, wait time.Duration) ([]Event, error) {
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		woke := news.wait() // antes de consultar, para no perder un aviso
		events, err := queryEvents(ctx, db, after, limit)
		if err != nil || len(events) > 0 || wait <= 0 {
			return events, err
		}
		select {
		case <-ctx.Done():
			return events, nil
		case <-deadline.C:
			return events, nil
		case <-woke:
			// El aviso llega antes del COMMIT de la operación. Como las
			// transacciones toman el candado de escritura al iniciar
			// (_txlock=immediate), abrir una aquí espera a que la otra
			// termine; después el evento ya se ve (o se deshizo).
			if tx, err := db.BeginTx(ctx, nil); err == nil {
				tx.Rollback()
			}
		}
	}
}

func queryEvents(ctx context.Context, db *sql.DB, after int64, limit int) ([]Event, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT seq, type, payload, created_at FROM plugin_events
		WHERE seq > ? ORDER BY seq LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []Event{}
	for rows.Next() {
		var e Event
		var payload string
		if err := rows.Scan(&e.Seq, &e.Type, &payload, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.Payload = json.RawMessage(payload)
		events = append(events, e)
	}
	return events, rows.Err()
}

// LatestSeq es el último evento registrado (0 si no hay). Un plugin recién
// instalado lo usa si quiere empezar "desde ahora" y no desde el historial.
func LatestSeq(ctx context.Context, db *sql.DB) (int64, error) {
	var seq int64
	err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM plugin_events`).Scan(&seq)
	return seq, err
}

// Ack guarda hasta dónde leyó un plugin. Pedir eventos después de after
// confirma que ya procesó los anteriores.
func Ack(ctx context.Context, db *sql.DB, id string, after int64) error {
	_, err := db.ExecContext(ctx, `
		UPDATE plugins SET cursor = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND cursor < ?`, after, id, after)
	return err
}

// Cuánto se guardan los eventos: los que ya leyeron todos los plugins
// activos (o todos, si no hay plugins) se borran a los 30 días, para que un
// plugin que se instala después pueda reconstruir el último mes. Nada pasa de
// 90 días aunque un plugin desactivado no lo haya leído.
const (
	keepRead = "-30 days"
	keepMax  = "-90 days"
)

// Prune borra los eventos viejos.
func Prune(ctx context.Context, db *sql.DB) (int64, error) {
	res, err := db.ExecContext(ctx, `
		DELETE FROM plugin_events
		WHERE created_at < datetime('now', ?)
		   OR (created_at < datetime('now', ?)
		       AND seq <= COALESCE((SELECT MIN(cursor) FROM plugins WHERE enabled = 1), 9223372036854775807))`,
		keepMax, keepRead)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
