package opos

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client habla con la API de plugins de Open POS (/api/plugin/v1).
type Client struct {
	base  string
	token string
	http  *http.Client
}

// NewClient crea un cliente con la dirección y el token que da el POS.
func NewClient(apiURL, token string) *Client {
	// Sin Timeout global: Events espera hasta que haya eventos; cada llamada
	// se acota con su contexto.
	return &Client{base: strings.TrimSuffix(apiURL, "/"), token: token, http: &http.Client{}}
}

// APIError es una respuesta de error del POS.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("open pos %d: %s", e.Status, e.Message) }

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+"/api/plugin/v1"+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return &APIError{Status: res.StatusCode, Message: strings.TrimSpace(string(msg))}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(res.Body).Decode(out)
}

// Info es lo que el POS sabe del plugin.
type Info struct {
	PluginID    string   `json:"plugin_id"`
	Permissions []string `json:"permissions"`
	POSVersion  string   `json:"pos_version"`
	APIVersion  int      `json:"api_version"`
}

func (c *Client) Info(ctx context.Context) (Info, error) {
	var info Info
	err := c.do(ctx, http.MethodGet, "/info", nil, &info)
	return info, err
}

// Events regresa hasta limit eventos posteriores a after, esperando hasta
// wait (máx. 55 s) si todavía no hay. Pedir después de after le confirma al
// POS que ya procesaste hasta ahí. Requiere el permiso events:read.
func (c *Client) Events(ctx context.Context, after int64, limit int, wait time.Duration) ([]Event, error) {
	q := url.Values{}
	q.Set("after", strconv.FormatInt(after, 10))
	q.Set("limit", strconv.Itoa(limit))
	q.Set("wait", strconv.Itoa(int(wait.Seconds())))
	ctx, cancel := context.WithTimeout(ctx, wait+30*time.Second)
	defer cancel()
	var out struct {
		Events []Event `json:"events"`
	}
	err := c.do(ctx, http.MethodGet, "/events?"+q.Encode(), nil, &out)
	return out.Events, err
}

// LatestSeq es el último evento registrado: úsalo como cursor inicial para
// empezar "desde ahora" en lugar de procesar el historial guardado.
func (c *Client) LatestSeq(ctx context.Context) (int64, error) {
	var out struct {
		Seq int64 `json:"seq"`
	}
	err := c.do(ctx, http.MethodGet, "/events/latest", nil, &out)
	return out.Seq, err
}

// Catalog regresa el menú completo, incluidos productos archivados y
// agotados. Requiere el permiso catalog:read.
func (c *Client) Catalog(ctx context.Context) (*Catalog, error) {
	var cat Catalog
	if err := c.do(ctx, http.MethodGet, "/catalog", nil, &cat); err != nil {
		return nil, err
	}
	return &cat, nil
}

// SetAvailability marca un producto agotado (available=false) o disponible.
// El motivo queda en la bitácora del POS. Requiere products:availability.
func (c *Client) SetAvailability(ctx context.Context, productID int64, available bool, reason string) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/products/%d/availability", productID),
		map[string]any{"available": available, "reason": reason}, nil)
}
