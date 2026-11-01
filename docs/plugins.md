# Plugins

Un plugin es un programa aparte que se instala sobre Open POS para agregarle
algo (inventario, facturación, integraciones) **sin tocar el código del POS**.
Se desarrolla en su propio repositorio y se instala copiando una carpeta.

- Corre como **proceso aparte**: si el plugin falla, la venta sigue.
- **No escribe en `pos.db`**: guarda lo suyo en su propia carpeta de datos.
- Se entera de lo que pasa por una **bitácora de eventos** que se escribe en
  la misma transacción que la venta: aunque el plugin esté caído o se
  instale después, no pierde ni duplica nada.
- Puede tener su **sección en el panel de administración**, que se ve como
  cualquier otra.

Esta es la versión **1** de la API de plugins. El SDK para Go está en
[`sdk/`](../sdk) (licencia MIT) y hay un ejemplo completo en
[`examples/plugins/hola`](../examples/plugins/hola).

## Cómo encaja

```
Mac del negocio
┌───────────────────────── open-pos ─────────────────────────┐
│ venta ──(misma transacción)──► plugin_events               │
│ /admin/plugins/<id>/*  ──proxy──┐      ▲ el plugin lee     │
│ /api/plugin/v1/*  ◄─────────────┼──────┤ eventos y catálogo│
│ supervisor: arranca, reinicia y apaga el plugin            │
└─────────────────────────────────┼──────────────────────────┘
                                  ▼  socket Unix (sin puerto de red)
                    plugins/<id>/  (ejecutable + plugin.json)
                    plugins-datos/<id>/  (sus datos)
```

## Instalar un plugin

1. Copia la carpeta del plugin (su `plugin.json` y su ejecutable) dentro de
   `plugins/`, junto a la base de datos:
   - App de Mac: `~/Library/Application Support/Open POS/plugins/`
   - Servidor a mano: `plugins/` junto a `pos.db` (o `POS_PLUGINS_DIR`).
2. Reinicia Open POS. El plugin aparece en **Administración → 🧩 Plugins**.

Si macOS bloquea el ejecutable por venir de internet, quítale la cuarentena:

```bash
xattr -dr com.apple.quarantine "~/Library/Application Support/Open POS/plugins/<id>"
```

Desactivar un plugin desde el panel lo apaga y deja de recibir eventos; al
activarlo retoma donde se quedó. Para desinstalarlo, borra su carpeta de
`plugins/` (sus datos quedan en `plugins-datos/<id>/` hasta que los borres).

**Instala solo plugins de alguien de confianza.** Un plugin corre en la
misma computadora con los permisos de Open POS: los permisos de
`plugin.json` evitan errores, no aíslan a un programa malicioso.

## plugin.json

```json
{
  "id": "inventario",
  "name": "Inventario",
  "version": "0.1.0",
  "description": "Existencias por insumo con recetas por producto.",
  "api_version": 1,
  "min_pos_version": "0.8.0",
  "executable": "inventario",
  "permissions": ["events:read", "catalog:read", "products:availability"],
  "admin": { "label": "📦 Inventario" }
}
```

| Campo | |
|---|---|
| `id` | Minúsculas, números y guiones (2–32). Va en rutas y carpetas: no lo cambies. |
| `api_version` | Debe ser `1`. |
| `min_pos_version` | Opcional. Una versión de desarrollo del POS (`dev`) no se bloquea. |
| `executable` | Ruta dentro de la carpeta del plugin. En Windows se le agrega `.exe`. |
| `permissions` | `events:read`, `catalog:read`, `products:availability`. |
| `admin` | Opcional: agrega una pestaña al panel que abre `/admin/plugins/<id>/`. |

## Ciclo de vida

Open POS arranca el ejecutable con la carpeta del plugin como directorio de
trabajo y estas variables:

