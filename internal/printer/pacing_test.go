package printer

import (
	"context"
	"image"
	"image/color"
	"net"
	"testing"
	"time"
)

func TestPrintDuration(t *testing.T) {
	if d := PrintDuration(DrawerKick()); d != 0 {
		t.Fatalf("el pulso del cajón no saca papel, estimó %v", d)
	}

	// 20 líneas normales + 2 de letra doble + avance de 4 líneas:
	// (20 + 2×2 + 4) × 30 puntos = 840 puntos = 105 mm.
	tk := NewTicket(58)
	for i := 0; i < 20; i++ {
		tk.Line("Latte 12oz              $70.00")
	}
	tk.Size(2, 2).Line("TOTAL").Line("$1,400").Size(1, 1).Feed(4).Cut()
	want := jobOverhead + time.Duration(105.0/mmPerSecond*float64(time.Second))
	if d := PrintDuration(tk.Bytes()); d < want-50*time.Millisecond || d > want+50*time.Millisecond {
		t.Fatalf("ticket de texto: estimó %v, quería ~%v", d, want)
	}

	// Un logo de 96 puntos de alto son 4 franjas de 24; sus bytes (que pueden
	// valer 0x0a) no cuentan como saltos de línea.
	img := image.NewGray(image.Rect(0, 0, 200, 96))
	for y := 0; y < 96; y++ {
		for x := 0; x < 200; x++ {
			img.SetGray(x, y, color.Gray{Y: 0})
		}
	}
	logo := NewTicket(58)
	logo.Image(img, 50)
	d := PrintDuration(logo.Bytes())
	want = jobOverhead + time.Duration(12.0/mmPerSecond*float64(time.Second)) + 4*bandOverhead
	if d < want-50*time.Millisecond || d > want+50*time.Millisecond {
		t.Fatalf("logo: estimó %v, quería ~%v", d, want)
	}
}

// Un trabajo que llega mientras la impresora sigue imprimiendo el anterior se
// pierde: la cola espera a que termine de salir el papel.
func TestQueueWaitsForPaper(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	old, oldPacing := mmPerSecond, Pacing
	mmPerSecond, Pacing = 400, true // 10 veces más rápido para la prueba
	defer func() { mmPerSecond, Pacing = old, oldPacing }()

	tk := NewTicket(58)
	for i := 0; i < 40; i++ {
		tk.Line("linea")
	}
	tgt := Target{ConnectionType: ConnNetwork, IPAddress: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port}
	ctx := context.Background()
	start := time.Now()
	Send(ctx, tgt, tk.Bytes())
	if err := OpenDrawer(ctx, tgt); err != nil {
		t.Fatal(err)
	}
	if waited, need := time.Since(start), PrintDuration(tk.Bytes()); waited < need {
		t.Fatalf("el cajón se mandó a los %v, antes de que terminara el ticket (%v)", waited, need)
	}
}
