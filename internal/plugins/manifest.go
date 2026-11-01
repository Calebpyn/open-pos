package plugins

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

// Permisos que un plugin puede pedir en su plugin.json.
const (
	PermEvents       = "events:read"           // leer la bitácora de eventos
	PermCatalog      = "catalog:read"          // leer menú, modificadores y áreas
	PermAvailability = "products:availability" // marcar productos agotados/disponibles
)

var knownPerms = map[string]bool{PermEvents: true, PermCatalog: true, PermAvailability: true}

// Manifest es el plugin.json de un plugin.
type Manifest struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Version       string   `json:"version"`
	Description   string   `json:"description,omitempty"`
	APIVersion    int      `json:"api_version"`
	MinPOSVersion string   `json:"min_pos_version,omitempty"`
	Executable    string   `json:"executable"`
	Permissions   []string `json:"permissions"`
	// Admin, si está, agrega una pestaña al panel de administración que
	// abre /admin/plugins/<id>/.
	Admin *AdminEntry `json:"admin,omitempty"`
}

type AdminEntry struct {
	Label string `json:"label"`
}

// El id aparece en rutas, carpetas y nombres de socket.
var idRe = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)

// ReadManifest lee y valida dir/plugin.json.
func ReadManifest(dir, posVersion string) (Manifest, error) {
	var m Manifest
	b, err := os.ReadFile(filepath.Join(dir, "plugin.json"))
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("plugin.json inválido: %w", err)
	}
	return m, m.validate(posVersion)
}

func (m *Manifest) validate(posVersion string) error {
	switch {
	case !idRe.MatchString(m.ID):
		return fmt.Errorf("id %q inválido: minúsculas, números y guiones, empezando con letra", m.ID)
	case strings.TrimSpace(m.Name) == "":
		return fmt.Errorf("falta el nombre del plugin")
	case m.APIVersion != APIVersion:
		return fmt.Errorf("pide la versión %d de la API de plugins; este Open POS tiene la %d", m.APIVersion, APIVersion)
	case m.Executable == "" || !filepath.IsLocal(m.Executable):
		return fmt.Errorf("executable debe ser una ruta dentro de la carpeta del plugin")
	case m.Admin != nil && strings.TrimSpace(m.Admin.Label) == "":
		return fmt.Errorf("admin.label no puede ir vacío")
	}
	for _, p := range m.Permissions {
		if !knownPerms[p] {
			return fmt.Errorf("permiso desconocido %q", p)
		}
	}
	if m.MinPOSVersion != "" && olderThan(posVersion, m.MinPOSVersion) {
		return fmt.Errorf("requiere Open POS %s o más reciente (este es %s)", m.MinPOSVersion, posVersion)
	}
	return nil
}

// Can indica si el plugin pidió un permiso.
func (m *Manifest) Can(perm string) bool {
	for _, p := range m.Permissions {
		if p == perm {
			return true
		}
	}
	return false
}

// executablePath es la ruta del ejecutable (con .exe en Windows).
func (m *Manifest) executablePath(dir string) string {
	exe := m.Executable
	if runtime.GOOS == "windows" && !strings.HasSuffix(exe, ".exe") {
		exe += ".exe"
	}
	return filepath.Join(dir, exe)
}

// olderThan compara versiones "1.2.3" (se ignora "v" y lo que va después de
// "-"). Una versión de desarrollo ("dev") no es más vieja que ninguna.
func olderThan(have, want string) bool {
	h, ok1 := versionParts(have)
	w, ok2 := versionParts(want)
	if !ok1 || !ok2 {
		return false
	}
	for i := range 3 {
		if h[i] != w[i] {
			return h[i] < w[i]
		}
	}
	return false
}

func versionParts(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(v, "v")
	v, _, _ = strings.Cut(v, "-")
	parts := strings.Split(v, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
