import Cocoa
import ServiceManagement
@preconcurrency import WebKit

// Open POS para Mac: una app con su propia ventana. El servidor corre dentro
// de la app mientras está abierta (el iPad y las demás terminales se conectan
// a él) y se apaga al salir.

let appVersion = Bundle.main.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String ?? "dev"
let serverPort = Int(ProcessInfo.processInfo.environment["OPEN_POS_PORT"] ?? "") ?? 8080

final class AppDelegate: NSObject, NSApplicationDelegate {
    let server = Server(port: serverPort)
    var main: WebWindow!
    var loading: LoadingView!
    private var quitConfirmed = false
    private var poweringOff = false
    private var restarts = 0

    func applicationDidFinishLaunching(_ notification: Notification) {
        buildMenu()
        if offerInstallFromDiskImage() { return }

        main = WebWindow(origin: server.baseURL, isMain: true, frame: NSRect(x: 0, y: 0, width: 1280, height: 820))
        main.onCloseRequest = { NSApp.terminate(nil) }
        if !main.window.setFrameUsingName("OpenPOSMain") { main.window.center() }

        loading = LoadingView(frame: main.webView.bounds)
        loading.autoresizingMask = [.width, .height]
        loading.onRetry = { [weak self] in self?.startServer() }
        loading.onShowLog = { [weak self] in self?.showLog() }
        main.webView.addSubview(loading)
        main.window.makeKeyAndOrderFront(nil)
        NSApp.activate(ignoringOtherApps: true)

        NSWorkspace.shared.notificationCenter.addObserver(
            forName: NSWorkspace.willPowerOffNotification, object: nil, queue: .main) { [weak self] _ in
            self?.poweringOff = true
        }
        server.onUnexpectedExit = { [weak self] code in self?.serverDied(code) }

        loading.show(message: "Iniciando Open POS…")
        migrateFromOldVersions { [weak self] in self?.startServer() }
    }

    // MARK: Servidor

    func startServer() {
        loading.show(message: "Iniciando Open POS…")
        if server.portInUse() {
            loading.fail(
                title: "El puerto \(server.port) está ocupado",
                detail: "Otro programa (¿otra copia de Open POS?) ya usa el puerto \(server.port). Ciérralo y reintenta.")
            return
        }
        do {
            try server.start()
        } catch {
            loading.fail(title: "No se pudo iniciar el servidor", detail: error.localizedDescription)
            return
        }
        server.waitUntilReady { [weak self] ok in
            guard let self else { return }
            if ok {
                self.loading.hide()
                self.main.load(self.server.baseURL)
            } else {
                self.loading.fail(title: "El servidor no respondió", detail: self.server.logTail())
            }
        }
    }

    // Si el servidor se cae con la app abierta, se levanta de nuevo (hasta 3
    // veces seguidas) y la página se recarga sola.
    func serverDied(_ code: Int32) {
        restarts += 1
        guard restarts <= 3 else {
            loading.fail(title: "El servidor se detuvo", detail: server.logTail())
            return
        }
        DispatchQueue.main.asyncAfter(deadline: .now() + 1) { [weak self] in
            self?.startServer()
            DispatchQueue.main.asyncAfter(deadline: .now() + 60) { self?.restarts = 0 }
        }
    }

    // MARK: Salir

    func applicationShouldTerminate(_ sender: NSApplication) -> NSApplication.TerminateReply {
        guard server.isRunning else { return .terminateNow }
        if quitConfirmed || poweringOff {
            shutDown()
            return .terminateLater
        }
        server.status { [weak self] st in
            guard let self else { return }
            guard let st, st.open_tables > 0 || !st.terminals.isEmpty else {
                self.shutDown()
                return
            }
            self.confirmQuit(st)
        }
        return .terminateLater
    }

    private func confirmQuit(_ st: Server.Status) {
        var parts: [String] = []
        if st.open_tables > 0 {
            parts.append(st.open_tables == 1 ? "hay 1 mesa abierta" : "hay \(st.open_tables) mesas abiertas")
        }
        if !st.terminals.isEmpty {
            let n = st.terminals.count
            parts.append((n == 1 ? "1 terminal conectada" : "\(n) terminales conectadas") + " (\(st.terminals.joined(separator: ", ")))")
        }
        let a = NSAlert()
        a.alertStyle = .warning
        a.messageText = "¿Cerrar Open POS?"
        a.informativeText = "Ahora " + parts.joined(separator: " y ") + ". " +
            "Si cierras, el iPad y las demás terminales dejarán de funcionar hasta que vuelvas a abrir Open POS. " +
            "Las mesas y órdenes no se pierden."
        a.addButton(withTitle: "Cancelar")
        a.addButton(withTitle: "Cerrar Open POS")
        a.buttons[1].hasDestructiveAction = true
        a.beginSheetModal(for: main.window) { [weak self] resp in
            if resp == .alertSecondButtonReturn {
                self?.shutDown()
            } else {
                NSApp.reply(toApplicationShouldTerminate: false)
            }
        }
    }

