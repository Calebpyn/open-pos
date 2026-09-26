package handler

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

type AdminUser struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Role      string    `json:"role"`
	RoleLabel string    `json:"role_label"`
	IsActive  bool      `json:"is_active"`
	CreatedAt time.Time `json:"created_at"`
}

type userRequest struct {
	Name     string `json:"name"`
	Role     string `json:"role"`
	PIN      string `json:"pin"` // al editar, vacío = no cambia
	IsActive *bool  `json:"is_active"`
}

// GET /api/admin/users
func (h *POSHandler) AdminListUsers(w http.ResponseWriter, r *http.Request) {
	rows, err := h.DB.QueryContext(r.Context(),
		`SELECT id, name, role, is_active, created_at FROM users ORDER BY is_active DESC, name`)
	if err != nil {
		serverError(w, "Error consultando usuarios", err)
		return
	}
	defer rows.Close()
	out := []AdminUser{}
	for rows.Next() {
		var u AdminUser
		if err := rows.Scan(&u.ID, &u.Name, &u.Role, &u.IsActive, &u.CreatedAt); err != nil {
			serverError(w, "Error leyendo usuarios", err)
			return
		}
		u.RoleLabel = roleLabels[u.Role]
		out = append(out, u)
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out, "roles": roleOptions()})
}

func roleOptions() []map[string]string {
	out := []map[string]string{}
	for _, k := range []string{RoleAdmin, RoleManager, RoleCashier, RoleWaiter} {
		out = append(out, map[string]string{"key": k, "label": roleLabels[k]})
	}
	return out
}

// pinTaken indica si otro usuario activo ya usa ese PIN. Como el PIN es lo
// único que identifica a alguien al firmar, no puede repetirse.
func pinTaken(ctx context.Context, q queryer, pin string, exceptID int64) (bool, error) {
	u, err := userByPIN(ctx, q, pin)
	if errors.Is(err, errBadPIN) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return u.ID != exceptID, nil
}

