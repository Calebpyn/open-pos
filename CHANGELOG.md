# Cambios

Aquí se anotan los cambios de cada versión. El formato se basa en
[Keep a Changelog](https://keepachangelog.com/es-ES/1.1.0/) y las versiones
siguen [versionado semántico](https://semver.org/lang/es/).

En cada versión se indica si trae **cambios a la base de datos**. Esas
versiones hacen un respaldo automático antes de aplicarlos, y conviene
instalarlas al cierre, sin mesas abiertas.

## [0.8.0-beta] - 2026-10-08

### Agregado
- **Compras:** la lista de compras del negocio, en lugar de la hoja de papel
  con "falta" y "urge". Botón 📝 en la pantalla del POS.
  - Caja (o un encargado) abre la lista; cada quien anota desde su terminal,
    con su PIN, lo que falta o urge, con una nota. Si varios piden lo mismo,
    se junta en un renglón.
  - Quien surte ve la lista por lugar (Costco, mercado, proveedor), con
    presupuesto estimado, otros lugares y sustitutos si no hay; va tachando y
    anotando lo que pagó, y el precio queda como referencia para la próxima.
    La lista se manda por WhatsApp con un botón.
  - Al cerrar la lista, lo que no se compró pasa solo a la siguiente, con
    quién lo pidió y hace cuántos días.
  - En **Administración → 🛒 Compras**: insumos, lugares, quién puede surtir
    desde su terminal e importación desde Excel (CSV). Los insumos se agrupan
    por dónde se guardan, categoría, dónde se compran o rango de precio, con
    filtros (sin lugar, sin precio, "hay más barato", con sustituto,
    archivados).
  - Cada insumo tiene sus lugares **en orden de prioridad** (▲▼), con una nota
    de por qué ("conviene por caja", "solo si no hay en Costco") y aviso de
    cuál es el más barato. Quien surte ve las alternativas en ese orden.
  - Cada lugar puede tener **su propia presentación** (Costco: burbuja de
    400 g; Fresh Market: de 200 g) o venderse **a granel** (precio por kg,
    cantidades como 1.5 kg). El contenido se lee solo del texto ("Burbuja
    400 g", "Caja 12 × 1 L") y el "más barato" se compara **por kilo, litro
    o pieza**, no por el precio del paquete.
  - **Tickets de compra con OCR:** quien surte toma foto del ticket desde su
    teléfono y se leen tienda, fecha, total y productos (con el OCR de macOS,
    sin internet). Se revisa, se relaciona cada renglón con su insumo y al
    registrarlo se tacha lo que estaba en la lista y se actualizan los precios
    (también por kg). Lo que se relaciona a mano se aprende para el siguiente
    ticket. Las fotos van en `compras-tickets/` junto a la base y se copian al
    respaldo diario. Pestaña 🧾 Tickets, con lo registrado en el mes por
    tienda.
- **Plugins:** programas aparte que se instalan sobre Open POS copiando su
  carpeta en `plugins/`. El POS los arranca, los reinicia si se caen y los
  apaga al cerrar; si un plugin falla, la venta sigue. Se enteran de lo que
  pasa (comandas, cobros, anulaciones, entregas, cambios al menú, cortes) por
  una bitácora de eventos que se escribe en la misma transacción que la venta,
  pueden tener su sección en el panel de administración y se respaldan junto
  con la base. Nueva sección **Administración → 🧩 Plugins**. Guía en
  [docs/plugins.md](docs/plugins.md), SDK para Go en `sdk/` y plugin de
  ejemplo en `examples/plugins/hola`.

**Cambios a la base de datos:** tablas nuevas `plugin_events`, `plugins` y
las de Compras (`compras_*`, incluidos los tickets).

## [0.7.2-beta] - 2026-09-28

Primera versión pública.

### Agregado
- Administración a distancia por Tailscale: desde una dirección de Tailscale
  solo se abre el panel de administración (con PIN de admin); el punto de
  venta, la caja y el cobro se bloquean. Las direcciones para conectarse
  aparecen en Administración → Configuración → Acceso remoto.
- Open POS se publica como software libre bajo la licencia
  [AGPL-3.0](LICENSE).
- El README aclara que Open POS sigue en beta y por ahora solo corre en Mac,
  e incluye la hoja de ruta (siguiente: versión para Windows).
- Política de seguridad ([SECURITY.md](SECURITY.md)), guía para contribuir
  ([CONTRIBUTING.md](CONTRIBUTING.md)), código de conducta, plantillas para
  reportar fallas y proponer mejoras, y pruebas automáticas en cada cambio.

Sin cambios a la base de datos.

## Versiones anteriores (betas privadas)

Hasta la 0.7 Open POS fue una beta privada, usada a diario en una cafetería.
Lo que ya incluye está descrito en la sección
[Qué hace](README.md#qué-hace) del README: mesas y plano del salón, comandas
por área de producción, monitor Expo, cobro con pagos mixtos y cuentas por
persona, turnos de caja con arqueo ciego, panel de administración con
reportes, terminales de mesero con PIN y respaldos automáticos.

[0.8.0-beta]: https://github.com/Calebpyn/open-pos/releases/tag/v0.8.0-beta
[0.7.2-beta]: https://github.com/Calebpyn/open-pos/releases/tag/v0.7.2-beta