    private func shutDown() {
        quitConfirmed = true
        loading?.show(message: "Cerrando Open POS…")
        server.stop { NSApp.reply(toApplicationShouldTerminate: true) }
    }

    // MARK: Instalar desde el DMG

    /// Abierta desde el DMG, la app se copia a Aplicaciones y se abre de ahí
    /// (desde el DMG dejaría de existir al expulsarlo).
    func offerInstallFromDiskImage() -> Bool {
        let path = Bundle.main.bundlePath
        guard path.hasPrefix("/Volumes/") || path.contains("/AppTranslocation/") else { return false }
        let me = ProcessInfo.processInfo.processIdentifier
        if let id = Bundle.main.bundleIdentifier,
           NSRunningApplication.runningApplications(withBundleIdentifier: id).contains(where: { $0.processIdentifier != me }) {
            showAlert(in: nil, title: "Open POS ya está abierto",
                      text: "Cierra Open POS (Cmd+Q) y vuelve a abrir el instalador para actualizarlo.")
            NSApp.terminate(nil)
            return true
        }
        let a = NSAlert()
        a.messageText = "Instalar Open POS \(appVersion)"
        a.informativeText = "Open POS se copiará a la carpeta Aplicaciones (si ya hay una versión, se reemplaza). Tus datos se conservan."
        a.addButton(withTitle: "Instalar")
        a.addButton(withTitle: "Cancelar")
        NSApp.activate(ignoringOtherApps: true)
        guard a.runModal() == .alertFirstButtonReturn else {
            NSApp.terminate(nil)
            return true
        }
        let dest = URL(fileURLWithPath: "/Applications/Open POS.app")
        do {
            if FileManager.default.fileExists(atPath: dest.path) {
                try FileManager.default.removeItem(at: dest)
            }
            try FileManager.default.copyItem(at: URL(fileURLWithPath: path), to: dest)
            let x = Process()
            x.executableURL = URL(fileURLWithPath: "/usr/bin/xattr")
            x.arguments = ["-dr", "com.apple.quarantine", dest.path]
            try? x.run()
            x.waitUntilExit()
        } catch {
            showAlert(in: nil, title: "No se pudo instalar",
                      text: "Arrastra Open POS a la carpeta Aplicaciones y ábrelo desde ahí.\n\n\(error.localizedDescription)")
            NSApp.terminate(nil)
            return true
        }
        let cfg = NSWorkspace.OpenConfiguration()
        cfg.createsNewApplicationInstance = true
        NSWorkspace.shared.openApplication(at: dest, configuration: cfg) { _, _ in
            DispatchQueue.main.async { NSApp.terminate(nil) }
        }
        return true
    }

    // MARK: Menús

