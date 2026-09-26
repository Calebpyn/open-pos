package database

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"log"
	"net/url"
	"path/filepath"
	"sort"
	"strings"

	_ "github.com/glebarez/go-sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migraciones que ya existían antes de tener control de versiones.
// Una base creada con el InitDB anterior ya las trae aplicadas.
var legacyMigrations = []string{"001_initial_schema.sql", "002_seed_data.sql"}

func InitDB(dbPath string) (*sql.DB, error) {
	// Los PRAGMAs van en el DSN para que se apliquen a CADA conexión del pool,
	// no solo a la primera. _txlock=immediate toma el lock de escritura al
	// iniciar la transacción y evita SQLITE_BUSY al pasar de lectura a escritura
	// cuando dos terminales escriben al mismo tiempo.
	params := url.Values{}
	params.Add("_pragma", "foreign_keys(1)")
	params.Add("_pragma", "journal_mode(WAL)")
	params.Add("_pragma", "synchronous(NORMAL)")
	params.Add("_pragma", "busy_timeout(5000)")
	// Tras cada checkpoint el WAL se recorta a 8 MB como máximo (antes crecía
	// sin límite y no se encogía).
	params.Add("_pragma", "journal_size_limit(8388608)")
	params.Set("_txlock", "immediate")

	db, err := sql.Open("sqlite", dbPath+"?"+params.Encode())
	if err != nil {
		return nil, fmt.Errorf("error al abrir sqlite: %w", err)
	}

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("error conectando a sqlite: %w", err)
	}

	if err := migrate(context.Background(), db, BackupDirFor(dbPath)); err != nil {
		db.Close()
		return nil, err
	}
	if err := compactIfBloated(context.Background(), db); err != nil {
		log.Printf("%v", err) // no impide arrancar
	}

	return db, nil
}

// migrate aplica en orden las migraciones de migrations/ que aún no estén
// registradas en schema_migrations. Si la base ya tiene datos y una versión
// nueva trae cambios, antes se guarda un respaldo en backupDir: si algo
// saliera mal al actualizar, queda la base exactamente como estaba.
func migrate(ctx context.Context, db *sql.DB, backupDir string) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`); err != nil {
		return fmt.Errorf("error creando schema_migrations: %w", err)
	}

	if err := adoptLegacyDB(ctx, db); err != nil {
		return err
	}

	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("error leyendo migraciones: %w", err)
	}
	var files []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)

	var pending []string
	for _, name := range files {
		var exists int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, name).Scan(&exists); err != nil {
			return fmt.Errorf("error consultando migración %s: %w", name, err)
		}
		if exists == 0 {
			pending = append(pending, name)
		}
	}

	var existing int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&existing); err != nil {
		return err
	}
	if len(pending) > 0 && existing > 0 && backupDir != "" {
		b, err := CreateBackup(ctx, db, backupDir, BackupKeep)
		if err != nil {
			// Sin respaldo no se toca una base con datos.
			return fmt.Errorf("no se pudo respaldar la base antes de actualizarla: %w", err)
		}
		log.Printf("Respaldo antes de actualizar: %s", filepath.Join(backupDir, b.Name))
	}

	for _, name := range pending {
		script, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("error leyendo %s: %w", name, err)
		}

		log.Printf("Aplicando migración %s...", name)
		if err := applyMigration(ctx, db, name, string(script)); err != nil {
			return fmt.Errorf("error en migración %s: %w", name, err)
		}
	}

	return nil
}

// adoptLegacyDB marca como aplicadas las migraciones iniciales en bases
// creadas antes de existir schema_migrations.
func adoptLegacyDB(ctx context.Context, db *sql.DB) error {
	var tracked int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&tracked); err != nil {
		return err
	}
	if tracked > 0 {
		return nil
	}

	var hasSchema int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'printers'`).Scan(&hasSchema); err != nil {
		return err
	}
	if hasSchema == 0 {
		return nil
	}

	log.Println("Base de datos existente detectada, registrando migraciones iniciales...")
	for _, name := range legacyMigrations {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO schema_migrations (version) VALUES (?)`, name); err != nil {
			return err
		}
	}
	return nil
}

// applyMigration corre el script en una sola conexión con foreign_keys
// apagado, como pide SQLite para reconstruir tablas, y valida la integridad
// referencial antes de confirmar.
func applyMigration(ctx context.Context, db *sql.DB, name, script string) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	// foreign_keys no se puede cambiar dentro de una transacción.
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return err
	}
	defer conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`)

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, script); err != nil {
		return err
	}

	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return err
	}
	violations := rows.Next()
	rows.Close()
	if violations {
		return fmt.Errorf("la migración dejó llaves foráneas inválidas")
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version) VALUES (?)`, name); err != nil {
		return err
	}

	return tx.Commit()
}
