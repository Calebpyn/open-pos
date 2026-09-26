#!/bin/sh
# Regenera web/static/css/app.css con las clases de Tailwind que usan las
# plantillas. Correrlo después de usar clases nuevas (en modo desarrollo,
# POS_WEB_DIR=web, las páginas cargan Tailwind en el navegador y se ven bien
# aunque app.css no las tenga todavía).
#
# Requiere Node e internet la primera vez (npx descarga Tailwind 3).
set -e
cd "$(dirname "$0")/.."
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
printf '@tailwind base;\n@tailwind components;\n@tailwind utilities;\n' > "$tmp/in.css"
cat > "$tmp/tailwind.config.js" <<CFG
module.exports = {
  content: ['$PWD/web/templates/**/*.html', '$PWD/web/static/js/*.js', '$PWD/internal/**/*.go'],
}
CFG
{
  printf '/* Generado de las plantillas con Tailwind CSS 3 (MIT). No editar a mano:\n   regenerar con scripts/build-css.sh al usar clases nuevas. */\n'
  npx --yes tailwindcss@3 -c "$tmp/tailwind.config.js" -i "$tmp/in.css" --minify 2>/dev/null
} > "$tmp/app.css"
mv "$tmp/app.css" web/static/css/app.css
echo "web/static/css/app.css: $(wc -c < web/static/css/app.css) bytes"
