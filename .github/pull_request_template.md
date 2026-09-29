## Qué cambia

<!-- Describe el cambio y por qué hace falta. Si resuelve un issue: "Resuelve #123". -->

## Cómo lo probaste

<!-- Pasos que seguiste. Si tocaste impresión, di con qué impresora (modelo, 58/80 mm, USB o red). -->

## Revisión

- [ ] `gofmt -l .` no lista nada, y `go vet ./...` y `go test -race ./...` pasan
- [ ] Agregué o actualicé pruebas si cambió lógica de cobro, caja, impresión o permisos
- [ ] Los cambios a la base van en una migración **nueva** (no modifiqué una existente)
- [ ] No agregué dependencias de internet en tiempo de ejecución (CDNs, APIs externas)
- [ ] Si usé clases de Tailwind nuevas, regeneré `web/static/css/app.css`
