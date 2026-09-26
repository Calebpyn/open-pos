package printer

import "time"

// --- Ritmo de la impresora ---
//
// CUPS (y un socket TCP) dan por terminado un trabajo en cuanto entregan los
// bytes, pero la impresora tarda segundos más en imprimirlos. Lo que llega
// mientras sigue imprimiendo, algunas térmicas lo pierden. Con Pacing, antes
// del siguiente trabajo la cola espera lo que tarda en salir el papel del
// anterior.

// mmPerSecond es la velocidad de impresión que se supone. Las de 58 mm
// económicas dicen imprimir a 50-90 mm/s, pero con letra grande o logo van
// más lento: con 40 mm/s el pulso del cajón todavía llegaba con la impresora
// ocupada. Se usa un valor bajo para no quedarse cortos.
var mmPerSecond = 40.0

// Pacing está apagado: esperar a que "saliera el papel" no resolvió los
// tickets sin encabezado y retrasaba todo. Se deja la estimación por si se
// vuelve a necesitar.
var Pacing = false

const (
	dotsPerMM      = 8  // 203 dpi
	defaultSpacing = 30 // ESC 2: 1/6 de pulgada
	bandOverhead   = 40 * time.Millisecond
	jobOverhead    = time.Second // arranque del motor, avance final y margen
	maxPrintTime   = 20 * time.Second
)

// PrintDuration estima cuánto tarda la impresora en sacar un trabajo ESC/POS:
// suma el avance de papel de cada salto de línea (según el interlineado y el
// alto de letra vigentes), los avances explícitos y las franjas de imagen.
func PrintDuration(data []byte) time.Duration {
	spacing, height := defaultSpacing, 1
	dots, bands := 0, 0
	band := false // el siguiente salto de línea cierra una franja de imagen
	for i := 0; i < len(data); i++ {
		b := data[i]
		switch b {
		case '\n':
			if band {
				dots += spacing
				bands++
				band = false
			} else {
				dots += spacing * height
			}
		case 0x1b: // ESC
			if i+1 >= len(data) {
				break
			}
			cmd := data[i+1]
			i++
			switch cmd {
			case '2':
				spacing = defaultSpacing
			case '3':
				if i+1 < len(data) {
					spacing = int(data[i+1])
				}
				i++
			case 'd': // avance de n líneas
				if i+1 < len(data) {
					dots += int(data[i+1]) * spacing
				}
				i++
			case 'J': // avance de n puntos
				if i+1 < len(data) {
					dots += int(data[i+1])
				}
				i++
			case '*': // imagen: m nL nH y los datos de la franja
				if i+3 < len(data) {
					n := int(data[i+2]) | int(data[i+3])<<8
					per := 1
					if data[i+1] >= 32 {
						per = 3
					}
					i += 3 + n*per
					band = true
				}
			case 'p': // cajón: m t1 t2
				i += 3
			case '@':
			default: // t, a, E, !, -, M, G, R...: un parámetro
				i++
			}
		case 0x1d: // GS
			if i+1 >= len(data) {
				break
			}
			cmd := data[i+1]
			i++
			switch cmd {
			case '!':
				if i+1 < len(data) {
					height = int(data[i+1]&0x0f) + 1
				}
				i++
			case 'V':
				if i+1 < len(data) && (data[i+1] == 65 || data[i+1] == 66) {
					i++
				}
				i++
			case 'L', 'W':
				i += 2
			default:
				i++
			}
		}
	}
	if dots == 0 && bands == 0 {
		return 0
	}
	mm := float64(dots) / dotsPerMM
	d := jobOverhead + time.Duration(mm/mmPerSecond*float64(time.Second)) + time.Duration(bands)*bandOverhead
	return min(d, maxPrintTime)
}
