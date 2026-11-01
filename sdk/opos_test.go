package opos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePOS sirve eventos 1..n como la API del POS.
func fakePOS(t *testing.T, token string, n int64) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "token", http.StatusUnauthorized)
			return
		}
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		evs := []Event{}
		for s := after + 1; s <= n && len(evs) < 2; s++ {
			evs = append(evs, Event{Seq: s, Type: EventBillPaid, Payload: json.RawMessage(`{"total": 10}`)})
		}
		if len(evs) == 0 {
			time.Sleep(20 * time.Millisecond) // "espera" corta
		}
		json.NewEncoder(w).Encode(map[string]any{"events": evs})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestConsumeRetriesFailedBatch(t *testing.T) {
	srv := fakePOS(t, "tok", 5)
	p := &Plugin{API: NewClient(srv.URL, "tok")}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var mu sync.Mutex
	var seen []int64
	failed := false
	done := make(chan struct{})
	go func() {
		p.Consume(ctx, func(context.Context) (int64, error) { return 0, nil },
			func(_ context.Context, evs []Event) error {
				mu.Lock()
				defer mu.Unlock()
				if evs[0].Seq == 3 && !failed {
					failed = true
					return errors.New("falla simulada")
				}
				var bill BillPaid
				if err := evs[0].Decode(&bill); err != nil || bill.Total != 10 {
					t.Errorf("payload: %v %+v", err, bill)
				}
				for _, e := range evs {
					seen = append(seen, e.Seq)
				}
				if seen[len(seen)-1] == 5 {
					cancel()
				}
				return nil
			})
		close(done)
	}()
	<-done
	if fmt.Sprint(seen) != "[1 2 3 4 5]" {
		t.Fatalf("cada evento una vez y en orden, aun con el reintento: %v", seen)
	}
}

func TestRunRequiresToken(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "opos-sdk") // los sockets Unix piden rutas cortas
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "p.sock")
	p := &Plugin{ID: "p", token: "secreto", socket: socket, mux: http.NewServeMux()}
	p.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		Page(w, "Título", "<p>"+UserFrom(r).Name+" en "+BasePath(r)+"</p>")
	})
	p.OnBackup(func(_ context.Context, dir string) error {
		return os.WriteFile(filepath.Join(dir, "datos.db"), []byte("x"), 0o600)
	})
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error)
	go func() { stopped <- p.Run(ctx) }()

	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
	do := func(method, path, token, body string) *http.Response {
		t.Helper()
		for range 50 {
			req, _ := http.NewRequest(method, "http://plugin"+path, strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("X-Opos-User-Name", "Ana")
			req.Header.Set("X-Opos-Base-Path", "/admin/plugins/p")
			res, err := client.Do(req)
			if err == nil {
				return res
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("el plugin no abrió su socket")
		return nil
	}

	if res := do("GET", "/_opos/health", "otro", ""); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("sin el token correcto: %d", res.StatusCode)
	}
	if res := do("GET", "/_opos/health", "secreto", ""); res.StatusCode != http.StatusOK {
		t.Fatalf("health: %d", res.StatusCode)
	}
	res := do("GET", "/", "secreto", "")
	var body strings.Builder
	buf := make([]byte, 256)
	n, _ := res.Body.Read(buf)
	body.Write(buf[:n])
	if res.Header.Get("X-Opos-Layout") != "admin" || res.Header.Get("X-Opos-Title") != "Título" ||
		body.String() != "<p>Ana en /admin/plugins/p</p>" {
		t.Fatalf("página: %v %q", res.Header, body.String())
	}
	backup := t.TempDir()
	if res := do("POST", "/_opos/backup", "secreto", `{"dir": "`+backup+`"}`); res.StatusCode != http.StatusOK {
		t.Fatalf("respaldo: %d", res.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(backup, "datos.db")); err != nil {
		t.Fatal(err)
	}

	cancel()
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Fatal("al terminar debe borrar su socket")
	}
}
