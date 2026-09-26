package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"

	"github.com/calebpyn/open-pos/internal/printer"
)

type PrinterView struct {
	ID             int64    `json:"id"`
	Name           string   `json:"name"`
	ConnectionType string   `json:"connection_type"`
	SystemName     string   `json:"system_name"`
	DeviceURI      string   `json:"device_uri"`
	IPAddress      string   `json:"ip_address"`
	Port           int      `json:"port"`
	PaperWidth     int      `json:"paper_width"`
	IsActive       bool     `json:"is_active"`
	PrintsReceipts bool     `json:"prints_receipts"`
	HasDrawer      bool     `json:"has_drawer"` // tiene el cajón de dinero conectado
	AreaIDs        []int64  `json:"area_ids"`   // áreas de producción cuyas comandas imprime
	Areas          []string `json:"areas"`      // nombres, para mostrar
	// Status: ready, printing, disabled, missing_queue, offline, inactive
	Status       string `json:"status"`
	StatusReason string `json:"status_reason"` // motivo que da CUPS, si lo hay
	PendingJobs  int    `json:"pending_jobs"`  // trabajos atorados en la cola
}

type printerRequest struct {
	Name           string  `json:"name"`
	ConnectionType string  `json:"connection_type"`
	SystemName     string  `json:"system_name"`
	DeviceURI      string  `json:"device_uri"`
	IPAddress      string  `json:"ip_address"`
	Port           int     `json:"port"`
	PaperWidth     int     `json:"paper_width"`
	IsActive       bool    `json:"is_active"`
	PrintsReceipts bool    `json:"prints_receipts"`
	HasDrawer      bool    `json:"has_drawer"`
	AreaIDs        []int64 `json:"area_ids"`
}

