import Foundation

/// Arranca y detiene el servidor de Open POS (el binario Go que va dentro de
/// la app). El servidor vive exactamente lo que vive la app: si la app se
/// cierra de golpe, el servidor lo detecta y se apaga solo (OPEN_POS_APP=1).
final class Server {
    let port: Int
    let dataDir: URL
    let logURL: URL
    private(set) var process: Process?
    private var stopping = false

    /// Se llama (en el hilo principal) si el servidor se cae sin que se lo
    /// hayamos pedido.
    var onUnexpectedExit: ((Int32) -> Void)?

    var baseURL: URL { URL(string: "http://127.0.0.1:\(port)/")! }
    var isRunning: Bool { process?.isRunning ?? false }

    /// OPEN_POS_DATA_DIR: otra carpeta de datos (para pruebas); en ese modo
    /// no se toca el servicio de la beta instalada.
    let isolated: Bool

    init(port: Int) {
        self.port = port
        let home = homeDir
        if let dir = ProcessInfo.processInfo.environment["OPEN_POS_DATA_DIR"], !dir.isEmpty {
            isolated = true
            dataDir = URL(fileURLWithPath: dir, isDirectory: true)
            logURL = dataDir.appendingPathComponent("servidor.log")
        } else {
            isolated = false
            dataDir = home.appendingPathComponent("Library/Application Support/Open POS", isDirectory: true)
            logURL = home.appendingPathComponent("Library/Logs/Open POS/servidor.log")
        }
    }

    func start() throws {
        let fm = FileManager.default
        try fm.createDirectory(at: dataDir, withIntermediateDirectories: true)
        try fm.createDirectory(at: logURL.deletingLastPathComponent(), withIntermediateDirectories: true)
        if !fm.fileExists(atPath: logURL.path) {
            fm.createFile(atPath: logURL.path, contents: nil)
        }
        guard let bin = Bundle.main.url(forResource: "open-pos", withExtension: nil) else {
            throw NSError(domain: "OpenPOS", code: 1, userInfo: [NSLocalizedDescriptionKey: "No se encontró el servidor dentro de la app."])
        }
        let log = try FileHandle(forWritingTo: logURL)
        log.seekToEndOfFile()

        let p = Process()
        p.executableURL = bin
        p.currentDirectoryURL = dataDir
        p.environment = [
            "POS_DB": dataDir.appendingPathComponent("pos.db").path,
            "PORT": String(port),
            "PATH": "/usr/bin:/bin:/usr/sbin:/sbin",
            "HOME": FileManager.default.homeDirectoryForCurrentUser.path,
            "OPEN_POS_APP": "1",
        ]
        p.standardOutput = log
        p.standardError = log
        p.terminationHandler = { [weak self] proc in
            try? log.close()
            DispatchQueue.main.async {
                guard let self, self.process === proc else { return }
                self.process = nil
                if !self.stopping { self.onUnexpectedExit?(proc.terminationStatus) }
            }
        }
        stopping = false
        try p.run()
        process = p
    }

    /// Pide al servidor que termine lo que está haciendo y se apague; si no
    /// lo hace en 20 s, se detiene a la fuerza.
    func stop(completion: @escaping () -> Void) {
        guard let p = process, p.isRunning else {
            completion()
            return
        }
        stopping = true
        p.terminate() // SIGTERM: apagado ordenado, cierra la base limpiamente
        DispatchQueue.global().async {
            let deadline = Date().addingTimeInterval(20)
            while p.isRunning && Date() < deadline { usleep(100_000) }
            if p.isRunning { kill(p.processIdentifier, SIGKILL) }
            DispatchQueue.main.async { completion() }
        }
    }

    /// Espera a que el servidor responda.
    func waitUntilReady(timeout: TimeInterval = 40, completion: @escaping (Bool) -> Void) {
        let deadline = Date().addingTimeInterval(timeout)
        func attempt() {
            var req = URLRequest(url: baseURL.appendingPathComponent("static/css/app.css"))
            req.timeoutInterval = 1
            URLSession.shared.dataTask(with: req) { _, resp, _ in
                DispatchQueue.main.async {
                    if (resp as? HTTPURLResponse)?.statusCode == 200 {
                        completion(true)
                    } else if Date() > deadline || !(self.process?.isRunning ?? false) {
                        completion(false)
                    } else {
                        DispatchQueue.main.asyncAfter(deadline: .now() + 0.3, execute: attempt)
                    }
                }
            }.resume()
        }
        attempt()
    }

