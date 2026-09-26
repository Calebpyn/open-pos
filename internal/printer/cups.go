package printer

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// macOS ya no permite colas "raw", así que las colas se crean con el driver
// PostScript genérico y los trabajos se envían con "-o raw", que salta los
// filtros y entrega los bytes ESC/POS tal cual al backend USB.
const genericPPD = "drv:///sample.drv/generic.ppd"

// Device es un dispositivo físico detectado por CUPS.
type Device struct {
	URI          string `json:"uri"`
	Info         string `json:"info"`
	MakeAndModel string `json:"make_and_model"`
}

// Queue es una cola de impresión configurada en CUPS.
type Queue struct {
	Name    string `json:"name"`
	URI     string `json:"uri"`
	State   string `json:"state"` // idle, printing, disabled
	Enabled bool   `json:"enabled"`
	Reason  string `json:"reason"`  // mensaje de CUPS, p. ej. "Unable to send data to printer."
	Pending int    `json:"pending"` // trabajos sin terminar
}

// errorPolicy hace que una falla del USB no pause la cola: CUPS reintenta el
// trabajo en lugar de detener la impresora. Con la política por omisión
// (stop-printer) la cola queda pausada hasta que alguien la reanude a mano.
const errorPolicy = "printer-error-policy=retry-job"

var queueNameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,127}$`)

// ValidQueueName indica si el nombre es aceptable como cola de CUPS.
func ValidQueueName(name string) bool { return queueNameRe.MatchString(name) }

// ValidDeviceURI acepta solo URIs de dispositivo USB (las de red se manejan
// por TCP directo, sin CUPS). No se usa url.Parse porque CUPS escapa los
// espacios del fabricante en el host (usb://Thermal%20Printer/...), algo que
// Go rechaza. Es seguro: los comandos reciben argumentos, nunca pasan por un shell.
func ValidDeviceURI(uri string) bool {
	rest, ok := strings.CutPrefix(uri, "usb://")
	if !ok || rest == "" || len(uri) > 1024 {
		return false
	}
	for _, r := range uri {
		if r < 0x21 || r == 0x7f {
			return false
		}
	}
	return true
}

func run(ctx context.Context, stdin []byte, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	// Salida en inglés para poder interpretarla sin importar el idioma del sistema.
	cmd.Env = append(cmd.Environ(), "LANG=C", "LC_ALL=C")
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		// Los comandos de CUPS ya anteponen su nombre ("lp: ...").
		if !strings.HasPrefix(msg, name+":") {
			msg = name + ": " + msg
		}
		return stdout.String(), errors.New(msg)
	}
	return stdout.String(), nil
}

// DetectUSB lista las impresoras USB conectadas que CUPS puede ver.
func DetectUSB(ctx context.Context) ([]Device, error) {
	// El backend USB de CUPS tarda ~13 s en sondear los puertos, así que la
	// pantalla lo pide en segundo plano.
	out, err := run(ctx, nil, "lpinfo", "-l", "-v", "--include-schemes", "usb")
	if err != nil {
		return nil, err
	}
	var devices []Device
	var cur *Device
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if uri, ok := strings.CutPrefix(line, "Device: uri = "); ok {
			if cur != nil && ValidDeviceURI(cur.URI) {
				devices = append(devices, *cur)
			}
			cur = &Device{URI: uri}
			continue
		}
		if cur == nil {
			continue
		}
		if v, ok := strings.CutPrefix(line, "info = "); ok {
			cur.Info = v
		} else if v, ok := strings.CutPrefix(line, "make-and-model = "); ok {
			cur.MakeAndModel = v
		}
	}
	if cur != nil && ValidDeviceURI(cur.URI) {
		devices = append(devices, *cur)
	}
	return devices, nil
}

// ListQueues regresa las colas de CUPS indexadas por nombre.
func ListQueues(ctx context.Context) (map[string]Queue, error) {
	devices, err := run(ctx, nil, "lpstat", "-v")
	if err != nil {
		// Sin colas configuradas lpstat termina con error; no es una falla.
		if strings.Contains(err.Error(), "No destinations") {
			return map[string]Queue{}, nil
		}
		return nil, err
	}
	status, _ := run(ctx, nil, "lpstat", "-p")
	jobs, _ := run(ctx, nil, "lpstat", "-o")
	return parseQueues(devices, status, jobs), nil
}

// parseQueues combina la salida de "lpstat -v" (dispositivos), "lpstat -p"
// (estado y motivo) y "lpstat -o" (trabajos sin terminar).
func parseQueues(devices, status, jobs string) map[string]Queue {
	queues := map[string]Queue{}
	sc := bufio.NewScanner(strings.NewReader(devices))
	for sc.Scan() {
		rest, ok := strings.CutPrefix(sc.Text(), "device for ")
		if !ok {
			continue
		}
		name, uri, ok := strings.Cut(rest, ": ")
		if !ok {
			continue
		}
		queues[name] = Queue{Name: name, URI: strings.TrimSpace(uri), State: "idle", Enabled: true}
	}

	// "printer COLA disabled since ... -" seguido de una línea con tabulador
	// que explica el motivo ("Unable to send data to printer.").
	last := ""
	sc = bufio.NewScanner(strings.NewReader(status))
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "\t") || strings.HasPrefix(line, " ") {
			if q, ok := queues[last]; ok && q.Reason == "" {
				q.Reason = strings.TrimSpace(line)
				queues[last] = q
			}
			continue
		}
		rest, ok := strings.CutPrefix(line, "printer ")
		last = ""
		if !ok {
			continue
		}
		name, st, _ := strings.Cut(rest, " ")
		q, ok := queues[name]
		if !ok {
			continue
		}
		last = name
		switch {
		case strings.Contains(st, "disabled"):
			q.State, q.Enabled = "disabled", false
		case strings.Contains(st, "now printing"):
			q.State, q.Enabled = "printing", true
		default:
			q.State, q.Enabled = "idle", true
		}
		queues[name] = q
	}

	// "COLA-38   usuario  1024   fecha"
	sc = bufio.NewScanner(strings.NewReader(jobs))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 0 {
			continue
		}
		i := strings.LastIndex(f[0], "-")
		if i <= 0 {
			continue
		}
		if q, ok := queues[f[0][:i]]; ok {
			q.Pending++
			queues[f[0][:i]] = q
		}
	}
	return queues
}

// EnsureQueue crea (o actualiza) una cola de CUPS apuntando al dispositivo.
func EnsureQueue(ctx context.Context, name, deviceURI, description string) error {
	if !ValidQueueName(name) {
		return fmt.Errorf("nombre de cola inválido: %q", name)
	}
	if !ValidDeviceURI(deviceURI) {
		return fmt.Errorf("dispositivo inválido: %q", deviceURI)
	}
	// lpadmin avisa por stderr que los drivers están obsoletos, pero termina bien.
	_, err := run(ctx, nil, "lpadmin",
		"-p", name, "-E",
		"-v", deviceURI,
		"-m", genericPPD,
		"-D", description,
		"-o", "printer-is-shared=false",
		"-o", errorPolicy)
	return err
}

// FixErrorPolicy aplica la política de reintento a una cola ya existente
// (las creadas antes no la tenían).
func FixErrorPolicy(ctx context.Context, name string) error {
	if !ValidQueueName(name) {
		return fmt.Errorf("nombre de cola inválido: %q", name)
	}
	_, err := run(ctx, nil, "lpadmin", "-p", name, "-o", errorPolicy)
	return err
}

// Resume reanuda una cola pausada y hace que vuelva a aceptar trabajos.
func Resume(ctx context.Context, name string) error {
	if !ValidQueueName(name) {
		return fmt.Errorf("nombre de cola inválido: %q", name)
	}
	if _, err := run(ctx, nil, "cupsenable", name); err != nil {
		return err
	}
	_, err := run(ctx, nil, "cupsaccept", name)
	return err
}

// CancelJobs descarta todos los trabajos pendientes de una cola.
func CancelJobs(ctx context.Context, name string) error {
	if !ValidQueueName(name) {
		return fmt.Errorf("nombre de cola inválido: %q", name)
	}
	_, err := run(ctx, nil, "cancel", "-a", name)
	return err
}

// RemoveQueue elimina una cola de CUPS. No falla si no existe.
func RemoveQueue(ctx context.Context, name string) error {
	if !ValidQueueName(name) {
		return fmt.Errorf("nombre de cola inválido: %q", name)
	}
	_, err := run(ctx, nil, "lpadmin", "-x", name)
	if err != nil && strings.Contains(err.Error(), "does not exist") {
		return nil
	}
	return err
}

// Tiempos de espera de un trabajo. Si la impresora no empieza (o reporta un
// error) en printWait, el trabajo se da por fallido. Mientras esté
// imprimiendo sin errores se le da hasta printMax: un ticket con logo puede
// tardar más de 10 segundos, y cortarlo a medias hace que la impresora
// imprima el resto de la imagen como texto basura.
var (
	printWait = 8 * time.Second
	printMax  = 90 * time.Second
	pollEvery = 300 * time.Millisecond
)

// PrintCUPS envía bytes ESC/POS a una cola de CUPS y espera a que se
// imprima. Regresa el ID del trabajo, o un error si no salió: "lp" acepta el
// trabajo aunque la impresora no responda, así que aceptarlo no basta.
func PrintCUPS(ctx context.Context, queue string, data []byte) (string, error) {
	if !ValidQueueName(queue) {
		return "", fmt.Errorf("nombre de cola inválido: %q", queue)
	}
	if err := resumeIfPaused(ctx, queue); err != nil {
		return "", err
	}

	out, err := run(ctx, data, "lp", "-d", queue, "-o", "raw", "-t", "Open POS")
	if err != nil {
		// lp reporta una cola inexistente como "No such file or directory".
		if strings.Contains(err.Error(), "No such file or directory") || strings.Contains(err.Error(), "does not exist") {
			return "", fmt.Errorf("la cola %q no existe en CUPS; revisa la impresora en Dispositivos", queue)
		}
		return "", err
	}
	// "request id is COLA-12 (1 file(s))"
	rest, ok := strings.CutPrefix(strings.TrimSpace(out), "request id is ")
	if !ok {
		return "", errors.New("CUPS no regresó un ID de trabajo")
	}
	id, _, _ := strings.Cut(rest, " ")
	return id, waitJob(ctx, queue, id)
}

// waitJob espera a que el trabajo deje la cola. Si no sale se cancela: es
// mejor avisar que falló (y reimprimir desde el POS) que dejar que salga
// minutos después, cuando alguien ya lo reimprimió.
func waitJob(ctx context.Context, queue, id string) error {
	start := time.Now()
	var q Queue
	var stalledSince time.Time // desde cuándo la impresora no está trabajando
	for {
		jobs, err := run(ctx, nil, "lpstat", "-o", queue)
		if err == nil && !hasJob(jobs, id) {
			return nil
		}
		if st, err := run(ctx, nil, "lpstat", "-p", queue); err == nil {
			q = parseQueues("device for "+queue+": x\n", st, "")[queue]
		}
		var stalled time.Duration
		if q.State == "printing" && !errorReason(q.Reason) {
			stalledSince = time.Time{}
		} else {
			if stalledSince.IsZero() {
				stalledSince = time.Now()
			}
			stalled = time.Since(stalledSince)
		}
		if giveUp(q, time.Since(start), stalled) || ctx.Err() != nil {
			break
		}
		time.Sleep(pollEvery)
	}
	// Pudo terminar entre la última revisión y ahora: no se reporta como
	// fallido algo que sí salió.
	if jobs, err := run(context.Background(), nil, "lpstat", "-o", queue); err == nil && !hasJob(jobs, id) {
		return nil
	}
	run(context.Background(), nil, "cancel", id)
	reason := q.Reason
	if reason == "" {
		reason = "no respondió"
	}
	return fmt.Errorf("la impresora no imprimió (%s). Revisa que esté encendida, con papel y conectada; el trabajo se canceló para que no salga tarde", reason)
}

// stallGrace es cuánto debe seguir la impresora sin trabajar para darse por
// atorada: entre un trabajo y el siguiente queda inactiva un instante.
const stallGrace = 2 * time.Second

// giveUp decide si un trabajo que sigue en la cola ya se da por fallido.
// stalled es cuánto lleva la impresora seguida sin imprimir (o con error).
func giveUp(q Queue, elapsed, stalled time.Duration) bool {
	switch {
	case !q.Enabled && q.Name != "":
		return true // CUPS pausó la cola
	case elapsed > printMax:
		return true
	case elapsed > printWait:
		return stalled >= stallGrace
	}
	return false
}

// errorReason reconoce los mensajes de CUPS que indican que la impresora no
// está recibiendo datos (desconectada, apagada, sin respuesta).
func errorReason(reason string) bool {
	r := strings.ToLower(reason)
	for _, s := range []string{"unable", "not connected", "offline", "not responding", "waiting for printer", "error", "failed"} {
		if strings.Contains(r, s) {
			return true
		}
	}
	return false
}

func hasJob(lpstatOut, id string) bool {
	sc := bufio.NewScanner(strings.NewReader(lpstatOut))
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) > 0 && f[0] == id {
			return true
		}
	}
	return false
}

// resumeIfPaused reanuda una cola que CUPS pausó tras una falla. Lo que se
// quedó atorado en ella se descarta primero: son comandas o tickets viejos
// que el POS ya reportó como fallidos.
func resumeIfPaused(ctx context.Context, queue string) error {
	st, err := run(ctx, nil, "lpstat", "-p", queue)
	if err != nil {
		return nil // la cola no existe o CUPS no responde: lp dará el error claro
	}
	q := parseQueues("device for "+queue+": x\n", st, "")[queue]
	if q.Enabled {
		return nil
	}
	CancelJobs(ctx, queue)
	if err := Resume(ctx, queue); err != nil {
		return fmt.Errorf("la cola %q está pausada en CUPS y no se pudo reanudar: %w", queue, err)
	}
	return nil
}
