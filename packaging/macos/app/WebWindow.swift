import Cocoa
@preconcurrency import WebKit

/// Una ventana con el POS. La principal muestra el POS; las secundarias se
/// abren para el ticket (window.open) y se cierran solas.
final class WebWindow: NSObject, NSWindowDelegate, WKNavigationDelegate, WKUIDelegate, WKDownloadDelegate, WKScriptMessageHandler {
    let window: NSWindow
    let webView: WKWebView
    let isMain: Bool
    private let origin: URL
    private var children: [WebWindow] = []
    private var downloads: [WKDownload: URL] = [:]
    weak var parent: WebWindow?

    /// Para la ventana principal: al cerrarla se sale de la app (con aviso).
    var onCloseRequest: (() -> Void)?

    // window.print() no hace nada en WebKit fuera de Safari: se reemplaza por
    // el diálogo de impresión de macOS.
    static let printShim = """
    window.print = function () { window.webkit.messageHandlers.openpos.postMessage({ action: 'print' }); };
    """

    static func makeConfiguration() -> WKWebViewConfiguration {
        let config = WKWebViewConfiguration()
        config.preferences.javaScriptCanOpenWindowsAutomatically = true
        config.websiteDataStore = .default() // conserva la sesión del admin
        return config
    }

    init(origin: URL, configuration: WKWebViewConfiguration? = nil, isMain: Bool, frame: NSRect) {
        self.origin = origin
        self.isMain = isMain
        let config = configuration ?? WebWindow.makeConfiguration()
        webView = WKWebView(frame: frame, configuration: config)
        window = NSWindow(
            contentRect: frame,
            styleMask: [.titled, .closable, .miniaturizable, .resizable],
            backing: .buffered, defer: false)
        super.init()

        // Las ventanas hijas (ticket) comparten la configuración de la
        // principal, y con ella su manejador de mensajes y el reemplazo de
        // window.print(); el manejador imprime la vista que mandó el mensaje.
        if configuration == nil {
            let ucc = config.userContentController
            ucc.add(WeakScriptHandler(self), name: "openpos")
            ucc.addUserScript(WKUserScript(source: WebWindow.printShim, injectionTime: .atDocumentStart, forMainFrameOnly: false))
        }

        webView.navigationDelegate = self
        webView.uiDelegate = self
        webView.allowsBackForwardNavigationGestures = true
        webView.setValue(false, forKey: "drawsBackground")

        window.title = "Open POS"
        window.contentView = webView
        window.delegate = self
        window.isReleasedWhenClosed = false
        window.minSize = NSSize(width: 640, height: 480)
        if isMain {
            window.setFrameAutosaveName("OpenPOSMain")
            window.tabbingMode = .disallowed
        }
    }

    func load(_ url: URL) { webView.load(URLRequest(url: url)) }

    private func isOwnURL(_ url: URL) -> Bool {
        url.host == origin.host && url.port == origin.port
    }

    // MARK: Ventana

    func windowShouldClose(_ sender: NSWindow) -> Bool {
        if isMain, let onCloseRequest {
            onCloseRequest()
            return false
        }
        return true
    }

    func windowWillClose(_ notification: Notification) {
        parent?.children.removeAll { $0 === self }
    }

    // MARK: Navegación

    func webView(_ webView: WKWebView, decidePolicyFor action: WKNavigationAction, decisionHandler: @escaping (WKNavigationActionPolicy) -> Void) {
        guard let url = action.request.url else { return decisionHandler(.allow) }
        if url.scheme == "http" || url.scheme == "https", !isOwnURL(url) {
            // Enlaces externos, en el navegador.
            NSWorkspace.shared.open(url)
            return decisionHandler(.cancel)
        }
        decisionHandler(action.shouldPerformDownload ? .download : .allow)
    }

    func webView(_ webView: WKWebView, decidePolicyFor response: WKNavigationResponse, decisionHandler: @escaping (WKNavigationResponsePolicy) -> Void) {
        if let http = response.response as? HTTPURLResponse,
           let cd = http.value(forHTTPHeaderField: "Content-Disposition"), cd.lowercased().hasPrefix("attachment") {
            return decisionHandler(.download)
        }
        decisionHandler(response.canShowMIMEType ? .allow : .download)
    }

    func webView(_ webView: WKWebView, navigationAction: WKNavigationAction, didBecome download: WKDownload) {
        download.delegate = self
    }

    func webView(_ webView: WKWebView, navigationResponse: WKNavigationResponse, didBecome download: WKDownload) {
        download.delegate = self
    }

    func webView(_ webView: WKWebView, didFinish navigation: WKNavigation!) {
        if let title = webView.title, !title.isEmpty { window.title = title }
    }

    func webViewWebContentProcessDidTerminate(_ webView: WKWebView) {
        webView.reload()
    }

    // MARK: Descargas (CSV de reportes, respaldos)

    func download(_ download: WKDownload, decideDestinationUsing response: URLResponse, suggestedFilename: String, completionHandler: @escaping (URL?) -> Void) {
        let dir = FileManager.default.urls(for: .downloadsDirectory, in: .userDomainMask)[0]
        var dest = dir.appendingPathComponent(suggestedFilename)
        let base = dest.deletingPathExtension().lastPathComponent, ext = dest.pathExtension
        var n = 2
        while FileManager.default.fileExists(atPath: dest.path) {
            dest = dir.appendingPathComponent("\(base) (\(n))").appendingPathExtension(ext)
            n += 1
        }
        downloads[download] = dest
        completionHandler(dest)
    }

