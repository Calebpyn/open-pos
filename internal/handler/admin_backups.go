package handler

import (
	"database/sql"
	"net/http"
	"path/filepath"

	"github.com/calebpyn/open-pos/internal/database"
)

// BackupKeep es cuántos respaldos se conservan (uno por día).
const BackupKeep = database.BackupKeep

// GET /api/admin/backups
func (h *POSHandler) AdminListBackups(w http.ResponseWriter, r *http.Request) {
	list, err := database.ListBackups(h.BackupDir)
	if err != nil {
		serverError(w, "Error leyendo respaldos", err)
		return
	}
	dir, _ := filepath.Abs(h.BackupDir)
	writeJSON(w, http.StatusOK, map[string]any{"dir": dir, "keep": BackupKeep, "backups": list})
}

// POST /api/admin/backups - Respaldo inmediato.
func (h *POSHandler) AdminCreateBackup(w http.ResponseWriter, r *http.Request) {
	b, err := database.CreateBackup(r.Context(), h.DB, h.BackupDir, BackupKeep)
	if err != nil {
		serverError(w, "Error creando el respaldo", err)
		return
	}
	h.adminWrite(w, r, "BACKUP_CREATED", func(tx *sql.Tx) (map[string]any, error) {
		return map[string]any{"name": b.Name, "size": b.Size}, nil
	})
}

// GET /api/admin/backups/{name} - Descarga un respaldo (p. ej. para una USB).
func (h *POSHandler) AdminDownloadBackup(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !database.ValidBackupName(name) {
		http.Error(w, "Respaldo inválido", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	http.ServeFile(w, r, filepath.Join(h.BackupDir, name))
}
