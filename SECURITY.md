# Seguridad

Open POS maneja el dinero de negocios reales, así que los problemas de
seguridad tienen prioridad sobre cualquier otra cosa.

## Cómo reportar una vulnerabilidad

**No abras un issue público.** Repórtala en privado desde la pestaña
**Security → Report a vulnerability** de este repositorio. Solo el
mantenedor la ve.

Incluye, si puedes:

- Qué versión usas (en la app de Mac: menú Open POS → Acerca de Open POS).
- Qué puede hacer alguien que aproveche el problema (ver datos, cobrar,
  cancelar órdenes, entrar al panel de administración…).
- Pasos para reproducirlo.

Qué puedes esperar:

- Respuesta en un plazo de **7 días**, aunque sea para confirmar que lo recibimos.
- Si se confirma, una versión corregida lo antes posible y un aviso en las
  notas de la versión. Te damos crédito si quieres.

Este es un proyecto mantenido por voluntarios: no hay recompensas económicas.

## Versiones con soporte

Solo la **última versión publicada** recibe correcciones de seguridad.
Actualizar no borra datos (ver el README), así que la recomendación siempre es
usar la más reciente.

## Cómo está pensado (modelo de seguridad)

Conocer estos supuestos ayuda a saber qué es un problema y qué no.

- **El servidor vive dentro del negocio.** Corre en una computadora del local
  y las terminales se conectan por la red Wi-Fi del negocio. **No está
  pensado para exponerse a internet** (no abras puertos del router hacia él).
- **La compu central tiene acceso completo.** Las peticiones que llegan desde
  la misma máquina (loopback) pueden cobrar, abrir la caja y entrar al admin.
- **Las demás terminales (iPad, teléfonos) entran como mesero con PIN.** El
  servidor bloquea cobro, caja y administración para ellas, salvo que el
  administrador le dé "Caja completa" a un dispositivo.
- **Administración remota solo por Tailscale.** Desde una dirección de
  Tailscale solo se abre el panel de administración, con PIN de admin.
- **Los datos se quedan en el local.** No hay servicios externos ni
  telemetría. La base de datos (SQLite) y los respaldos están en el disco de
  la computadora del negocio, sin cifrar: protege el acceso a esa computadora.

Por ejemplo, **sí es un problema de seguridad**: que un iPad con PIN de mesero
pueda cobrar o entrar al admin, que se pueda saltar el PIN, o que desde
Tailscale se pueda usar el punto de venta. **No lo es**: que alguien con
acceso físico a la computadora del servidor pueda leer la base de datos.

## Recomendaciones para el negocio

- **Cambia el PIN inicial `0001`** en Administración → Usuarios en cuanto
  instales, y usa un PIN distinto para cada persona.
- Usa una contraseña en la red Wi-Fi del negocio y, si puedes, una red aparte
  para clientes.
- Copia un respaldo fuera del local de vez en cuando (USB o nube).
- Mantén Open POS actualizado.
