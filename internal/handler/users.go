package handler

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"regexp"
)

// User es quien autoriza una operación de caja con su PIN.
type User struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"`
}

var (
	pinRe     = regexp.MustCompile(`^[0-9]{4,6}$`)
	errBadPIN = errors.New("PIN incorrecto")
)

func hashPIN(salt, pin string) string {
	sum := sha256.Sum256([]byte(salt + ":" + pin))
	return hex.EncodeToString(sum[:])
}

// userByPIN busca al usuario activo dueño del PIN. Cada PIN lleva su propia
// sal, así que se compara contra cada usuario (son pocos).
func userByPIN(ctx context.Context, q queryer, pin string) (User, error) {
	if !pinRe.MatchString(pin) {
		return User{}, errBadPIN
	}
	rows, err := q.QueryContext(ctx,
		`SELECT id, name, role, pin_salt, pin_hash FROM users WHERE is_active = 1`)
	if err != nil {
		return User{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var u User
		var salt, hash string
		if err := rows.Scan(&u.ID, &u.Name, &u.Role, &salt, &hash); err != nil {
			return User{}, err
		}
		if subtle.ConstantTimeCompare([]byte(hashPIN(salt, pin)), []byte(hash)) == 1 {
			return u, nil
		}
	}
	if err := rows.Err(); err != nil {
		return User{}, err
	}
	return User{}, errBadPIN
}