    func downloadDidFinish(_ download: WKDownload) {
        if let dest = downloads.removeValue(forKey: download) {
            NSWorkspace.shared.activateFileViewerSelecting([dest])
        }
    }

    func download(_ download: WKDownload, didFailWithError error: Error, resumeData: Data?) {
        downloads.removeValue(forKey: download)
        showAlert(in: window, title: "No se pudo descargar el archivo", text: error.localizedDescription)
    }

    // MARK: Diálogos de la página

    func webView(_ webView: WKWebView, runJavaScriptAlertPanelWithMessage message: String, initiatedByFrame frame: WKFrameInfo, completionHandler: @escaping () -> Void) {
        let a = NSAlert()
        a.messageText = message
        a.addButton(withTitle: "OK")
        a.beginSheetModal(for: window) { _ in completionHandler() }
    }

    func webView(_ webView: WKWebView, runJavaScriptConfirmPanelWithMessage message: String, initiatedByFrame frame: WKFrameInfo, completionHandler: @escaping (Bool) -> Void) {
        let a = NSAlert()
        a.messageText = message
        a.addButton(withTitle: "Aceptar")
        a.addButton(withTitle: "Cancelar")
        a.beginSheetModal(for: window) { completionHandler($0 == .alertFirstButtonReturn) }
    }

    func webView(_ webView: WKWebView, runJavaScriptTextInputPanelWithPrompt prompt: String, defaultText: String?, initiatedByFrame frame: WKFrameInfo, completionHandler: @escaping (String?) -> Void) {
        let a = NSAlert()
        a.messageText = prompt
        let field = NSTextField(frame: NSRect(x: 0, y: 0, width: 260, height: 24))
        field.stringValue = defaultText ?? ""
        a.accessoryView = field
        a.addButton(withTitle: "Aceptar")
        a.addButton(withTitle: "Cancelar")
        a.beginSheetModal(for: window) { completionHandler($0 == .alertFirstButtonReturn ? field.stringValue : nil) }
    }

    // <input type="file"> (logo del ticket)
    func webView(_ webView: WKWebView, runOpenPanelWith parameters: WKOpenPanelParameters, initiatedByFrame frame: WKFrameInfo, completionHandler: @escaping ([URL]?) -> Void) {
        let panel = NSOpenPanel()
        panel.allowsMultipleSelection = parameters.allowsMultipleSelection
        panel.canChooseDirectories = false
        panel.beginSheetModal(for: window) { completionHandler($0 == .OK ? panel.urls : nil) }
    }

    // window.open (el ticket para imprimir): ventana propia, más chica.
    func webView(_ webView: WKWebView, createWebViewWith configuration: WKWebViewConfiguration, for action: WKNavigationAction, windowFeatures: WKWindowFeatures) -> WKWebView? {
        if let url = action.request.url, url.scheme?.hasPrefix("http") == true, !isOwnURL(url) {
            NSWorkspace.shared.open(url)
            return nil
        }
        let child = WebWindow(origin: origin, configuration: configuration, isMain: false,
                              frame: NSRect(x: 0, y: 0, width: 460, height: 720))
        child.parent = self
        children.append(child)
        child.window.cascadeTopLeft(from: NSPoint(x: window.frame.minX + 40, y: window.frame.maxY - 40))
        child.window.makeKeyAndOrderFront(nil)
        return child.webView
    }

    func webViewDidClose(_ webView: WKWebView) {
        window.close()
    }

    // MARK: Mensajes de la página

    func userContentController(_ ucc: WKUserContentController, didReceive message: WKScriptMessage) {
        guard let body = message.body as? [String: Any], body["action"] as? String == "print" else { return }
        let target = message.webView ?? webView
        let info = NSPrintInfo.shared.copy() as! NSPrintInfo
        info.topMargin = 12
        info.bottomMargin = 12
        info.leftMargin = 12
        info.rightMargin = 12
        info.isVerticallyCentered = false
        let op = target.printOperation(with: info)
        op.view?.frame = target.bounds
        let host = target.window ?? window
        // Se espera a que la página termine de acomodarse antes de imprimir.
        DispatchQueue.main.asyncAfter(deadline: .now() + 0.2) {
            op.runModal(for: host, delegate: nil, didRun: nil, contextInfo: nil)
        }
    }
}

/// WKUserContentController retiene a su manejador; esto evita un ciclo.
final class WeakScriptHandler: NSObject, WKScriptMessageHandler {
    weak var target: WKScriptMessageHandler?
    init(_ target: WKScriptMessageHandler) { self.target = target }
    func userContentController(_ ucc: WKUserContentController, didReceive message: WKScriptMessage) {
        target?.userContentController(ucc, didReceive: message)
    }
}

func showAlert(in window: NSWindow?, title: String, text: String, style: NSAlert.Style = .warning) {
    let a = NSAlert()
    a.alertStyle = style
    a.messageText = title
    a.informativeText = text
    a.addButton(withTitle: "OK")
    if let window { a.beginSheetModal(for: window) } else { a.runModal() }
}