func (req *printerRequest) validate() error {
	req.Name = strings.TrimSpace(req.Name)
	req.SystemName = strings.TrimSpace(req.SystemName)
	req.DeviceURI = strings.TrimSpace(req.DeviceURI)
	req.IPAddress = strings.TrimSpace(req.IPAddress)

	if req.Name == "" {
		return errors.New("El nombre es obligatorio")
	}
	if req.PaperWidth != 58 && req.PaperWidth != 80 {
		return errors.New("El ancho de papel debe ser 58 u 80 mm")
	}

	switch req.ConnectionType {
	case printer.ConnCUPS:
		if !printer.ValidQueueName(req.SystemName) {
			return errors.New("El nombre de sistema solo puede tener letras, números, punto, guion y guion bajo (sin espacios)")
		}
		if req.DeviceURI != "" && !printer.ValidDeviceURI(req.DeviceURI) {
			return errors.New("El dispositivo USB no es válido")
		}
		req.IPAddress, req.Port = "", 9100
	case printer.ConnNetwork:
		if net.ParseIP(req.IPAddress) == nil {
			return errors.New("La dirección IP no es válida")
		}
		if req.Port == 0 {
			req.Port = 9100
		}
		if req.Port < 1 || req.Port > 65535 {
			return errors.New("El puerto no es válido")
		}
		// system_name es NOT NULL en el esquema; para red se guarda la dirección.
		req.SystemName = fmt.Sprintf("%s:%d", req.IPAddress, req.Port)
		req.DeviceURI = ""
	default:
		return errors.New("Tipo de conexión inválido")
	}

	seen := map[int64]bool{}
	ids := []int64{}
	for _, id := range req.AreaIDs {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	req.AreaIDs = ids
	return nil
}

func loadPrinters(ctx context.Context, q queryer) ([]PrinterView, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, name, COALESCE(connection_type, 'CUPS'), system_name,
		       COALESCE(device_uri, ''), COALESCE(ip_address, ''), COALESCE(port, 9100),
		       COALESCE(paper_width, 80), COALESCE(is_active, 1), COALESCE(prints_receipts, 0), COALESCE(has_drawer, 0)
		FROM printers ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	printers := []PrinterView{}
	index := map[int64]int{}
	for rows.Next() {
		p := PrinterView{AreaIDs: []int64{}, Areas: []string{}}
		if err := rows.Scan(&p.ID, &p.Name, &p.ConnectionType, &p.SystemName, &p.DeviceURI,
			&p.IPAddress, &p.Port, &p.PaperWidth, &p.IsActive, &p.PrintsReceipts, &p.HasDrawer); err != nil {
			return nil, err
		}
		index[p.ID] = len(printers)
		printers = append(printers, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	aRows, err := q.QueryContext(ctx, `
		SELECT pa.printer_id, a.id, a.name
		FROM printer_areas pa JOIN production_areas a ON a.id = pa.area_id
		ORDER BY a.sort_order, a.id`)
	if err != nil {
		return nil, err
	}
	defer aRows.Close()
	for aRows.Next() {
		var printerID, areaID int64
		var areaName string
		if err := aRows.Scan(&printerID, &areaID, &areaName); err != nil {
			return nil, err
		}
		if i, ok := index[printerID]; ok {
			printers[i].AreaIDs = append(printers[i].AreaIDs, areaID)
			printers[i].Areas = append(printers[i].Areas, areaName)
		}
	}
	return printers, aRows.Err()
}

// savePrinterRoutes reemplaza las áreas de producción cuyas comandas imprime la impresora.
func savePrinterRoutes(ctx context.Context, tx *sql.Tx, printerID int64, areaIDs []int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM printer_areas WHERE printer_id = ?`, printerID); err != nil {
		return err
	}
	for _, areaID := range areaIDs {
		res, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO printer_areas (printer_id, area_id)
			SELECT ?, id FROM production_areas WHERE id = ?`, printerID, areaID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("área %d no encontrada", areaID)
		}
	}
	return nil
}

func loadPrinter(ctx context.Context, q queryer, id int64) (*PrinterView, error) {
	printers, err := loadPrinters(ctx, q)
	if err != nil {
		return nil, err
	}
	for i := range printers {
		if printers[i].ID == id {
			return &printers[i], nil
		}
	}
	return nil, sql.ErrNoRows
}

// fillStatus consulta CUPS y la red para saber si cada impresora está lista.
func fillStatus(ctx context.Context, printers []PrinterView) {
	queues, err := printer.ListQueues(ctx)
	if err != nil {
		queues = map[string]printer.Queue{}
	}
	for i := range printers {
		p := &printers[i]
		switch {
		case !p.IsActive:
			p.Status = "inactive"
		case p.ConnectionType == printer.ConnNetwork:
			if printer.Reachable(ctx, p.IPAddress, p.Port) {
				p.Status = "ready"
			} else {
				p.Status = "offline"
			}
		default:
			q, ok := queues[p.SystemName]
			switch {
			case !ok:
				p.Status = "missing_queue"
			case !q.Enabled:
				p.Status = "disabled"
			case q.State == "printing":
				p.Status = "printing"
			default:
				p.Status = "ready"
			}
			if ok {
				p.StatusReason, p.PendingJobs = q.Reason, q.Pending
				if p.DeviceURI == "" {
					p.DeviceURI = q.URI
				}
			}
		}
	}
}

// GET /api/printers
func (h *POSHandler) ListPrinters(w http.ResponseWriter, r *http.Request) {
	printers, err := loadPrinters(r.Context(), h.DB)
	if err != nil {
		serverError(w, "Error consultando impresoras", err)
		return
	}
	fillStatus(r.Context(), printers)
	writeJSON(w, http.StatusOK, printers)
}

type DetectedDevice struct {
	printer.Device
	RegisteredAs string `json:"registered_as"` // nombre de la impresora que ya lo usa
}

// GET /api/printers/detect - Impresoras USB conectadas a este equipo
func (h *POSHandler) DetectPrinters(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	devices, err := printer.DetectUSB(ctx)
	if err != nil {
		serverError(w, "Error detectando dispositivos USB", err)
		return
	}
	printers, err := loadPrinters(ctx, h.DB)
	if err != nil {
		serverError(w, "Error consultando impresoras", err)
		return
	}
	fillStatus(ctx, printers)

	out := []DetectedDevice{}
	for _, d := range devices {
		dd := DetectedDevice{Device: d}
		for _, p := range printers {
			if p.DeviceURI == d.URI {
				dd.RegisteredAs = p.Name
				break
			}
		}
		out = append(out, dd)
	}
	writeJSON(w, http.StatusOK, out)
}

func decodePrinterRequest(w http.ResponseWriter, r *http.Request) (*printerRequest, bool) {
	var req printerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "JSON inválido", http.StatusBadRequest)
		return nil, false
	}
	if err := req.validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return nil, false
	}
	return &req, true
}