    private func buildMenu() {
        let bar = NSMenu()

        let app = NSMenu(title: "Open POS")
        app.addItem(withTitle: "Acerca de Open POS", action: #selector(about), keyEquivalent: "")
        app.addItem(.separator())
        app.addItem(withTitle: "Conectar un iPad…", action: #selector(connectIPad), keyEquivalent: "")
        if #available(macOS 13.0, *) {
            app.addItem(withTitle: "Abrir al iniciar sesión", action: #selector(toggleLoginItem(_:)), keyEquivalent: "")
        }
        app.addItem(.separator())
        app.addItem(withTitle: "Ocultar Open POS", action: #selector(NSApplication.hide(_:)), keyEquivalent: "h")
        let others = app.addItem(withTitle: "Ocultar otros", action: #selector(NSApplication.hideOtherApplications(_:)), keyEquivalent: "h")
        others.keyEquivalentModifierMask = [.command, .option]
        app.addItem(withTitle: "Mostrar todo", action: #selector(NSApplication.unhideAllApplications(_:)), keyEquivalent: "")
        app.addItem(.separator())
        app.addItem(withTitle: "Salir de Open POS", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")
        addSubmenu(app, to: bar)

        // Sin este menú no funcionan Cmd+C / Cmd+V en los campos de texto.
        let edit = NSMenu(title: "Edición")
        edit.addItem(withTitle: "Deshacer", action: Selector(("undo:")), keyEquivalent: "z")
        let redo = edit.addItem(withTitle: "Rehacer", action: Selector(("redo:")), keyEquivalent: "z")
        redo.keyEquivalentModifierMask = [.command, .shift]
        edit.addItem(.separator())
        edit.addItem(withTitle: "Cortar", action: #selector(NSText.cut(_:)), keyEquivalent: "x")
        edit.addItem(withTitle: "Copiar", action: #selector(NSText.copy(_:)), keyEquivalent: "c")
        edit.addItem(withTitle: "Pegar", action: #selector(NSText.paste(_:)), keyEquivalent: "v")
        edit.addItem(withTitle: "Seleccionar todo", action: #selector(NSText.selectAll(_:)), keyEquivalent: "a")
        addSubmenu(edit, to: bar)

        let view = NSMenu(title: "Ver")
        view.addItem(withTitle: "Punto de venta", action: #selector(goHome), keyEquivalent: "1")
        view.addItem(withTitle: "Caja", action: #selector(goCash), keyEquivalent: "2")
        view.addItem(withTitle: "Administración", action: #selector(goAdmin), keyEquivalent: "3")
        view.addItem(.separator())
        view.addItem(withTitle: "Atrás", action: #selector(goBack), keyEquivalent: "[")
        view.addItem(withTitle: "Recargar", action: #selector(reload), keyEquivalent: "r")
        view.addItem(.separator())
        view.addItem(withTitle: "Acercar", action: #selector(zoomIn), keyEquivalent: "+")
        view.addItem(withTitle: "Alejar", action: #selector(zoomOut), keyEquivalent: "-")
        view.addItem(withTitle: "Tamaño real", action: #selector(zoomReset), keyEquivalent: "0")
        view.addItem(.separator())
        let full = view.addItem(withTitle: "Pantalla completa", action: #selector(NSWindow.toggleFullScreen(_:)), keyEquivalent: "f")
        full.keyEquivalentModifierMask = [.command, .control]
        addSubmenu(view, to: bar)

        let window = NSMenu(title: "Ventana")
        window.addItem(withTitle: "Minimizar", action: #selector(NSWindow.performMiniaturize(_:)), keyEquivalent: "m")
        window.addItem(withTitle: "Zoom", action: #selector(NSWindow.performZoom(_:)), keyEquivalent: "")
        window.addItem(withTitle: "Cerrar", action: #selector(NSWindow.performClose(_:)), keyEquivalent: "w")
        addSubmenu(window, to: bar)
        NSApp.windowsMenu = window

        let help = NSMenu(title: "Ayuda")
        help.addItem(withTitle: "Abrir carpeta de datos y respaldos", action: #selector(openDataFolder), keyEquivalent: "")
        help.addItem(withTitle: "Ver registro del servidor", action: #selector(showLog), keyEquivalent: "")
        addSubmenu(help, to: bar)

        NSApp.mainMenu = bar
    }

    private func addSubmenu(_ menu: NSMenu, to bar: NSMenu) {
        let item = NSMenuItem(title: menu.title, action: nil, keyEquivalent: "")
        item.submenu = menu
        bar.addItem(item)
    }

    @objc func about() {
        NSApp.orderFrontStandardAboutPanel(options: [
            .applicationName: "Open POS",
            .applicationVersion: appVersion,
            .version: "",
        ])
    }

    @objc func connectIPad() {
        let url = localIPAddress().map { "http://\($0):\(server.port)" }
        let a = NSAlert()
        a.messageText = "Conectar un iPad"
        if let url {
            a.informativeText = "En el iPad, conectado a la misma red Wi-Fi que esta Mac, abre Safari y entra a:\n\n\(url)\n\n" +
                "Tip: en Safari toca Compartir → \"Agregar a inicio\" para tenerlo como app.\n" +
                "Open POS debe estar abierto en esta Mac para que el iPad funcione."
            a.addButton(withTitle: "Copiar dirección")
            a.addButton(withTitle: "Listo")
        } else {
            a.informativeText = "Esta Mac no está conectada a una red. Conéctala al Wi-Fi del negocio e intenta de nuevo."
            a.addButton(withTitle: "Listo")
        }
        a.beginSheetModal(for: main.window) { resp in
            if let url, resp == .alertFirstButtonReturn {
                NSPasteboard.general.clearContents()
                NSPasteboard.general.setString(url, forType: .string)
            }
        }
    }

    @available(macOS 13.0, *)
    @objc func toggleLoginItem(_ sender: NSMenuItem) {
        let svc = SMAppService.mainApp
        do {
            if svc.status == .enabled { try svc.unregister() } else { try svc.register() }
        } catch {
            showAlert(in: main.window, title: "No se pudo cambiar", text: error.localizedDescription)
        }
    }

    @objc func goHome() { main?.load(server.baseURL) }
    @objc func goCash() { main?.load(server.baseURL.appendingPathComponent("caja")) }
    @objc func goAdmin() { main?.load(server.baseURL.appendingPathComponent("admin")) }
    @objc func goBack() { main?.webView.goBack() }
    @objc func reload() { main?.webView.reload() }
    @objc func zoomIn() { main.map { $0.webView.pageZoom = min(2, $0.webView.pageZoom + 0.1) } }
    @objc func zoomOut() { main.map { $0.webView.pageZoom = max(0.5, $0.webView.pageZoom - 0.1) } }
    @objc func zoomReset() { main?.webView.pageZoom = 1 }
    @objc func openDataFolder() { NSWorkspace.shared.open(server.dataDir) }
    @objc func showLog() {
        NSWorkspace.shared.open([server.logURL], withApplicationAt: URL(fileURLWithPath: "/System/Applications/Utilities/Console.app"),
                                configuration: NSWorkspace.OpenConfiguration())
    }
}

extension AppDelegate: NSMenuItemValidation {
    func validateMenuItem(_ item: NSMenuItem) -> Bool {
        if #available(macOS 13.0, *), item.action == #selector(toggleLoginItem(_:)) {
            item.state = SMAppService.mainApp.status == .enabled ? .on : .off
        }
        return true
    }
}

/// Pantalla mientras arranca el servidor, o si algo falla.
final class LoadingView: NSView {
    private let spinner = NSProgressIndicator()
    private let title = NSTextField(labelWithString: "")
    private let detail = NSTextField(wrappingLabelWithString: "")
    private let buttons = NSStackView()
    var onRetry: (() -> Void)?
    var onShowLog: (() -> Void)?

    override init(frame: NSRect) {
        super.init(frame: frame)
        wantsLayer = true
        layer?.backgroundColor = NSColor(red: 0.06, green: 0.09, blue: 0.16, alpha: 1).cgColor

        spinner.style = .spinning
        spinner.appearance = NSAppearance(named: .darkAqua)
        title.font = .systemFont(ofSize: 18, weight: .semibold)
        title.textColor = .white
        title.alignment = .center
        detail.font = .monospacedSystemFont(ofSize: 11, weight: .regular)
        detail.textColor = NSColor(white: 0.75, alpha: 1)
        detail.alignment = .center
        detail.preferredMaxLayoutWidth = 560

        let retry = NSButton(title: "Reintentar", target: self, action: #selector(retry))
        let log = NSButton(title: "Ver registro", target: self, action: #selector(showLog))
        buttons.addArrangedSubview(log)
        buttons.addArrangedSubview(retry)
        buttons.spacing = 12

        let icon = NSImageView(image: NSApp.applicationIconImage)
        icon.widthAnchor.constraint(equalToConstant: 96).isActive = true
        icon.heightAnchor.constraint(equalToConstant: 96).isActive = true

        let stack = NSStackView(views: [icon, spinner, title, detail, buttons])
        stack.orientation = .vertical
        stack.spacing = 14
        stack.translatesAutoresizingMaskIntoConstraints = false
        addSubview(stack)
        NSLayoutConstraint.activate([
            stack.centerXAnchor.constraint(equalTo: centerXAnchor),
            stack.centerYAnchor.constraint(equalTo: centerYAnchor),
            stack.widthAnchor.constraint(lessThanOrEqualToConstant: 600),
        ])
    }

    required init?(coder: NSCoder) { fatalError() }

    func show(message: String) {
        isHidden = false
        superview?.addSubview(self, positioned: .above, relativeTo: nil)
        spinner.isHidden = false
        spinner.startAnimation(nil)
        title.stringValue = message
        detail.isHidden = true
        buttons.isHidden = true
    }

    func fail(title text: String, detail info: String) {
        isHidden = false
        spinner.stopAnimation(nil)
        spinner.isHidden = true
        title.stringValue = text
        detail.stringValue = info
        detail.isHidden = info.isEmpty
        buttons.isHidden = false
    }

    func hide() {
        spinner.stopAnimation(nil)
        isHidden = true
    }

    @objc private func retry() { onRetry?() }
    @objc private func showLog() { onShowLog?() }
}

let app = NSApplication.shared
let delegate = AppDelegate()
app.delegate = delegate
app.setActivationPolicy(.regular)
app.run()