    struct Status: Decodable {
        let open_tables: Int
        let active_orders: Int
        let terminals: [String]
    }

    /// Qué se interrumpiría al cerrar (mesas abiertas, terminales conectadas).
    func status(completion: @escaping (Status?) -> Void) {
        var req = URLRequest(url: baseURL.appendingPathComponent("api/app/status"))
        req.timeoutInterval = 2
        URLSession.shared.dataTask(with: req) { data, _, _ in
            let st = data.flatMap { try? JSONDecoder().decode(Status.self, from: $0) }
            DispatchQueue.main.async { completion(st) }
        }.resume()
    }

    /// Últimas líneas del registro, para mostrar si algo falla.
    func logTail(lines: Int = 6) -> String {
        guard let text = try? String(contentsOf: logURL, encoding: .utf8) else { return "" }
        return text.split(separator: "\n").suffix(lines).joined(separator: "\n")
    }

    /// ¿Hay algo escuchando ya en el puerto?
    func portInUse() -> Bool {
        let fd = socket(AF_INET, SOCK_STREAM, 0)
        guard fd >= 0 else { return false }
        defer { close(fd) }
        var addr = sockaddr_in()
        addr.sin_family = sa_family_t(AF_INET)
        addr.sin_port = in_port_t(UInt16(port).bigEndian)
        addr.sin_addr.s_addr = inet_addr("127.0.0.1")
        return withUnsafePointer(to: &addr) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                connect(fd, $0, socklen_t(MemoryLayout<sockaddr_in>.size)) == 0
            }
        }
    }

    /// La beta 0.1/0.2 instalaba el servidor como servicio de launchd que
    /// quedaba corriendo por detrás. Se retira para que no ocupe el puerto ni
    /// vuelva a arrancar solo.
    func removeLegacyService() {
        if isolated { return }
        let label = "com.openpos.server"
        let plist = homeDir
            .appendingPathComponent("Library/LaunchAgents/\(label).plist")
        guard FileManager.default.fileExists(atPath: plist.path) else { return }
        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/bin/launchctl")
        p.arguments = ["bootout", "gui/\(getuid())/\(label)"]
        p.standardError = FileHandle.nullDevice
        try? p.run()
        p.waitUntilExit()
        try? FileManager.default.removeItem(at: plist)
        try? FileManager.default.removeItem(at: dataDir.appendingPathComponent(".version"))
        // Esperar a que suelte el puerto.
        let deadline = Date().addingTimeInterval(20)
        while portInUse() && Date() < deadline { usleep(200_000) }
    }
}

/// Carpeta del usuario. OPEN_POS_HOME la cambia solo para pruebas.
let homeDir: URL = {
    if let h = ProcessInfo.processInfo.environment["OPEN_POS_HOME"], !h.isEmpty {
        return URL(fileURLWithPath: h, isDirectory: true)
    }
    return FileManager.default.homeDirectoryForCurrentUser
}()

/// IP de esta Mac en la red local (para conectar el iPad).
func localIPAddress() -> String? {
    var ifaddr: UnsafeMutablePointer<ifaddrs>?
    guard getifaddrs(&ifaddr) == 0, let first = ifaddr else { return nil }
    defer { freeifaddrs(ifaddr) }
    var best: String?
    for ptr in sequence(first: first, next: { $0.pointee.ifa_next }) {
        let ifa = ptr.pointee
        guard let sa = ifa.ifa_addr, sa.pointee.sa_family == UInt8(AF_INET) else { continue }
        let name = String(cString: ifa.ifa_name)
        guard name.hasPrefix("en") else { continue }
        var host = [CChar](repeating: 0, count: Int(NI_MAXHOST))
        if getnameinfo(sa, socklen_t(sa.pointee.sa_len), &host, socklen_t(host.count), nil, 0, NI_NUMERICHOST) == 0 {
            let ip = String(cString: host)
            if name == "en0" { return ip }
            best = best ?? ip
        }
    }
    return best
}