func systemNameTaken(ctx context.Context, tx *sql.Tx, systemName string, exceptID int64) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM printers WHERE system_name = ? AND id != ?`, systemName, exceptID).Scan(&n)
	return n > 0, err
}

// POST /api/printers - Registra una impresora (y su cola de CUPS si es USB)
func (h *POSHandler) CreatePrinter(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	req, ok := decodePrinterRequest(w, r)
	if !ok {
		return
	}

	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		serverError(w, "Error al iniciar transacción", err)
		return
	}
	defer tx.Rollback()

	if taken, err := systemNameTaken(ctx, tx, req.SystemName, 0); err != nil {
		serverError(w, "Error validando impresora", err)
		return
	} else if taken {
		http.Error(w, "Ya hay una impresora registrada con ese nombre de sistema", http.StatusConflict)
		return
	}

	res, err := tx.ExecContext(ctx, `
		INSERT INTO printers (name, system_name, connection_type, ip_address, port, is_active, device_uri, paper_width, prints_receipts, has_drawer, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`,
		req.Name, req.SystemName, req.ConnectionType, nullIfEmpty(req.IPAddress), req.Port,
		req.IsActive, nullIfEmpty(req.DeviceURI), req.PaperWidth, req.PrintsReceipts, req.HasDrawer)
	if err != nil {
		serverError(w, "Error registrando impresora", err)
		return
	}
	id, _ := res.LastInsertId()

	if err := savePrinterRoutes(ctx, tx, id, req.AreaIDs); err != nil {
		http.Error(w, "Error guardando áreas: "+err.Error(), http.StatusBadRequest)
		return
	}

	// La cola se crea antes del commit: si CUPS falla, no queda un registro a medias.
	if req.ConnectionType == printer.ConnCUPS && req.DeviceURI != "" {
		if err := printer.EnsureQueue(ctx, req.SystemName, req.DeviceURI, req.Name); err != nil {
			http.Error(w, "No se pudo crear la cola en CUPS: "+err.Error(), http.StatusBadGateway)
			return
		}
	}

	if err := audit(ctx, tx, "PRINTER_CREATED", map[string]any{
		"printer_id": id, "name": req.Name, "system_name": req.SystemName,
		"prints_receipts": req.PrintsReceipts, "has_drawer": req.HasDrawer, "area_ids": req.AreaIDs,
	}); err != nil {
		serverError(w, "Error registrando bitácora", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "Error al guardar impresora", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

// PUT /api/printers/{id}
func (h *POSHandler) UpdatePrinter(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := pathID(r)
	if !ok {
		http.Error(w, "ID de impresora inválido", http.StatusBadRequest)
		return
	}
	req, ok := decodePrinterRequest(w, r)
	if !ok {
		return
	}

	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		serverError(w, "Error al iniciar transacción", err)
		return
	}
	defer tx.Rollback()

	var oldConn, oldSystem string
	var oldURI sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT COALESCE(connection_type, 'CUPS'), system_name, device_uri FROM printers WHERE id = ?`, id).
		Scan(&oldConn, &oldSystem, &oldURI)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "Impresora no encontrada", http.StatusNotFound)
		return
	}
	if err != nil {
		serverError(w, "Error consultando impresora", err)
		return
	}

	if taken, err := systemNameTaken(ctx, tx, req.SystemName, id); err != nil {
		serverError(w, "Error validando impresora", err)
		return
	} else if taken {
		http.Error(w, "Ya hay una impresora registrada con ese nombre de sistema", http.StatusConflict)
		return
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE printers SET name = ?, system_name = ?, connection_type = ?, ip_address = ?, port = ?,
		       is_active = ?, device_uri = ?, paper_width = ?, prints_receipts = ?, has_drawer = ?
		WHERE id = ?`,
		req.Name, req.SystemName, req.ConnectionType, nullIfEmpty(req.IPAddress), req.Port,
		req.IsActive, nullIfEmpty(req.DeviceURI), req.PaperWidth, req.PrintsReceipts, req.HasDrawer, id); err != nil {
		serverError(w, "Error actualizando impresora", err)
		return
	}

	if err := savePrinterRoutes(ctx, tx, id, req.AreaIDs); err != nil {
		http.Error(w, "Error guardando áreas: "+err.Error(), http.StatusBadRequest)
		return
	}

	if req.ConnectionType == printer.ConnCUPS && req.DeviceURI != "" {
		if err := printer.EnsureQueue(ctx, req.SystemName, req.DeviceURI, req.Name); err != nil {
			http.Error(w, "No se pudo actualizar la cola en CUPS: "+err.Error(), http.StatusBadGateway)
			return
		}
	}

	if err := audit(ctx, tx, "PRINTER_UPDATED", map[string]any{
		"printer_id": id, "name": req.Name, "system_name": req.SystemName,
		"prints_receipts": req.PrintsReceipts, "has_drawer": req.HasDrawer, "area_ids": req.AreaIDs,
	}); err != nil {
		serverError(w, "Error registrando bitácora", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "Error al guardar impresora", err)
		return
	}

	// Si la cola que creamos antes cambió de nombre o ya no se usa, se limpia.
	if oldConn == printer.ConnCUPS && oldURI.Valid && oldURI.String != "" &&
		(req.ConnectionType != printer.ConnCUPS || oldSystem != req.SystemName) {
		if err := printer.RemoveQueue(ctx, oldSystem); err != nil {
			serverError(w, "Impresora guardada, pero no se pudo borrar la cola anterior", err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

// DELETE /api/printers/{id}?remove_queue=1
func (h *POSHandler) DeletePrinter(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := pathID(r)
	if !ok {
		http.Error(w, "ID de impresora inválido", http.StatusBadRequest)
		return
	}

	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		serverError(w, "Error al iniciar transacción", err)
		return
	}
	defer tx.Rollback()

	var name, conn, systemName string
	err = tx.QueryRowContext(ctx,
		`SELECT name, COALESCE(connection_type, 'CUPS'), system_name FROM printers WHERE id = ?`, id).
		Scan(&name, &conn, &systemName)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "Impresora no encontrada", http.StatusNotFound)
		return
	}
	if err != nil {
		serverError(w, "Error consultando impresora", err)
		return
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM printer_areas WHERE printer_id = ?`, id); err != nil {
		serverError(w, "Error liberando áreas", err)
		return
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM printers WHERE id = ?`, id); err != nil {
		serverError(w, "Error eliminando impresora", err)
		return
	}
	if err := audit(ctx, tx, "PRINTER_DELETED", map[string]any{"printer_id": id, "name": name, "system_name": systemName}); err != nil {
		serverError(w, "Error registrando bitácora", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "Error al eliminar impresora", err)
		return
	}

	if r.URL.Query().Get("remove_queue") == "1" && conn == printer.ConnCUPS {
		if err := printer.RemoveQueue(ctx, systemName); err != nil {
			serverError(w, "Impresora eliminada, pero no se pudo borrar la cola de CUPS", err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /api/printers/{id}/test - Imprime un ticket de prueba
func (h *POSHandler) TestPrinter(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := pathID(r)
	if !ok {
		http.Error(w, "ID de impresora inválido", http.StatusBadRequest)
		return
	}
	p, err := loadPrinter(ctx, h.DB, id)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "Impresora no encontrada", http.StatusNotFound)
		return
	}
	if err != nil {
		serverError(w, "Error consultando impresora", err)
		return
	}
	settings, err := loadSettings(ctx, h.DB)
	if err != nil {
		serverError(w, "Error cargando configuración", err)
		return
	}

	jobID, err := printer.Send(ctx, targetOf(p), testDoc(settings.BusinessName, p).Bytes())
	if err != nil {
		http.Error(w, "No se pudo imprimir: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job_id": jobID})
}

// POST /api/printers/{id}/resume?discard=1 - Reanuda una cola que CUPS pausó.
// Con discard=1 descarta antes lo que se quedó atorado (comandas y tickets
// viejos) para que no salga todo de golpe.
func (h *POSHandler) ResumePrinter(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := pathID(r)
	if !ok {
		http.Error(w, "ID de impresora inválido", http.StatusBadRequest)
		return
	}
	p, err := loadPrinter(ctx, h.DB, id)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "Impresora no encontrada", http.StatusNotFound)
		return
	}
	if err != nil {
		serverError(w, "Error consultando impresora", err)
		return
	}
	if p.ConnectionType != printer.ConnCUPS {
		http.Error(w, "Solo las impresoras USB (CUPS) se pausan", http.StatusBadRequest)
		return
	}
	discard := r.URL.Query().Get("discard") == "1"
	if discard {
		if err := printer.CancelJobs(ctx, p.SystemName); err != nil {
			http.Error(w, "No se pudieron descartar los trabajos: "+err.Error(), http.StatusBadGateway)
			return
		}
	}
	// De paso se corrige la política para que no se vuelva a pausar sola.
	printer.FixErrorPolicy(ctx, p.SystemName)
	if err := printer.Resume(ctx, p.SystemName); err != nil {
		http.Error(w, "No se pudo reanudar: "+err.Error(), http.StatusBadGateway)
		return
	}
	u, _ := currentUser(ctx)
	tx, err := h.DB.BeginTx(ctx, nil)
	if err == nil {
		auditBy(ctx, tx, u, r, "PRINTER_RESUMED", map[string]any{"printer_id": id, "name": p.Name, "discarded_jobs": discard})
		tx.Commit()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// RepairPrinterQueues aplica a las colas de CUPS existentes la política de
// reintento (las creadas antes de este cambio se pausaban ante cualquier falla).
func RepairPrinterQueues(ctx context.Context, db *sql.DB) {
	printers, err := loadPrinters(ctx, db)
	if err != nil {
		log.Printf("No se pudieron revisar las colas de impresión: %v", err)
		return
	}
	for _, p := range printers {
		if p.ConnectionType != printer.ConnCUPS || !printer.ValidQueueName(p.SystemName) {
			continue
		}
		if err := printer.FixErrorPolicy(ctx, p.SystemName); err != nil {
			log.Printf("No se pudo ajustar la cola %s: %v", p.SystemName, err)
		}
	}
}

// AreaInfo es un área de producción en el formulario de impresoras.
type AreaInfo struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	ColorHex string `json:"color_hex"`
}

// GET /api/areas - Áreas de producción activas (para asignarlas a impresoras)
func (h *POSHandler) ListAreas(w http.ResponseWriter, r *http.Request) {
	rows, err := h.DB.QueryContext(r.Context(), `
		SELECT id, name, color_hex FROM production_areas WHERE is_active = 1 ORDER BY sort_order, id`)
	if err != nil {
		serverError(w, "Error consultando áreas", err)
		return
	}
	defer rows.Close()

	areas := []AreaInfo{}
	for rows.Next() {
		var a AreaInfo
		if err := rows.Scan(&a.ID, &a.Name, &a.ColorHex); err != nil {
			serverError(w, "Error leyendo áreas", err)
			return
		}
		areas = append(areas, a)
	}
	writeJSON(w, http.StatusOK, areas)
}
