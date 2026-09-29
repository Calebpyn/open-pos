# Cambios

Aquí se anotan los cambios de cada versión. El formato se basa en
[Keep a Changelog](https://keepachangelog.com/es-ES/1.1.0/) y las versiones
siguen [versionado semántico](https://semver.org/lang/es/).

En cada versión se indica si trae **cambios a la base de datos**. Esas
versiones hacen un respaldo automático antes de aplicarlos, y conviene
instalarlas al cierre, sin mesas abiertas.

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

[0.7.2-beta]: https://github.com/Calebpyn/open-pos/releases/tag/v0.7.2-beta
