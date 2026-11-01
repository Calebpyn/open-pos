#!/bin/sh
# Compila el lector de tickets (OCR de macOS) en bin/open-pos-ocr para
# desarrollo. El instalador (build-dmg.sh) lo mete dentro de la app.
set -e
cd "$(dirname "$0")/.."
mkdir -p bin
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
# Mismo arreglo que build-dmg.sh: algunas Command Line Tools traen un
# module.modulemap viejo que rompe los módulos de Apple; se oculta solo aquí.
swift_inc="$(dirname "$(dirname "$(xcrun --find swiftc)")")/include/swift"
overlay=""
if [ -f "$swift_inc/module.modulemap" ] && [ -f "$swift_inc/bridging.modulemap" ]; then
	: > "$work/empty.modulemap"
	printf '{"version":0,"case-sensitive":"false","roots":[{"type":"file","name":"%s","external-contents":"%s"}]}\n' \
		"$swift_inc/module.modulemap" "$work/empty.modulemap" > "$work/overlay.yaml"
	overlay="-vfsoverlay $work/overlay.yaml -Xcc -ivfsoverlay -Xcc $work/overlay.yaml"
fi
# shellcheck disable=SC2086
swiftc -swift-version 5 -O -target "$(uname -m)-apple-macos12" $overlay \
	packaging/macos/ocr/main.swift -o bin/open-pos-ocr
echo "bin/open-pos-ocr listo"
