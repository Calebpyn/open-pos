package handler

import (
	"os"
	"testing"

	"github.com/calebpyn/open-pos/internal/printer"
)

func TestMain(m *testing.M) {
	// Las impresoras de prueba reciben al instante; no hace falta esperar a
	// que "salga el papel" entre trabajos.
	printer.Pacing = false
	os.Exit(m.Run())
}
