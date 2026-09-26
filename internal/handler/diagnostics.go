package handler

import (
	"context"
	"fmt"
	"net/http"
	osexec "os/exec"
	"strings"
	"sync"
	"time"

	"github.com/calebpyn/open-pos/internal/printer"
)

// LogTail guarda las últimas líneas del registro del servidor para el
// diagnóstico (main lo conecta a log.SetOutput).
var LogTail = &logRing{max: 400}

type logRing struct {
	sync.Mutex
	max   int
	lines []string
	part  string
}

func (l *logRing) Write(p []byte) (int, error) {
	l.Lock()
	defer l.Unlock()
	text := l.part + string(p)
	parts := strings.Split(text, "\n")
	l.part = parts[len(parts)-1]
	l.lines = append(l.lines, parts[:len(parts)-1]...)
	if len(l.lines) > l.max {
		l.lines = l.lines[len(l.lines)-l.max:]
	}
	return len(p), nil
}

func (l *logRing) String() string {
	l.Lock()
	defer l.Unlock()
	return strings.Join(l.lines, "\n")
}

// Version la fija main al arrancar.
var Version = "dev"

// GET /api/admin/diagnostics - Archivo de texto con lo necesario para
// entender un problema de impresión o del cajón: impresoras configuradas,
// colas de CUPS, últimos trabajos enviados y el registro del servidor.
func (h *POSHandler) AdminDiagnostics(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var b strings.Builder
	now := time.Now()
	fmt.Fprintf(&b, "Open POS %s — diagnóstico %s\n\n", Version, now.Format("2006-01-02 15:04:05"))

	b.WriteString("== Impresoras configuradas ==\n")
	if list, err := loadPrinters(ctx, h.DB); err != nil {
		fmt.Fprintf(&b, "error: %v\n", err)
	} else {
		for _, p := range list {
			fmt.Fprintf(&b, "#%d %q  %s %s %s:%d  papel %dmm  activa=%v  tickets=%v  cajón=%v  áreas=%v\n",
				p.ID, p.Name, p.ConnectionType, p.SystemName, p.IPAddress, p.Port, p.PaperWidth,
				p.IsActive, p.PrintsReceipts, p.HasDrawer, p.AreaIDs)
		}
	}

	if f, err := loadReceiptFormat(ctx, h.DB); err == nil {
		fmt.Fprintf(&b, "\n== Formato del ticket ==\nlogo=%v (%d%%)  nombre=%v  encabezado=%d líneas\n",
			f.ShowLogo, f.LogoWidthPct, f.ShowName, len(nonEmptyLines(f.HeaderLines)))
	}
	if logo := loadLogo(ctx, h.DB); logo != nil {
		fmt.Fprintf(&b, "logo guardado: %dx%d px\n", logo.Bounds().Dx(), logo.Bounds().Dy())
	}

	for _, cmd := range [][]string{{"lpstat", "-v"}, {"lpstat", "-p"}, {"lpstat", "-o"}} {
		fmt.Fprintf(&b, "\n== %s ==\n%s\n", strings.Join(cmd, " "), runQuick(ctx, cmd[0], cmd[1:]...))
	}

	b.WriteString("\n== Últimos trabajos enviados (hora, tipo, destino, tamaño, espera en cola, envío) ==\n")
	for _, j := range printer.Journal() {
		b.WriteString(j.String() + "\n")
	}

	b.WriteString("\n== Registro del servidor ==\n")
	b.WriteString(LogTail.String())
	b.WriteString("\n")

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="diagnostico-open-pos-`+now.Format("20060102-150405")+`.txt"`)
	w.Write([]byte(b.String()))
}

func runQuick(ctx context.Context, name string, args ...string) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := osexec.CommandContext(ctx, name, args...)
	cmd.Env = append(cmd.Environ(), "LANG=C", "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	if err != nil && len(out) == 0 {
		return "error: " + err.Error()
	}
	return strings.TrimSpace(string(out))
}
