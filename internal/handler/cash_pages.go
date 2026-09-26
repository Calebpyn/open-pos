package handler

import (
	"database/sql"
	"errors"
	"net/http"
)

// GET /caja - Turno de caja: apertura, movimientos, cajón y arqueo.
func (h *UIHandler) ServeCash(w http.ResponseWriter, r *http.Request) {
	renderPage(w, map[string]any{
		"Denominations": denominations,
		"MovementKinds": movementKinds,
	}, "caja.html")
}

// GET /caja/turnos/{id} - Corte de un turno cerrado. Un turno abierto no se
// muestra: el arqueo es ciego hasta cerrar.
func (h *UIHandler) ServeCashReport(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.Error(w, "ID de turno inválido", http.StatusBadRequest)
		return
	}
	rep, err := loadCashReport(r.Context(), h.POS.DB, id)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "Turno no encontrado", http.StatusNotFound)
		return
	}
	if err != nil {
		serverError(w, "Error calculando el corte", err)
		return
	}
	if !rep.Session.Closed() {
		http.Redirect(w, r, "/caja", http.StatusSeeOther)
		return
	}
	renderPage(w, rep, "caja_reporte.html")
}