| Variable | |
|---|---|
| `OPOS_PLUGIN_ID` | El id. |
| `OPOS_SOCKET` | Socket Unix donde el plugin debe escuchar HTTP. |
| `OPOS_TOKEN` | Secreto compartido: el POS lo manda en cada petición al plugin (`Authorization: Bearer`) y el plugin lo usa para llamar a la API. Cambia en cada arranque. |
| `OPOS_API_URL` | `http://127.0.0.1:<puerto>` del POS. |
| `OPOS_DATA_DIR` | Carpeta de datos del plugin (fuera de su carpeta de instalación, para que sobreviva a una actualización). |
| `OPOS_POS_VERSION` | Versión del POS. |
| `OPOS_SUPERVISED` | `1` cuando lo arrancó el POS: el plugin debe terminar si su proceso padre desaparece. |

- El POS espera hasta 20 s a que responda `GET /_opos/health`.
- Si el proceso termina, lo vuelve a arrancar esperando 1 s, 2 s, 4 s… hasta
  1 min entre intentos.
- Al apagarse, el POS manda `SIGINT` y espera 10 s antes de matarlo.
- La salida del plugin (stdout/stderr) va al registro del POS con el prefijo
  `[plugin <id>]`, y aparece en el diagnóstico descargable.

Rutas que el plugin debe atender en su socket (el SDK ya las trae):

| Ruta | |
|---|---|
| `GET /_opos/health` | 200 si está listo. |
| `POST /_opos/backup` `{"dir": "..."}` | Dejar en `dir` una copia consistente de sus datos. |

## API para plugins (`/api/plugin/v1`)

Solo responde en la misma computadora y con `Authorization: Bearer <OPOS_TOKEN>`.

| Ruta | Permiso | |
|---|---|---|
| `GET /info` | — | id, permisos, versión del POS y de la API. |
| `GET /events?after=N&limit=100&wait=25` | `events:read` | Eventos con `seq > N`, en orden. Con `wait` (máx. 55 s) espera a que haya alguno. Pedir `after=N` confirma que ya procesaste hasta N. |
| `GET /events/latest` | `events:read` | `{"seq": N}`: el último evento, para empezar "desde ahora". |
| `GET /catalog` | `catalog:read` | Categorías, productos (incluye archivados y agotados), áreas y grupos de modificadores con sus opciones. |
| `POST /products/{id}/availability` `{"available": false, "reason": "..."}` | `products:availability` | Marca un producto agotado o disponible. Queda en la bitácora con el id del plugin. |

## Eventos

Cada evento tiene `seq` (crece siempre), `type`, `payload` y `created_at`
(UTC). Los productos van como **item**:

```json
{
  "item_id": 12, "order_id": 5, "product_id": 1, "product": "Latte 12oz",
  "quantity": 2, "unit_price": 75.0, "status": "PENDING", "notes": "",
  "modifiers": [{ "option_id": 901, "name": "Leche de almendra" }]
}
```

`unit_price` incluye los extras de los modificadores. `option_id` es 0 si la
opción ya no existe en el catálogo.

| Tipo | Cuándo | Payload |
|---|---|---|
| `items.added` | Comanda nueva o productos agregados a una orden | `order_id`, `order_type` (`DINE_IN`, `TAKEAWAY`, `FLASH`), `user_id`, `items` |
| `item.changed` | Caja edita un producto sin cobrar (producto, modificadores, cantidad o nota) | `order_id`, `item_id`, `before`, `after` (items), `user_id` |
| `item.voided` | Se anula un producto, o se cierra con saldo una orden que ya tenía cuentas | `order_id`, `reason`, `items` |
| `item.split` | Al cobrar parte de un renglón ("1 de 2×") se separa en uno nuevo | `order_id`, `item_id`, `new_item_id`, `quantity` (lo que pasó al nuevo), `remaining` |
| `bill.paid` | Cuenta cobrada | `order_id`, `bill_id`, `bill_number`, `total`, `tip`, `items`, `payments` (`method`, `amount`), `user_id` |
| `order.closed` | La orden se cierra | `order_id`, `status` (`PAID` o `CANCELLED`), `reason`, `unpaid_items` (en una cancelada: lo que no se vendió) |
| `items.delivered` | Expo entrega (o destacha) piezas | `order_id`, `items` (`item_id`, `delta`), `user_id` |
| `catalog.changed` | Cambió el menú desde el panel o un plugin | `action` (`PRODUCT_UPDATED`, `PRODUCT_AVAILABILITY`, …), `details` |
| `cash.closed` | Corte de turno | `cash_session_id`, `user_id` |

