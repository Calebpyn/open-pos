package printer

import (
	"context"
	"fmt"
	"log"
	"net"
	"strconv"
	"sync"
	"time"
)

const (
	ConnCUPS    = "CUPS"    // USB a través de una cola de CUPS
	ConnNetwork = "NETWORK" // TCP directo (normalmente puerto 9100)
)

// Target es la información mínima para mandar un trabajo a una impresora.
type Target struct {
	ConnectionType string
	SystemName     string // cola de CUPS
	IPAddress      string
	Port           int
}

func (t Target) describe() string {
	if t.ConnectionType == ConnCUPS {
		return t.SystemName
	}
	return net.JoinHostPort(t.IPAddress, strconv.Itoa(t.Port))
}

func (t Target) key() string {
	return t.ConnectionType + "|" + t.SystemName + "|" + t.IPAddress + "|" + strconv.Itoa(t.Port)
}

// --- Cola de trabajos por impresora ---
//
// Todo lo que el POS imprime pasa por aquí. Cada impresora tiene su propia
// cola atendida por una sola goroutine, así los trabajos salen uno por uno y
// en el orden en que llegaron (comanda, luego cajón, luego ticket), aunque
// lleguen a la vez desde varias terminales.

// drawerSettle es cuánto se espera después de abrir el cajón antes del
// siguiente trabajo: mientras dispara el pulso, las térmicas económicas
// ignoran lo que les llega (el ticket salía sin logo ni encabezado).
var drawerSettle = 1500 * time.Millisecond

// jobGap es la pausa mínima entre dos trabajos a la misma impresora; además
// se espera lo que tarda en imprimirse el anterior (ver pacing.go).
var jobGap = 400 * time.Millisecond

type job struct {
	queued time.Time
	ctx    context.Context
	target Target
	data   []byte
	drawer bool // pulso de apertura del cajón
	done   chan result
}

type result struct {
	id  string
	err error
}

type device struct{ jobs chan *job }

var devices sync.Map // Target.key() -> *device

func deviceFor(t Target) *device {
	if d, ok := devices.Load(t.key()); ok {
		return d.(*device)
	}
	d := &device{jobs: make(chan *job, 64)}
	if actual, loaded := devices.LoadOrStore(t.key(), d); loaded {
		return actual.(*device)
	}
	go d.run()
	return d
}

func (d *device) run() {
	var readyAt time.Time
	for j := range d.jobs {
		if wait := time.Until(readyAt); wait > 0 {
			select {
			case <-time.After(wait):
			case <-j.ctx.Done():
			}
		}
		kind := "ticket"
		if j.drawer {
			kind = "cajón"
		}
		rec := JobRecord{Queued: j.queued, Target: j.target.describe(), Kind: kind, Bytes: len(j.data), Head: hexHead(j.data, 48)}
		if err := j.ctx.Err(); err != nil {
			rec.Started, rec.Finished, rec.Err = time.Now(), time.Now(), "cancelado antes de enviarse: "+err.Error()
			record(rec)
			j.done <- result{err: err}
			continue
		}
		start := time.Now()
		id, err := transmit(j.ctx, j.target, j.data)
		rec.Started, rec.Finished, rec.CUPSID = start, time.Now(), id
		if err != nil {
			rec.Err = err.Error()
		}
		record(rec)
		readyAt = time.Now().Add(jobGap)
		if err == nil {
			if j.drawer {
				readyAt = time.Now().Add(drawerSettle)
			} else if Pacing {
				// La impresora empieza a imprimir en cuanto le llegan los
				// bytes; se espera a que termine de sacar el papel.
				if done := start.Add(PrintDuration(j.data)); done.After(readyAt) {
					readyAt = done
				}
			}
		}
		j.done <- result{id, err}
	}
}

func submit(ctx context.Context, t Target, data []byte, drawer bool) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	j := &job{queued: time.Now(), ctx: ctx, target: t, data: data, drawer: drawer, done: make(chan result, 1)}
	select {
	case deviceFor(t).jobs <- j:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	select {
	case r := <-j.done:
		return r.id, r.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// Send imprime un documento y espera a que salga. Para CUPS regresa el ID del
// trabajo.
func Send(ctx context.Context, t Target, data []byte) (string, error) {
	return submit(ctx, t, data, false)
}

// OpenDrawer manda el pulso que abre el cajón conectado a la impresora. El
// siguiente trabajo a esa impresora espera a que termine el pulso.
func OpenDrawer(ctx context.Context, t Target) error {
	start := time.Now()
	id, err := submit(ctx, t, DrawerKick(), true)
	log.Printf("Cajón en %s: %v (trabajo %q, error: %v)", t.describe(), time.Since(start).Round(time.Millisecond), id, err)
	return err
}

// transmit entrega los bytes a la impresora según su conexión.
func transmit(ctx context.Context, t Target, data []byte) (string, error) {
	switch t.ConnectionType {
	case ConnCUPS:
		return PrintCUPS(ctx, t.SystemName, data)
	case ConnNetwork:
		return "", sendTCP(ctx, t.IPAddress, t.Port, data)
	default:
		return "", fmt.Errorf("tipo de conexión desconocido: %q", t.ConnectionType)
	}
}

func sendTCP(ctx context.Context, host string, port int, data []byte) error {
	d := net.Dialer{Timeout: 3 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return fmt.Errorf("no se pudo conectar a %s:%d: %w", host, port, err)
	}
	defer conn.Close()
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write(data); err != nil {
		return fmt.Errorf("error enviando datos a %s:%d: %w", host, port, err)
	}
	return nil
}

// Reachable indica si una impresora de red acepta conexiones.
func Reachable(ctx context.Context, host string, port int) bool {
	d := net.Dialer{Timeout: 700 * time.Millisecond}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
