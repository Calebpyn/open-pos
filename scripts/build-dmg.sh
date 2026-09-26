#!/bin/sh
# Arma dist/OpenPOS-<versión>.dmg con "Open POS.app" para instalar en la Mac
# de la cafetería. Uso: scripts/build-dmg.sh [versión]   (p. ej. 0.1.0-beta)
#
# La app (Swift, packaging/macos/app) abre su propia ventana y corre dentro
# el servidor Go; ambos se compilan universales (Apple Silicon e Intel).
set -e
cd "$(dirname "$0")/.."
VERSION="${1:-0.1.0-beta}"
PKG=packaging/macos

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
stage="$work/dmg"
app="$stage/Open POS.app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources" dist

echo "Compilando servidor $VERSION..."
for arch in arm64 amd64; do
	CGO_ENABLED=0 GOOS=darwin GOARCH=$arch go build -trimpath \
		-ldflags="-s -w -X main.version=$VERSION" -o "$work/open-pos-$arch" ./cmd/pos
done
lipo -create -output "$app/Contents/Resources/open-pos" "$work/open-pos-arm64" "$work/open-pos-amd64"

echo "Compilando app de Mac..."
# Algunas instalaciones de Command Line Tools traen un module.modulemap viejo
# junto al nuevo bridging.modulemap y ningún módulo de Apple compila; se oculta
# el viejo solo durante la compilación (sin tocar el sistema).
swift_inc="$(dirname "$(dirname "$(xcrun --find swiftc)")")/include/swift"
overlay=""
if [ -f "$swift_inc/module.modulemap" ] && [ -f "$swift_inc/bridging.modulemap" ]; then
	: > "$work/empty.modulemap"
	printf '{"version":0,"case-sensitive":"false","roots":[{"type":"file","name":"%s","external-contents":"%s"}]}\n' \
		"$swift_inc/module.modulemap" "$work/empty.modulemap" > "$work/overlay.yaml"
	overlay="-vfsoverlay $work/overlay.yaml -Xcc -ivfsoverlay -Xcc $work/overlay.yaml"
fi
for arch in arm64 x86_64; do
	# shellcheck disable=SC2086
	swiftc -swift-version 5 -O -target $arch-apple-macos12 $overlay \
		-o "$work/OpenPOS-$arch" "$PKG"/app/*.swift
done
lipo -create -output "$app/Contents/MacOS/OpenPOS" "$work/OpenPOS-arm64" "$work/OpenPOS-x86_64"
printf '%s\n' "$VERSION" > "$app/Contents/Resources/version.txt"
sed "s/__VERSION__/$VERSION/g" "$PKG/Info.plist" > "$app/Contents/Info.plist"

echo "Generando ícono..."
iconset="$work/AppIcon.iconset"
mkdir -p "$iconset"
for s in 16 32 128 256 512; do
	sips -z $s $s "$PKG/AppIcon.png" --out "$iconset/icon_${s}x${s}.png" >/dev/null
	sips -z $((s * 2)) $((s * 2)) "$PKG/AppIcon.png" --out "$iconset/icon_${s}x${s}@2x.png" >/dev/null
done
iconutil -c icns "$iconset" -o "$app/Contents/Resources/AppIcon.icns"

# Firma local (ad-hoc): Apple Silicon no ejecuta binarios sin firma. No evita
# el aviso de Gatekeeper; para eso haría falta una cuenta de Apple Developer.
codesign --force -s - "$app/Contents/Resources/open-pos"
codesign --force -s - "$app"

sed "s/__VERSION__/$VERSION/g" "$PKG/Léeme.txt" > "$stage/Léeme.txt"
ln -s /Applications "$stage/Aplicaciones"

out="dist/OpenPOS-$VERSION.dmg"
rm -f "$out"
hdiutil create -quiet -volname "Open POS $VERSION" -srcfolder "$stage" -fs HFS+ -format UDZO "$out"
echo "Listo: $out ($(du -h "$out" | cut -f1))"