`user_id` es `null` cuando la operación se hizo en la compu central sin
usuario con PIN.

### Garantías

- **Orden:** los eventos llegan en el orden en que se confirmaron las
  operaciones.
- **Al menos una vez:** si el plugin se cae antes de guardar su avance,
  recibe otra vez el mismo lote. Guarda el efecto del lote **y** el último
  `seq` en una sola transacción de tu base (así lo hace `Consume` del SDK y el
  ejemplo), y nada se cuenta dos veces.
- **Retención:** los eventos que ya leyeron todos los plugins activos se
  borran a los 30 días; ninguno pasa de 90. Un plugin recién instalado puede
  reconstruir el último mes desde `after=0`, o empezar desde ahora con
  `/events/latest`.

## Páginas en el panel

Todo lo que el plugin sirva queda en `/admin/plugins/<id>/…`, detrás del
inicio de sesión de administrador (también por Tailscale). El plugin recibe:

| Encabezado | |
|---|---|
| `X-Opos-User-Id`, `X-Opos-User-Name`, `X-Opos-User-Role` | Quién está usando el panel. |
| `X-Opos-Base-Path` | `/admin/plugins/<id>`: úsalo para enlaces y formularios. |

El plugin nunca ve la cookie de sesión del POS, y el POS descarta los
`Set-Cookie` que responda el plugin.

Si una respuesta HTML trae `X-Opos-Layout: admin` (y opcionalmente
`X-Opos-Title`), el POS la pone dentro del panel: barra de navegación,
`app.css` (Tailwind), Alpine y htmx. Así la página se ve como una sección más.
Solo están las clases de Tailwind que ya usa el POS; para otras, trae tu
propio CSS. Las respuestas sin ese encabezado (JSON, CSS, descargas) pasan tal
cual.

## Respaldos

Con cada respaldo del POS (el diario y "Respaldar ahora"), cada plugin activo
recibe `POST /_opos/backup` con una carpeta
`respaldos/plugins/<id>/<fecha>/`. Se conservan tantos como respaldos de la
base.

## Desarrollar un plugin

1. Copia [`examples/plugins/hola`](../examples/plugins/hola) a tu propio
   repositorio y cambia el `id`.
2. Arranca Open POS apuntando a tu carpeta:
   ```bash
   POS_PLUGIN_DEV=/ruta/a/mi-plugin go run ./cmd/pos
   ```
   El POS no lo arranca: escribe `.opos-dev.env` en tu carpeta con la conexión
   (agrégalo a `.gitignore`).
3. Corre tu plugin con `go run .` en su carpeta. Reinícialo cuando reinicies
   el POS: el token cambia en cada arranque.

Varios plugins en desarrollo se separan con `:` en `POS_PLUGIN_DEV`.

Para distribuirlo, compila para la Mac del negocio y entrega la carpeta con
`plugin.json` y el ejecutable:

```bash
GOOS=darwin GOARCH=arm64 go build -ldflags "-s -w" -o dist/mi-plugin/mi-plugin .
cp plugin.json dist/mi-plugin/
```

## Compatibilidad

Dentro de `api_version: 1` solo se **agregan** cosas: eventos nuevos, campos
nuevos en los payloads y rutas nuevas. Ignora lo que no conozcas. Un cambio
que rompa algo sube la versión de la API.
