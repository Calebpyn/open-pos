# Open POS

Punto de venta para cafeterías y restaurantes pequeños, hecho para funcionar
**100% local y sin internet**: un solo ejecutable en una Mac, base de datos
SQLite, impresoras térmicas ESC/POS por USB o red, y terminales (iPad,
teléfonos) que se conectan por el Wi-Fi del negocio.

> Estado: beta en uso diario en una cafetería.

## Qué hace

**Toma de orden y servicio**
- Mesas con **plano del salón** editable (arrastrar y soltar sobre cuadrícula) o lista.
- Órdenes de mesa, para llevar y venta de mostrador (flash).
- Menú como árbol (categoría → subcategoría → producto) con buscador.
- **Modificadores** con precio extra (leche, jarabes, etc.) y notas por producto.
- **Cuentas por persona** desde la toma de orden ("Juan", "Persona 2", "Mesa").
- Comandas por **área de producción** (barra, cocina…), con formato configurable.
- **Monitor Expo**: seguimiento por platillo y por pieza, con tiempos.
- **Editar una orden ya enviada** (cambio de producto, modificadores, cantidad) con PIN de caja y comanda de corrección.

**Cobro y caja**
- Cobro por productos, por piezas de un renglón o por persona; pagos mixtos, propina, descuentos.
- **Pre-cuenta** para llevar a la mesa.
- **Cargo a nómina** para consumo de colaboradores, con vale firmado y liquidación semanal.
- Turnos de caja con **arqueo ciego**, gastos, conciliación de terminal de tarjeta y corte impreso.
- Botón para abrir el cajón de dinero conectado a la impresora.

**Administración** (`/admin`)
- Reportes de ventas, productos, métodos de pago, propinas y **tiempos de salida por área**.
- **Historial** de órdenes y pagos con detalle, bitácora y reimpresión de tickets.
- Menú, mesas, usuarios y roles, impresoras, formatos de impresión (con logo), configuración.
- Respaldos automáticos diarios y descarga de respaldos; diagnóstico de impresión.

**Terminales y acceso**
- La **compu central** (donde corre el servidor) tiene acceso completo.
- iPads y teléfonos entran como **terminal de mesero** con PIN: toman órdenes, ven Expo e imprimen pre-cuentas; no cobran ni entran al admin (se bloquea en el servidor).
- Se registra quién tomó y quién entregó cada producto.
- **Admin remoto** por [Tailscale](https://tailscale.com): desde fuera del local solo se abre el panel de administración.

## Arquitectura

| Pieza | Tecnología |
|---|---|
| Servidor | Go (`net/http`), un solo binario |
| Base de datos | SQLite (driver puro Go, sin cgo), modo WAL, migraciones embebidas |
| Interfaz | Plantillas HTML (`html/template`) + Alpine.js + HTMX, CSS de Tailwind precompilado |
| Impresión | ESC/POS generado por el servidor; USB vía CUPS (`lp -o raw`) o TCP 9100 |
| App de Mac | Envoltorio en Swift (WKWebView) que arranca el servidor y lo apaga al salir |

Todo (plantillas, CSS, JS, migraciones) va **embebido en el ejecutable**; no se
usan CDNs ni servicios externos en tiempo de ejecución.

```
cmd/pos/                 punto de entrada del servidor
internal/database/       conexión SQLite, migraciones, respaldos y mantenimiento
internal/database/migrations/   001…020 (se aplican solas al arrancar)
internal/handler/        HTTP: POS, cobro, caja, Expo, admin, impresión, terminales
internal/printer/        ESC/POS, cola por impresora, CUPS y red
web/templates/           páginas (POS, cobro, caja, admin)
web/static/              CSS precompilado, JS propio y librerías (vendor/)
packaging/macos/         app de Mac (Swift), ícono, Info.plist y Léeme del instalador
scripts/                 build-dmg.sh (instalador) y build-css.sh (CSS)
```

## Requisitos

- **Servidor:** macOS 12+ (Apple Silicon o Intel). También corre en Linux; la impresión USB necesita CUPS.
- **Terminales:** Safari (iOS 15+) o Chrome/Edge recientes, en la misma red.
- **Impresoras:** térmicas ESC/POS de 58 u 80 mm, USB (CUPS) o red.
- **Para compilar:** Go 1.27+. Para la app de Mac y el DMG, Xcode Command Line Tools (`swiftc`).

## Desarrollo

```bash
# Servidor con plantillas leídas del disco (recarga al editar HTML)
POS_WEB_DIR=web go run ./cmd/pos
```

Abre `http://localhost:8080`. La primera vez se crea `pos.db` con datos de
ejemplo; el usuario inicial tiene PIN `0001` (cámbialo en Admin → Usuarios).

Variables de entorno:

| Variable | Uso | Por omisión |
|---|---|---|
| `PORT` | Puerto HTTP | `8080` |
| `POS_DB` | Ruta de la base de datos | `pos.db` |
| `POS_BACKUP_DIR` | Carpeta de respaldos | `respaldos/` junto a la base |
| `POS_WEB_DIR` | Leer plantillas y estáticos del disco (modo desarrollo) | embebidos |

Pruebas:

```bash
go vet ./...
go test -race ./...
```

Si usas clases de Tailwind nuevas en las plantillas, regenera el CSS:

```bash
scripts/build-css.sh
```

## Instalador para la Mac del negocio

```bash
scripts/build-dmg.sh 0.7.1-beta
```

Genera `dist/OpenPOS-<versión>.dmg` con `Open POS.app` (servidor universal
arm64/x86_64 + app en Swift). Al abrirlo desde el DMG se instala en
Aplicaciones. Los datos viven en `~/Library/Application Support/Open POS/` y
**no se tocan al actualizar**; si una versión trae cambios a la base, antes se
guarda un respaldo automático. Detalles de uso para el negocio en
[`packaging/macos/Léeme.txt`](packaging/macos/Léeme.txt).

Las versiones que agregan migraciones conviene instalarlas al cierre, sin
mesas abiertas.

## Datos y respaldos

- Todo el dinero se calcula en centavos (enteros) para evitar errores de redondeo.
- Nada se borra: órdenes canceladas, productos anulados y mesas o productos archivados se conservan para el historial.
- Respaldo automático diario (se guardan 14) con `VACUUM INTO`, más uno antes de cada migración.
- Los respaldos viven en el mismo disco que la base: copia uno fuera del local de vez en cuando.

## Licencia

Aún sin licencia definida.
