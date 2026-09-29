# Open POS

[![Pruebas](https://github.com/Calebpyn/open-pos/actions/workflows/ci.yml/badge.svg)](https://github.com/Calebpyn/open-pos/actions/workflows/ci.yml)
[![Licencia: AGPL-3.0](https://img.shields.io/badge/licencia-AGPL--3.0-blue.svg)](LICENSE)

Punto de venta **libre y gratuito** para cafeterías y restaurantes pequeños,
hecho para funcionar **100% local y sin internet**: un solo ejecutable en una
Mac, base de datos SQLite, impresoras térmicas ESC/POS por USB o red, y
terminales (iPad, teléfonos) que se conectan por el Wi-Fi del negocio.

> **Estado: beta.** Por ahora **solo corre en Mac** (macOS 12 o más reciente).
> Se prueba todos los días en el negocio del autor, una cafetería en México,
> pero todavía puede tener fallas: haz respaldos y reporta lo que encuentres.

*[English summary below](#english-summary).*

## Así se ve

![Toma de orden: plano del salón, menú por categorías y la comanda de la mesa con cuentas por persona](docs/capturas/pos.png)

| Cobro | Monitor Expo |
|---|---|
| ![Cobro con cuentas por persona, descuentos, propina y pagos mixtos](docs/capturas/cobro.png) | ![Monitor Expo con tiempos por platillo](docs/capturas/expo.png) |

<img src="docs/capturas/ticket.png" alt="Ticket de venta con propina y pago con tarjeta" width="320" align="right">

**Cobro:** por productos o por persona, descuentos, propina y pagos mixtos
(efectivo, tarjeta, transferencia).

**Monitor Expo:** qué falta por entregar en cada mesa y cuánto tiempo lleva
cada platillo.

**Ticket:** se imprime en térmicas de 58 u 80 mm.

*Las capturas usan datos de ejemplo.*

<br clear="right">

## ¿Para quién es?

Para negocios pequeños de México que van empezando y no quieren (o no pueden)
pagar un punto de venta comercial.

- **Sin licencias, suscripciones ni comisiones.** Open POS es gratis y lo
  seguirá siendo.
- **Usa lo que ya tienes.** Corre en una Mac con macOS 12 o más reciente.
  El iPad o los teléfonos del equipo sirven como terminales de mesero; no
  necesitas comprar tabletas especiales.
- **Impresoras económicas.** Funciona con térmicas ESC/POS genéricas de 58 u
  80 mm, por USB o red.
- **Tus datos son tuyos.** Todo se queda en la computadora del negocio, sin
  nube ni cuentas en servicios de terceros. Si se va el internet, sigues
  vendiendo.

### Lo que todavía no hace

Mejor saberlo antes de instalarlo:

- **No emite facturas (CFDI) ni timbra.** Imprime tickets de venta; para
  facturar sigue usando tu proveedor de facturación o el portal del SAT.
- **No cobra con tarjeta por sí mismo.** Usa tu terminal (Mercado Pago u
  otra) y registra el pago en Open POS; el corte de caja ayuda a conciliarla.
- **No lleva inventario.**
- **Por ahora el servidor solo corre en Mac** (macOS 12 Monterey o más
  reciente). Las terminales sí pueden ser cualquier iPad, teléfono o
  computadora con un navegador moderno.
- **Una sola sucursal.** No hay nube ni panel para varias sucursales.

Si algo de esto es indispensable para ti, [cuéntanos](https://github.com/Calebpyn/open-pos/issues/new/choose):
así sabemos qué hace falta primero.

## Instalación (para el negocio)

1. Descarga el archivo `.dmg` de la versión más reciente en [Versiones](https://github.com/Calebpyn/open-pos/releases)
   (sección *Assets*).
2. Ábrelo, haz doble clic en **Open POS** y elige **Instalar**.
3. Si macOS dice que no puede verificar al desarrollador: **Configuración del
   Sistema → Privacidad y seguridad → "Abrir de todos modos"**. (La app
   todavía no está firmada con una cuenta de desarrollador de Apple.)
4. Si macOS pregunta si puede aceptar conexiones entrantes, elige
   **Permitir**: sin eso el iPad no se conecta.
5. Entra a **Administración** con el PIN `0001` y **cámbialo** en Usuarios.

La guía completa (uso diario, conectar un iPad, respaldos, actualizar) está en
[`packaging/macos/Léeme.txt`](packaging/macos/Léeme.txt), que también viene
dentro del instalador.

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

- **Servidor:** macOS 12 Monterey o más reciente (Apple Silicon o Intel). Es la única plataforma probada y con instalador. El código compila para Linux, pero no se ha probado ahí; si lo intentas, [cuéntanos cómo te fue](https://github.com/Calebpyn/open-pos/issues/new/choose).
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
scripts/build-dmg.sh 0.7.2-beta
```

Genera `dist/OpenPOS-<versión>.dmg` con `Open POS.app` (servidor universal
arm64/x86_64 + app en Swift). Al abrirlo desde el DMG se instala en
Aplicaciones. Los datos viven en `~/Library/Application Support/Open POS/` y
**no se tocan al actualizar**; si una versión trae cambios a la base, antes se
guarda un respaldo automático. Detalles de uso para el negocio en
[`packaging/macos/Léeme.txt`](packaging/macos/Léeme.txt).

Las versiones que agregan migraciones conviene instalarlas al cierre, sin
mesas abiertas.

## Administración remota (Tailscale)

El servidor reconoce las peticiones que llegan por [Tailscale](https://tailscale.com)
(direcciones `100.64.0.0/10` y `fd7a:115c:a1e0::/48`) y las trata como
**admin remoto**: solo `/admin` y `/api/admin/*` (con PIN de admin); el POS, la
caja y el cobro se redirigen al panel o se rechazan.

1. Instala Tailscale en la Mac del negocio y en tu celular/laptop, con la misma cuenta.
2. En **Admin → Configuración → Acceso remoto** aparecen las direcciones
   (nombre MagicDNS e IP de Tailscale) listas para copiar.
3. La Mac debe quedar encendida y sin reposo automático.

No hace falta abrir puertos ni exponer el servidor a internet.

## Datos y respaldos

- Todo el dinero se calcula en centavos (enteros) para evitar errores de redondeo.
- Nada se borra: órdenes canceladas, productos anulados y mesas o productos archivados se conservan para el historial.
- Respaldo automático diario (se guardan 14) con `VACUUM INTO`, más uno antes de cada migración.
- Los respaldos viven en el mismo disco que la base: copia uno fuera del local de vez en cuando.

## Hoja de ruta

Lo que sigue, en ese orden aproximado:

- **Salir de beta** con lo aprendido en el uso diario.
- **Versión para Windows**, para que la computadora que ya tiene el negocio
  sirva como servidor sin importar si es Mac o PC.

¿Te hace falta otra cosa? [Propónla](https://github.com/Calebpyn/open-pos/issues/new/choose):
las ideas que vienen de negocios reales tienen prioridad.

## Contribuir

Reportar fallas, probar con tu impresora, mejorar la documentación o escribir
código: todo ayuda. Lee la [guía para contribuir](CONTRIBUTING.md) y el
[código de conducta](CODE_OF_CONDUCT.md). Los problemas de seguridad se
reportan en privado, como explica [SECURITY.md](SECURITY.md). Los cambios de
cada versión están en el [CHANGELOG](CHANGELOG.md).

## Licencia

Open POS es software libre bajo la licencia
[GNU AGPL-3.0](LICENSE). En palabras simples:

- Puedes usarlo en tu negocio, gratis, sin límite de terminales ni de tiempo.
- Puedes modificarlo y compartirlo.
- Si distribuyes una versión modificada, **o la ofreces como servicio a otros
  por internet**, tienes que publicar tu código bajo la misma licencia.

Lo último es a propósito: evita que alguien tome Open POS, lo cierre y lo
venda como otro punto de venta de suscripción.

Copyright © 2026 Alonso Payán y colaboradores.

## English summary

Open POS is a free, open-source (AGPL-3.0) point of sale for small cafés and
restaurants, built to run **fully offline** on a single computer: one Go
binary, SQLite, ESC/POS thermal printers over USB (CUPS) or network, and
iPads/phones as waiter terminals over the local Wi-Fi. It is used daily in a
café in Mexico, and the interface and documentation are in Spanish. Issues and
pull requests in English are welcome too.
