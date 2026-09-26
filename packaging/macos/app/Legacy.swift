import Cocoa

// Actualizar desde la beta 0.1/0.2: aquellas versiones dejaban el servidor
// corriendo por fuera de la app (como servicio de launchd o en una ventana de
// Terminal) y, según cómo se abriera, la base podía quedar en otra carpeta
// (p. ej. ~/pos.db). Al abrir esta versión se apagan esos servidores y se
// trae la base correcta a la carpeta de datos, sin borrar nunca ninguna.

/// Resumen de una base para decidir cuál es la del negocio.
struct DBSummary {
    let path: String
    let products: Int
    let tables: Int
    let orders: Int
    let bills: Int
    let lastSale: String
    let modified: Date?

    var isEmpty: Bool { orders == 0 && bills == 0 }

    var description: String {
        var s = "\(products) productos, \(tables) mesas, \(orders) órdenes, \(bills) cobros"
        if !lastSale.isEmpty { s += " (último cobro: \(lastSale))" }
        if let modified {
            let f = DateFormatter()
            f.dateStyle = .medium
            f.timeStyle = .short
            f.locale = Locale(identifier: "es_MX")
            s += "\nModificada: \(f.string(from: modified))"
        }
        return s
    }
}

@discardableResult
func run(_ path: String, _ args: [String]) -> (Int32, String) {
    let p = Process()
    p.executableURL = URL(fileURLWithPath: path)
    p.arguments = args
    let out = Pipe()
    p.standardOutput = out
    p.standardError = FileHandle.nullDevice
    do { try p.run() } catch { return (-1, "") }
    let data = out.fileHandleForReading.readDataToEndOfFile()
    p.waitUntilExit()
    return (p.terminationStatus, String(data: data, encoding: .utf8) ?? "")
}

extension Server {
    var targetDB: String { dataDir.appendingPathComponent("pos.db").path }

    /// Servidores de Open POS que no son de esta app (versión anterior abierta
    /// en Terminal, o una copia que quedó huérfana).
    func strayServers() -> [pid_t] {
        // Por nombre de proceso y luego por la ruta de su ejecutable (no por
        // la línea de comandos: una terminal que mencione la ruta no cuenta).
        let pattern = ProcessInfo.processInfo.environment["OPEN_POS_STRAY_PATTERN"] ?? "Open POS.app/Contents/Resources/open-pos"
        let (_, out) = run("/usr/bin/pgrep", ["-x", "open-pos"])
        let mine = process?.processIdentifier
        return out.split(separator: "\n").compactMap { pid_t($0.trimmingCharacters(in: .whitespaces)) }
            .filter { pid in
                guard pid != mine, pid != getpid() else { return false }
                let (_, exe) = run("/bin/ps", ["-o", "comm=", "-p", String(pid)])
                return exe.trimmingCharacters(in: .whitespacesAndNewlines).hasSuffix(pattern)
            }
    }

    /// La base que tiene abierta un proceso.
    func databaseOf(_ pid: pid_t) -> String? {
        let (_, out) = run("/usr/sbin/lsof", ["-p", String(pid), "-Fn"])
        return out.split(separator: "\n")
            .filter { $0.hasPrefix("n") }
            .map { String($0.dropFirst()) }
            .first { $0.hasSuffix(".db") && !$0.contains("/respaldos/") }
    }

    /// Apaga en orden los servidores viejos y regresa las bases que usaban.
    func stopStrayServers() -> [String] {
        var dbs: [String] = []
        for pid in strayServers() {
            if let db = databaseOf(pid) { dbs.append(db) }
            kill(pid, SIGTERM) // guarda y cierra la base limpiamente
            let deadline = Date().addingTimeInterval(20)
            while kill(pid, 0) == 0 && Date() < deadline { usleep(200_000) }
            if kill(pid, 0) == 0 { kill(pid, SIGKILL) }
        }
        let deadline = Date().addingTimeInterval(10)
        while portInUse() && Date() < deadline { usleep(200_000) }
        return dbs
    }

    func summary(of path: String) -> DBSummary? {
        guard FileManager.default.fileExists(atPath: path) else { return nil }
        let (code, out) = run("/usr/bin/sqlite3", ["-separator", "|", path, """
            SELECT (SELECT COUNT(*) FROM products WHERE COALESCE(is_active, 1) = 1),
                   (SELECT COUNT(*) FROM dining_tables WHERE COALESCE(is_active, 1) = 1),
                   (SELECT COUNT(*) FROM orders),
                   (SELECT COUNT(*) FROM bills WHERE status = 'PAID'),
                   COALESCE((SELECT strftime('%d/%m/%Y', MAX(paid_at), 'localtime') FROM bills WHERE status = 'PAID'), '');
            """])
        let f = out.trimmingCharacters(in: .whitespacesAndNewlines).split(separator: "|", omittingEmptySubsequences: false)
        guard code == 0, f.count == 5 else { return nil }
        let attrs = try? FileManager.default.attributesOfItem(atPath: path)
        return DBSummary(path: path, products: Int(f[0]) ?? 0, tables: Int(f[1]) ?? 0, orders: Int(f[2]) ?? 0,
                         bills: Int(f[3]) ?? 0, lastSale: String(f[4]), modified: attrs?[.modificationDate] as? Date)
    }

