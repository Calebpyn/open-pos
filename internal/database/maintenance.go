package database

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// compactIfBloated compacta la base (VACUUM) cuando más de una cuarta parte
// de sus páginas quedó vacía (p. ej. después de borrar un logo grande).
// SQLite no devuelve ese espacio por sí solo. Se hace al arrancar, antes de
// atender peticiones, porque VACUUM bloquea la base mientras corre.
func compactIfBloated(ctx context.Context, db *sql.DB) error {
	var pages, free int
	if err := db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pages); err != nil {
		return err
	}
	if err := db.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&free); err != nil {
		return err
	}
	if pages < 256 || free*4 < pages {
		return nil
	}
	log.Printf("Compactando la base de datos (%d de %d páginas vacías)...", free, pages)
	if _, err := db.ExecContext(ctx, `VACUUM`); err != nil {
		return fmt.Errorf("error compactando la base: %w", err)
	}
	// En modo WAL el archivo se encoge hasta pasar el WAL a la base.
	_, err := db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	return err
}

// BackupKeep es cuántos respaldos se conservan (uno por día).
const BackupKeep = 14

// BackupDirFor es la carpeta de respaldos de una base: POS_BACKUP_DIR o
// "respaldos" junto al archivo de la base.
func BackupDirFor(dbPath string) string {
	if dir := os.Getenv("POS_BACKUP_DIR"); dir != "" {
		return dir
	}
	return filepath.Join(filepath.Dir(dbPath), "respaldos")
}

// Backup es un respaldo guardado en disco.
type Backup struct {
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"created_at"`
}

var backupNameRe = regexp.MustCompile(`^pos-\d{8}-\d{6}\.db$`)

// ValidBackupName evita que un nombre recibido por la API salga de la carpeta.
func ValidBackupName(name string) bool { return backupNameRe.MatchString(name) }

// CreateBackup guarda una copia consistente y compacta de la base en dir
// (VACUUM INTO funciona con el POS en uso) y deja solo los keep más recientes.
func CreateBackup(ctx context.Context, db *sql.DB, dir string, keep int) (Backup, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Backup{}, fmt.Errorf("no se pudo crear la carpeta de respaldos: %w", err)
	}
	name := "pos-" + time.Now().Format("20060102-150405") + ".db"
	path := filepath.Join(dir, name)
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, path); err != nil {
		return Backup{}, fmt.Errorf("error creando el respaldo: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return Backup{}, err
	}
	if err := pruneBackups(dir, keep); err != nil {
		log.Printf("No se pudieron borrar respaldos viejos: %v", err)
	}
	return Backup{Name: name, Size: info.Size(), CreatedAt: info.ModTime()}, nil
}

// ListBackups regresa los respaldos del más nuevo al más viejo.
func ListBackups(dir string) ([]Backup, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return []Backup{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Backup{}
	for _, e := range entries {
		if e.IsDir() || !ValidBackupName(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, Backup{Name: e.Name(), Size: info.Size(), CreatedAt: info.ModTime()})
	}
	// El nombre lleva fecha y hora, así que ordenar por nombre es cronológico.
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out, nil
}

func pruneBackups(dir string, keep int) error {
	list, err := ListBackups(dir)
	if err != nil {
		return err
	}
	for _, b := range list[min(keep, len(list)):] {
		if err := os.Remove(filepath.Join(dir, b.Name)); err != nil {
			return err
		}
	}
	return nil
}

// ScheduleBackups hace un respaldo al arrancar si el último tiene más de un
// día, y luego revisa cada hora (una Mac que se durmió no pierde el suyo).
func ScheduleBackups(ctx context.Context, db *sql.DB, dir string, keep int) {
	check := func() {
		list, err := ListBackups(dir)
		if err == nil && len(list) > 0 && time.Since(list[0].CreatedAt) < 24*time.Hour {
			return
		}
		b, err := CreateBackup(ctx, db, dir, keep)
		if err != nil {
			log.Printf("Respaldo automático fallido: %v", err)
			return
		}
		log.Printf("Respaldo automático: %s (%d KB)", filepath.Join(dir, b.Name), b.Size/1024)
	}
	check()
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			check()
		}
	}
}
