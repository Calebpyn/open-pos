# Hola mundo

Plugin de ejemplo para Open POS. Prueba todo el recorrido de un plugin:

- lee los eventos del POS (cuentas cobradas) y guarda sus números en su
  propia SQLite, con el cursor en la misma transacción;
- muestra una página dentro del panel de administración;
- lee el catálogo y marca productos agotados o disponibles;
- respalda su base con cada respaldo del POS.

La guía completa está en [docs/plugins.md](../../../docs/plugins.md).

## Probarlo en desarrollo

Desde la raíz del repositorio:

```bash
POS_PLUGIN_DEV=$PWD/examples/plugins/hola go run ./cmd/pos
```

Y en otra terminal:

```bash
cd examples/plugins/hola && go run .
```

Entra a **Administración → 👋 Hola**.

## Instalarlo

```bash
cd examples/plugins/hola
go build -ldflags "-s -w" -o dist/hola/hola . && cp plugin.json dist/hola/
```

Copia `dist/hola` a la carpeta `plugins/` junto a la base de datos y reinicia
Open POS.
