package printer

import (
	"bytes"
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func TestSendWaitsAfterDrawerKick(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var mu sync.Mutex
	var arrivals []time.Time
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			io.ReadAll(c)
			c.Close()
			mu.Lock()
			arrivals = append(arrivals, time.Now())
			mu.Unlock()
		}
	}()

	oldSettle, oldGap := drawerSettle, jobGap
	drawerSettle, jobGap = 300*time.Millisecond, 50*time.Millisecond
	defer func() { drawerSettle, jobGap = oldSettle, oldGap }()

	tgt := Target{ConnectionType: ConnNetwork, IPAddress: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port}
	ctx := context.Background()
	start := time.Now()
	if err := OpenDrawer(ctx, tgt); err != nil {
		t.Fatal(err)
	}
	if _, err := Send(ctx, tgt, []byte("ticket")); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < drawerSettle {
		t.Fatal("el ticket no debe mandarse mientras el cajón está disparando")
	}
	// Un trabajo normal no agrega espera.
	before := time.Now()
	Send(ctx, tgt, []byte("otro"))
	if time.Since(before) > 200*time.Millisecond {
		t.Fatal("entre trabajos normales solo va la pausa corta, no la del cajón")
	}
}

func TestSendKeepsOrder(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan string, 10)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			b, _ := io.ReadAll(c)
			c.Close()
			got <- string(b)
		}
	}()
	tgt := Target{ConnectionType: ConnNetwork, IPAddress: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port}
	var wg sync.WaitGroup
	for _, doc := range []string{"comanda", "ticket", "corte"} {
		wg.Add(1)
		go func(doc string) { defer wg.Done(); Send(context.Background(), tgt, []byte(doc)) }(doc)
		time.Sleep(20 * time.Millisecond) // llegan en este orden
	}
	wg.Wait()
	for _, want := range []string{"comanda", "ticket", "corte"} {
		if doc := <-got; doc != want {
			t.Fatalf("se imprimió %q cuando tocaba %q", doc, want)
		}
	}
}

func TestSendCanceledWhileQueued(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tgt := Target{ConnectionType: ConnNetwork, IPAddress: "127.0.0.1", Port: 9}
	if _, err := Send(ctx, tgt, []byte("x")); err == nil {
		t.Fatal("un trabajo cancelado no debe imprimirse")
	}
}

func TestTicketDoesNotResetPrinter(t *testing.T) {
	b := NewTicket(58).Line("hola").Feed(2).Cut().Bytes()
	if bytes.Contains(b, []byte{0x1b, '@'}) {
		t.Fatal("ESC @ reinicia la impresora y hace perder el principio del documento")
	}
	if !bytes.HasSuffix(b, []byte{0x1d, 'V', 1}) || bytes.Contains(b, []byte{0x1d, 'V', 66}) {
		t.Fatal("el corte debe ser GS V 1 (sin la B que imprimían las impresoras sin cortadora)")
	}
}
