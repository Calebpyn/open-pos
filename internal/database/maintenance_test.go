package database

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBackupsAndCompaction(t *testing.T) {
	dir := t.TempDir()
	db, err := InitDB(filepath.Join(dir, "pos.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	// Un blob grande que luego se borra deja la base llena de páginas vacías.
	big := make([]byte, 4<<20)
	if _, err := db.Exec(`INSERT INTO print_formats (key, data) VALUES ('logo', ?)`, big); err != nil {
		t.Fatal(err)
	}
	db.Exec(`DELETE FROM print_formats WHERE key = 'logo'`)
	if err := compactIfBloated(ctx, db); err != nil {
		t.Fatal(err)
	}
	var free int
	db.QueryRow(`PRAGMA freelist_count`).Scan(&free)
	if free > 10 {
		t.Fatalf("después de compactar quedaron %d páginas vacías", free)
	}

	backups := filepath.Join(dir, "respaldos")
	for i := 0; i < 3; i++ {
		b, err := CreateBackup(ctx, db, backups, 2)
		if err != nil {
			t.Fatal(err)
		}
		if b.Size == 0 {
			t.Fatal("respaldo vacío")
		}
		time.Sleep(1100 * time.Millisecond) // el nombre lleva segundos
	}
	list, _ := ListBackups(backups)
	if len(list) != 2 {
		t.Fatalf("se conservan los 2 más recientes, hay %d", len(list))
	}
	// El respaldo es una base válida con los datos.
	copyDB, err := InitDB(filepath.Join(backups, list[0].Name))
	if err != nil {
		t.Fatal(err)
	}
	var n int
	copyDB.QueryRow(`SELECT COUNT(*) FROM products`).Scan(&n)
	copyDB.Close()
	if n == 0 {
		t.Fatal("el respaldo no trae los productos")
	}
	if ValidBackupName("../pos.db") || !ValidBackupName(list[0].Name) {
		t.Fatal("validación de nombres de respaldo")
	}
	os.Remove(filepath.Join(backups, list[0].Name))
}

// Al actualizar a una versión con cambios de base, antes se respalda la base
// con datos; una base nueva no necesita respaldo.
func TestBackupBeforeMigrating(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pos.db")
	backups := filepath.Join(dir, "respaldos")

	db, err := InitDB(path)
	if err != nil {
		t.Fatal(err)
	}
	if list, _ := ListBackups(backups); len(list) != 0 {
		t.Fatalf("una base nueva no se respalda, hay %d", len(list))
	}
	// Simula que una migración es nueva para esta base (una que se puede
	// repetir: solo crea índices si no existen).
	db.Exec(`DELETE FROM schema_migrations WHERE version = '015_performance_indexes.sql'`)
	db.Exec(`INSERT INTO dining_tables (name, zone) VALUES ('Mesa respaldada', 'Interior')`)
	db.Close()

	db, err = InitDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	list, _ := ListBackups(backups)
	if len(list) != 1 {
		t.Fatalf("se esperaba 1 respaldo antes de actualizar, hay %d", len(list))
	}
	b, err := InitDB(filepath.Join(backups, list[0].Name))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var n int
	b.QueryRow(`SELECT COUNT(*) FROM dining_tables WHERE name = 'Mesa respaldada'`).Scan(&n)
	if n != 1 {
		t.Fatal("el respaldo debe tener los datos de antes de actualizar")
	}
}
