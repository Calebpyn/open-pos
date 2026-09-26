package printer

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// --- Bitácora de trabajos ---
//
// Los últimos trabajos enviados a cada impresora, para el diagnóstico: cuándo
// se pidieron, cuánto esperaron en la cola, cuánto tardó CUPS y con qué bytes
// empezaban. Solo vive en memoria.

type JobRecord struct {
	Queued   time.Time
	Started  time.Time
	Finished time.Time
	Target   string
	Kind     string // ticket | cajón
	Bytes    int
	Head     string // primeros bytes en hexadecimal
	CUPSID   string
	Err      string
}

const journalSize = 60

var journal struct {
	sync.Mutex
	jobs []JobRecord
}

func record(r JobRecord) {
	journal.Lock()
	defer journal.Unlock()
	journal.jobs = append(journal.jobs, r)
	if len(journal.jobs) > journalSize {
		journal.jobs = journal.jobs[len(journal.jobs)-journalSize:]
	}
}

// Journal regresa los trabajos recientes, del más viejo al más nuevo.
func Journal() []JobRecord {
	journal.Lock()
	defer journal.Unlock()
	return append([]JobRecord(nil), journal.jobs...)
}

func hexHead(data []byte, n int) string {
	var b strings.Builder
	for i, c := range data {
		if i == n {
			b.WriteString("…")
			break
		}
		fmt.Fprintf(&b, "%02x ", c)
	}
	return strings.TrimSpace(b.String())
}

func (r JobRecord) String() string {
	ms := func(d time.Duration) string { return fmt.Sprintf("%dms", d.Milliseconds()) }
	s := fmt.Sprintf("%s  %-6s %-22s %6d bytes  en cola %s  envío %s",
		r.Queued.Format("15:04:05.000"), r.Kind, r.Target, r.Bytes,
		ms(r.Started.Sub(r.Queued)), ms(r.Finished.Sub(r.Started)))
	if r.CUPSID != "" {
		s += "  " + r.CUPSID
	}
	if r.Err != "" {
		s += "  ERROR: " + r.Err
	}
	return s + "\n      " + r.Head
}
