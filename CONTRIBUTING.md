# Cómo contribuir

¡Gracias por querer ayudar! Open POS existe para que cualquier negocio pequeño
pueda tener un punto de venta sin pagar licencias ni suscripciones. Toda ayuda
cuenta: reportar fallas, probar con tu impresora, mejorar la documentación o
escribir código.

## Antes de empezar

- **¿Encontraste una falla?** Abre un issue con la plantilla "Reportar una
  falla". Si es de seguridad, **no** abras un issue: sigue [SECURITY.md](SECURITY.md).
- **¿Quieres proponer algo nuevo?** Abre primero un issue con la plantilla
  "Proponer una mejora" y platiquémoslo antes de escribir código. Así no
  trabajas en algo que no encaje con el proyecto.
- **¿Buscas por dónde empezar?** Revisa los issues marcados como
  `buen primer issue`.

## Preparar el entorno

Necesitas [Go](https://go.dev/dl/) 1.27 o más reciente.

```bash
git clone https://github.com/Calebpyn/open-pos.git
cd open-pos
POS_WEB_DIR=web go run ./cmd/pos
```

Abre `http://localhost:8080`. La primera vez se crea `pos.db` con datos de
ejemplo; el PIN inicial es `0001`. Con `POS_WEB_DIR=web` las plantillas se
leen del disco, así que los cambios en HTML se ven al recargar.

Antes de mandar tus cambios:

```bash
gofmt -l .          # no debe listar nada
go vet ./...
go test -race ./...
```

Si usaste clases de Tailwind nuevas en las plantillas, regenera el CSS con
`scripts/build-css.sh` (necesita Node) e incluye `web/static/css/app.css` en
tu cambio.

## Reglas del proyecto

Son pocas, pero importantes: protegen los datos y el dinero de negocios reales.

1. **Funciona sin internet.** Nada de CDNs, servicios externos ni
   telemetría en tiempo de ejecución. Las librerías de JavaScript van en
   `web/static/vendor/` y todo se embebe en el ejecutable.
2. **El dinero se calcula en centavos** (enteros), nunca con decimales
   flotantes. Revisa `internal/handler/money.go`.
3. **Nunca modifiques una migración que ya existe.** Los cambios a la base
   van en un archivo nuevo con el siguiente número en
   `internal/database/migrations/`. Las migraciones corren solas al
   arrancar sobre bases con datos reales: deben conservar los datos
   existentes.
4. **No se borra información del negocio.** Órdenes canceladas, productos
   anulados y registros archivados se conservan para el historial y la
   bitácora.
5. **El servidor valida todo.** Precios, permisos por terminal y totales se
   calculan o verifican en el servidor, aunque la pantalla ya lo haga.
6. **Español primero.** La interfaz, los mensajes y los comentarios del código
   están en español, pensando en negocios de México.

## Mandar tus cambios

1. Haz un fork y crea una rama desde `main`.
2. Haz cambios pequeños y enfocados: un pull request por tema.
3. Agrega o actualiza pruebas cuando cambies lógica (cobro, caja, impresión,
   permisos).
4. Llena la plantilla del pull request: qué cambia, por qué y cómo lo probaste.
   Si tocaste impresión, di con qué impresora probaste.

Al contribuir aceptas que tu código se publique bajo la misma licencia del
proyecto, [AGPL-3.0](LICENSE).

## Convivencia

Este proyecto sigue un [Código de conducta](CODE_OF_CONDUCT.md). En corto:
trata a los demás con respeto, sin importar su experiencia.
