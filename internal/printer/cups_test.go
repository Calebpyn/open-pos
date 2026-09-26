package printer

import (
	"testing"
	"time"
)

func TestParseQueues(t *testing.T) {
	devices := "device for EPSON_TM_BARRA: usb://Thermal%20Printer/H58%20Printer%20USB?serial=Printer\n" +
		"device for COCINA: usb://Otra/Impresora\n"
	status := "printer EPSON_TM_BARRA disabled since Wed Sep 23 19:46:49 2026 -\n" +
		"\tUnable to send data to printer.\n" +
		"printer COCINA is idle.  enabled since Wed Sep 23 10:00:00 2026\n"
	jobs := "EPSON_TM_BARRA-38       calebpayan        1024   Wed Sep 23 19:39:43 2026\n" +
		"EPSON_TM_BARRA-39       calebpayan        1024   Wed Sep 23 19:39:59 2026\n"

	q := parseQueues(devices, status, jobs)
	b := q["EPSON_TM_BARRA"]
	if b.Enabled || b.State != "disabled" || b.Reason != "Unable to send data to printer." || b.Pending != 2 {
		t.Fatalf("barra: %+v", b)
	}
	if b.URI != "usb://Thermal%20Printer/H58%20Printer%20USB?serial=Printer" {
		t.Fatalf("uri: %q", b.URI)
	}
	c := q["COCINA"]
	if !c.Enabled || c.State != "idle" || c.Reason != "" || c.Pending != 0 {
		t.Fatalf("cocina: %+v", c)
	}
}

func TestHasJob(t *testing.T) {
	out := "EPSON_TM_BARRA-38   calebpayan   1024   Wed Sep 23 19:39:43 2026\n"
	if !hasJob(out, "EPSON_TM_BARRA-38") || hasJob(out, "EPSON_TM_BARRA-3") || hasJob("", "EPSON_TM_BARRA-38") {
		t.Fatal("hasJob debe comparar el ID completo")
	}
}

func TestGiveUp(t *testing.T) {
	ok := Queue{Name: "B", Enabled: true, State: "printing"}
	paused := Queue{Name: "B", Enabled: false, State: "disabled"}
	s := time.Second
	for _, c := range []struct {
		q                Queue
		elapsed, stalled time.Duration
		want             bool
	}{
		{ok, 5 * s, 0, false},
		{ok, 30 * s, 0, false},                     // un ticket con logo largo sigue imprimiendo
		{ok, 91 * s, 0, true},                      // límite absoluto
		{ok, 9 * s, 500 * time.Millisecond, false}, // inactiva un instante entre trabajos
		{ok, 12 * s, 3 * s, true},                  // lleva rato sin imprimir: atorada
		{ok, 5 * s, 5 * s, false},                  // margen inicial
		{paused, 1 * s, 0, true},
	} {
		if got := giveUp(c.q, c.elapsed, c.stalled); got != c.want {
			t.Errorf("%+v a los %s (sin trabajar %s): %v, se esperaba %v", c.q, c.elapsed, c.stalled, got, c.want)
		}
	}
}

func TestErrorReason(t *testing.T) {
	if !errorReason("Unable to send data to printer.") || !errorReason("Waiting for printer to become available.") || errorReason("Sending data to printer.") {
		t.Fatal("errorReason debe distinguir fallas de avisos normales")
	}
}