    /// Bases de versiones anteriores que no están en la carpeta de datos.
    func legacyCandidates(running: [String]) -> [DBSummary] {
        let home = homeDir.path
        var paths = running
        paths.append(home + "/pos.db") // servidor abierto en Terminal desde la carpeta de usuario
        let target = URL(fileURLWithPath: targetDB).resolvingSymlinksInPath().path
        var seen = Set<String>()
        var out: [DBSummary] = []
        for p in paths {
            let std = URL(fileURLWithPath: p).resolvingSymlinksInPath().path
            guard std != target, !seen.contains(std), !checkedLegacy.contains(std) else { continue }
            seen.insert(std)
            if let s = summary(of: std) { out.append(s) }
        }
        return out
    }

    /// Bases que ya se revisaron (importadas o descartadas): no se vuelve a preguntar.
    private var checkedURL: URL { dataDir.appendingPathComponent(".bases-revisadas") }
    var checkedLegacy: Set<String> {
        let text = (try? String(contentsOf: checkedURL, encoding: .utf8)) ?? ""
        return Set(text.split(separator: "\n").map(String.init))
    }
    func markChecked(_ paths: [String]) {
        try? FileManager.default.createDirectory(at: dataDir, withIntermediateDirectories: true)
        let all = checkedLegacy.union(paths)
        try? all.sorted().joined(separator: "\n").write(to: checkedURL, atomically: true, encoding: .utf8)
    }

    /// Trae la base elegida a la carpeta de datos. La que estaba ahí no se
    /// borra: se guarda en respaldos.
    func importDatabase(from source: String) throws {
        let fm = FileManager.default
        try fm.createDirectory(at: dataDir, withIntermediateDirectories: true)
        let backups = dataDir.appendingPathComponent("respaldos")
        try fm.createDirectory(at: backups, withIntermediateDirectories: true)
        let stamp: String = {
            let f = DateFormatter()
            f.dateFormat = "yyyyMMdd-HHmmss"
            return f.string(from: Date())
        }()
        if fm.fileExists(atPath: targetDB) {
            let saved = backups.appendingPathComponent("pos-\(stamp).db").path
            let (code, _) = run("/usr/bin/sqlite3", [targetDB, ".backup '\(saved)'"])
            guard code == 0 else { throw importError("No se pudo guardar la base actual antes de importar.") }
            for suffix in ["", "-wal", "-shm"] { try? fm.removeItem(atPath: targetDB + suffix) }
        }
        // .backup copia la base completa aunque tenga cambios en su -wal.
        let (code, _) = run("/usr/bin/sqlite3", [source, ".backup '\(targetDB)'"])
        guard code == 0, fm.fileExists(atPath: targetDB) else {
            throw importError("No se pudo copiar la base de \(source).")
        }
    }

    private func importError(_ msg: String) -> NSError {
        NSError(domain: "OpenPOS", code: 2, userInfo: [NSLocalizedDescriptionKey: msg])
    }
}

extension AppDelegate {
    /// Se corre antes de arrancar el servidor. Llama a done en el hilo principal.
    func migrateFromOldVersions(done: @escaping () -> Void) {
        // En pruebas (carpeta de datos aparte) solo se revisa si además se
        // da una carpeta de usuario ficticia.
        guard !server.isolated || ProcessInfo.processInfo.environment["OPEN_POS_HOME"] != nil else { return done() }
        DispatchQueue.global().async {
            self.server.removeLegacyService()
            let running = self.server.stopStrayServers()
            let candidates = self.server.legacyCandidates(running: running)
            let current = self.server.summary(of: self.server.targetDB)
            DispatchQueue.main.async {
                self.chooseDatabase(current: current, candidates: candidates, done: done)
            }
        }
    }

    private func chooseDatabase(current: DBSummary?, candidates: [DBSummary], done: @escaping () -> Void) {
        let useful = candidates.filter { !$0.isEmpty || $0.products > 0 }
        guard !useful.isEmpty else {
            server.markChecked(candidates.map(\.path))
            return done()
        }
        var pick: DBSummary?
        if current == nil, useful.count == 1 {
            // Solo hay una base con datos, fuera de su lugar: se trae sin preguntar.
            pick = useful[0]
        } else {
            let a = NSAlert()
            a.messageText = "Encontramos datos de una versión anterior de Open POS"
            var text = "Elige qué base usar. La otra no se borra: se queda guardada.\n\n"
            if let current {
                text += "• Actual (\(current.path.replacingOccurrences(of: homeDir.path, with: "~"))):\n\(current.description)\n\n"
            }
            for c in useful {
                text += "• \(c.path.replacingOccurrences(of: homeDir.path, with: "~")):\n\(c.description)\n\n"
            }
            a.informativeText = text
            for c in useful {
                a.addButton(withTitle: "Usar " + c.path.replacingOccurrences(of: homeDir.path, with: "~"))
            }
            if current != nil { a.addButton(withTitle: "Seguir con la actual") }
            let resp = a.runModal()
            let idx = resp.rawValue - NSApplication.ModalResponse.alertFirstButtonReturn.rawValue
            if idx >= 0 && idx < useful.count { pick = useful[idx] }
        }
        server.markChecked(candidates.map(\.path))
        guard let pick else { return done() }
        loading.show(message: "Importando tus datos…")
        DispatchQueue.global().async {
            do {
                try self.server.importDatabase(from: pick.path)
                DispatchQueue.main.async { done() }
            } catch {
                DispatchQueue.main.async {
                    showAlert(in: self.main.window, title: "No se pudieron importar los datos",
                              text: error.localizedDescription + "\n\nTus datos siguen en \(pick.path).")
                    done()
                }
            }
        }
    }
}