func newSalt() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// POST /api/admin/users
func (h *POSHandler) AdminCreateUser(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	admin, _ := currentUser(ctx)
	var req userRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "JSON inválido", http.StatusBadRequest)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	switch {
	case req.Name == "":
		http.Error(w, "Escribe el nombre", http.StatusBadRequest)
		return
	case roleLabels[req.Role] == "":
		http.Error(w, "Rol inválido", http.StatusBadRequest)
		return
	case !pinRe.MatchString(req.PIN):
		http.Error(w, "El PIN debe tener de 4 a 6 dígitos", http.StatusBadRequest)
		return
	}

	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		serverError(w, "Error al iniciar transacción", err)
		return
	}
	defer tx.Rollback()
	if taken, err := pinTaken(ctx, tx, req.PIN, 0); err != nil {
		serverError(w, "Error validando PIN", err)
		return
	} else if taken {
		http.Error(w, "Ese PIN ya lo usa otro usuario", http.StatusConflict)
		return
	}
	salt, err := newSalt()
	if err != nil {
		serverError(w, "Error generando PIN", err)
		return
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO users (name, role, pin_salt, pin_hash) VALUES (?, ?, ?, ?)`,
		req.Name, req.Role, salt, hashPIN(salt, req.PIN))
	if err != nil {
		serverError(w, "Error creando el usuario", err)
		return
	}
	id, _ := res.LastInsertId()
	if err := auditBy(ctx, tx, admin, r, "USER_CREATED", map[string]any{
		"user_id": id, "name": req.Name, "role": req.Role,
	}); err != nil {
		serverError(w, "Error registrando bitácora", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "Error creando el usuario", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

// PUT /api/admin/users/{id}
func (h *POSHandler) AdminUpdateUser(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	admin, _ := currentUser(ctx)
	id, ok := pathID(r)
	if !ok {
		http.Error(w, "ID inválido", http.StatusBadRequest)
		return
	}
	var req userRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "JSON inválido", http.StatusBadRequest)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || roleLabels[req.Role] == "" || req.IsActive == nil {
		http.Error(w, "Nombre, rol y estado son obligatorios", http.StatusBadRequest)
		return
	}
	if req.PIN != "" && !pinRe.MatchString(req.PIN) {
		http.Error(w, "El PIN debe tener de 4 a 6 dígitos", http.StatusBadRequest)
		return
	}

	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		serverError(w, "Error al iniciar transacción", err)
		return
	}
	defer tx.Rollback()

	var before AdminUser
	err = tx.QueryRowContext(ctx, `SELECT id, name, role, is_active FROM users WHERE id = ?`, id).
		Scan(&before.ID, &before.Name, &before.Role, &before.IsActive)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "Usuario no encontrado", http.StatusNotFound)
		return
	}
	if err != nil {
		serverError(w, "Error consultando el usuario", err)
		return
	}

	// Mientras estuvo de baja su PIN pudo asignarse a alguien más, y como solo
	// se guarda el hash no hay forma de comprobarlo: se pide uno nuevo.
	if !before.IsActive && *req.IsActive && req.PIN == "" {
		http.Error(w, "Para reactivar a un usuario asígnale un PIN nuevo", http.StatusBadRequest)
		return
	}

	stillAdmin := *req.IsActive && req.Role == RoleAdmin
	if id == admin.ID && !stillAdmin {
		http.Error(w, "No puedes quitarte a ti mismo el acceso de administrador", http.StatusConflict)
		return
	}
	if before.IsActive && before.Role == RoleAdmin && !stillAdmin {
		var admins int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM users WHERE is_active = 1 AND role = ? AND id != ?`, RoleAdmin, id).Scan(&admins); err != nil {
			serverError(w, "Error consultando administradores", err)
			return
		}
		if admins == 0 {
			http.Error(w, "Debe quedar al menos un administrador activo", http.StatusConflict)
			return
		}
	}

	if _, err := tx.ExecContext(ctx, `UPDATE users SET name = ?, role = ?, is_active = ? WHERE id = ?`,
		req.Name, req.Role, *req.IsActive, id); err != nil {
		serverError(w, "Error actualizando el usuario", err)
		return
	}
	pinChanged := req.PIN != ""
	if pinChanged {
		if taken, err := pinTaken(ctx, tx, req.PIN, id); err != nil {
			serverError(w, "Error validando PIN", err)
			return
		} else if taken {
			http.Error(w, "Ese PIN ya lo usa otro usuario", http.StatusConflict)
			return
		}
		salt, err := newSalt()
		if err != nil {
			serverError(w, "Error generando PIN", err)
			return
		}
		if _, err := tx.ExecContext(ctx, `UPDATE users SET pin_salt = ?, pin_hash = ? WHERE id = ?`,
			salt, hashPIN(salt, req.PIN), id); err != nil {
			serverError(w, "Error cambiando el PIN", err)
			return
		}
	}
	// Un PIN cambiado o un usuario dado de baja no debe conservar sesiones.
	if (pinChanged || !*req.IsActive || req.Role != before.Role) && id != admin.ID {
		if err := revokeUserSessions(ctx, tx, id); err != nil {
			serverError(w, "Error cerrando sesiones", err)
			return
		}
	}

	if err := auditBy(ctx, tx, admin, r, "USER_UPDATED", map[string]any{
		"user_id":     id,
		"before":      map[string]any{"name": before.Name, "role": before.Role, "is_active": before.IsActive},
		"after":       map[string]any{"name": req.Name, "role": req.Role, "is_active": *req.IsActive},
		"pin_changed": pinChanged,
	}); err != nil {
		serverError(w, "Error registrando bitácora", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "Error actualizando el usuario", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// defaultPINActive indica si el PIN genérico 0001 sigue sirviendo; el panel
// lo advierte hasta que se cambie.
func defaultPINActive(ctx context.Context, q queryer) bool {
	_, err := userByPIN(ctx, q, "0001")
	return err == nil
}
