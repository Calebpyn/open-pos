// Package web contiene las plantillas y los archivos estáticos del POS. Van
// dentro del ejecutable: el servidor es un solo archivo que funciona desde
// cualquier carpeta, sin necesitar la carpeta web/ al lado.
package web

import (
	"embed"
	"io/fs"
	"os"
)

// _nav.html se nombra aparte: go:embed omite archivos que empiezan con "_".
//
//go:embed templates static templates/admin/_nav.html
var embedded embed.FS

// devDir permite editar plantillas sin recompilar: con POS_WEB_DIR=web se
// leen del disco en cada petición.
var devDir = os.Getenv("POS_WEB_DIR")

// Dev indica si las plantillas se leen del disco (modo desarrollo).
func Dev() bool { return devDir != "" }

// FS regresa los archivos: del disco en modo desarrollo o los incluidos en el
// ejecutable.
func FS() fs.FS {
	if devDir != "" {
		return os.DirFS(devDir)
	}
	return embedded
}

// Static es la carpeta static/ para servirla en /static/.
func Static() fs.FS {
	sub, err := fs.Sub(FS(), "static")
	if err != nil {
		panic(err)
	}
	return sub
}
