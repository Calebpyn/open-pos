// open-pos-ocr: lee el texto de una foto (ticket de compra) con el OCR de
// macOS (Vision), sin internet. Uso: open-pos-ocr <imagen>
//
// Escribe JSON en stdout: {"lines": [{"text", "x", "y", "w", "h", "conf"}]}
// con coordenadas de 0 a 1 desde la esquina superior izquierda. Open POS
// junta los textos que van en el mismo renglón del ticket.
import Foundation
import ImageIO
import Vision

struct Line: Codable {
    let text: String
    let x: Double, y: Double, w: Double, h: Double
    let conf: Double
}

struct Output: Codable { let lines: [Line] }

func fail(_ msg: String) -> Never {
    FileHandle.standardError.write((msg + "\n").data(using: .utf8)!)
    exit(1)
}

guard CommandLine.arguments.count == 2 else { fail("uso: open-pos-ocr <imagen>") }
let url = URL(fileURLWithPath: CommandLine.arguments[1])
guard let source = CGImageSourceCreateWithURL(url as CFURL, nil),
      let image = CGImageSourceCreateImageAtIndex(source, 0, nil) else {
    fail("no se pudo abrir la imagen")
}

// Las fotos del teléfono traen la orientación en EXIF.
var orientation = CGImagePropertyOrientation.up
if let props = CGImageSourceCopyPropertiesAtIndex(source, 0, nil) as? [CFString: Any],
   let raw = props[kCGImagePropertyOrientation] as? UInt32,
   let o = CGImagePropertyOrientation(rawValue: raw) {
    orientation = o
}

let request = VNRecognizeTextRequest()
request.recognitionLevel = .accurate
request.usesLanguageCorrection = true
if let supported = try? request.supportedRecognitionLanguages() {
    let wanted = ["es-MX", "es-ES", "en-US"].filter { supported.contains($0) }
    if !wanted.isEmpty { request.recognitionLanguages = wanted }
}

let handler = VNImageRequestHandler(cgImage: image, orientation: orientation, options: [:])
do {
    try handler.perform([request])
} catch {
    fail("OCR: \(error.localizedDescription)")
}

var lines: [Line] = []
for obs in request.results ?? [] {
    guard let best = obs.topCandidates(1).first else { continue }
    let b = obs.boundingBox // origen abajo a la izquierda
    lines.append(Line(text: best.string, x: Double(b.minX), y: Double(1 - b.maxY),
                      w: Double(b.width), h: Double(b.height), conf: Double(best.confidence)))
}
lines.sort { $0.y == $1.y ? $0.x < $1.x : $0.y < $1.y }

let enc = JSONEncoder()
FileHandle.standardOutput.write(try! enc.encode(Output(lines: lines)))
