// Mullion.swift — a tiny AppKit shell that owns a real window around the
// mullion control panel. Compiled directly with swiftc (no Xcode project,
// no storyboard): see build.sh for how this becomes Mullion.app.
//
// The panel itself is served by the mullion CLI (`mullion ui --window-host`);
// this app just launches that child process, waits for it to print the
// panel's URL, and shows it in a WKWebView. See macapp/build.sh and the Go
// side (internal/macapp) for the other half of the contract.

import Cocoa
import WebKit
import UniformTypeIdentifiers

// MARK: - Locating the mullion binary
//
// Apps launched from Finder don't inherit the user's shell PATH, so we
// can't just exec "mullion" and hope the shell finds it. Check the known
// install locations in the order the Homebrew formula and manual installs
// use them.
func locateMullionBinary() -> String? {
    let candidates = [
        ("~/.mullion/bin/mullion" as NSString).expandingTildeInPath,
        "/opt/homebrew/bin/mullion",
        "/usr/local/bin/mullion",
    ]
    for path in candidates where FileManager.default.isExecutableFile(atPath: path) {
        return path
    }
    return nil
}

private let mullionURLPrefix = "MULLION_UI_URL="

final class AppDelegate: NSObject, NSApplicationDelegate {
    var window: NSWindow!
    var webView: WKWebView!
    var statusLabel: NSTextField!

    var childProcess: Process?
    var childStdin: Pipe?
    var panelURL: URL?
    var stderrTail: [String] = []
    var urlResolved = false

    // MARK: Lifecycle

    func applicationDidFinishLaunching(_ notification: Notification) {
        NSApp.setActivationPolicy(.regular)
        buildMainMenu()
        buildWindow()
        NSApp.activate(ignoringOtherApps: true)
        startChildProcess()
    }

    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool {
        true
    }

    // Clicking the Dock icon while the panel is already running should
    // just bring the window forward, not spawn anything new.
    func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool {
        window.makeKeyAndOrderFront(nil)
        return true
    }

    func applicationWillTerminate(_ notification: Notification) {
        stopChildProcess()
    }

    // MARK: Menu

    // Built by hand since there's no storyboard/xib to supply the default
    // menu bar. The Edit menu's Undo/Redo/Cut/Copy/Paste/Select All use the
    // standard first-responder selectors (no explicit target) so they reach
    // whatever WKWebView's internal text field currently has focus — that's
    // what makes ⌘C/⌘V work while typing into the panel.
    func buildMainMenu() {
        let mainMenu = NSMenu()

        let appMenuItem = NSMenuItem()
        let appMenu = NSMenu()
        appMenu.addItem(withTitle: "About Mullion", action: #selector(NSApplication.orderFrontStandardAboutPanel(_:)), keyEquivalent: "")
        appMenu.addItem(NSMenuItem.separator())
        appMenu.addItem(withTitle: "Hide Mullion", action: #selector(NSApplication.hide(_:)), keyEquivalent: "h")
        appMenu.addItem(NSMenuItem.separator())
        appMenu.addItem(withTitle: "Quit Mullion", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")
        appMenuItem.submenu = appMenu
        mainMenu.addItem(appMenuItem)

        let editMenuItem = NSMenuItem()
        let editMenu = NSMenu(title: "Edit")
        editMenu.addItem(withTitle: "Undo", action: Selector(("undo:")), keyEquivalent: "z")
        editMenu.addItem(withTitle: "Redo", action: Selector(("redo:")), keyEquivalent: "Z")
        editMenu.addItem(NSMenuItem.separator())
        editMenu.addItem(withTitle: "Cut", action: #selector(NSText.cut(_:)), keyEquivalent: "x")
        editMenu.addItem(withTitle: "Copy", action: #selector(NSText.copy(_:)), keyEquivalent: "c")
        editMenu.addItem(withTitle: "Paste", action: #selector(NSText.paste(_:)), keyEquivalent: "v")
        editMenu.addItem(NSMenuItem.separator())
        editMenu.addItem(withTitle: "Select All", action: #selector(NSText.selectAll(_:)), keyEquivalent: "a")
        editMenuItem.submenu = editMenu
        mainMenu.addItem(editMenuItem)

        let viewMenuItem = NSMenuItem()
        let viewMenu = NSMenu(title: "View")
        viewMenu.addItem(withTitle: "Reload", action: #selector(reload(_:)), keyEquivalent: "r")
        viewMenuItem.submenu = viewMenu
        mainMenu.addItem(viewMenuItem)

        let windowMenuItem = NSMenuItem()
        let windowMenu = NSMenu(title: "Window")
        windowMenu.addItem(withTitle: "Minimize", action: #selector(NSWindow.performMiniaturize(_:)), keyEquivalent: "m")
        windowMenu.addItem(withTitle: "Close", action: #selector(NSWindow.performClose(_:)), keyEquivalent: "w")
        windowMenuItem.submenu = windowMenu
        mainMenu.addItem(windowMenuItem)
        // Lets AppKit append the standard "Bring All to Front" etc. items.
        NSApp.windowsMenu = windowMenu

        NSApp.mainMenu = mainMenu
    }

    @objc func reload(_ sender: Any?) {
        webView?.reload()
    }

    // MARK: Window

    func buildWindow() {
        let rect = NSRect(x: 0, y: 0, width: 1180, height: 800)
        window = NSWindow(
            contentRect: rect,
            styleMask: [.titled, .closable, .miniaturizable, .resizable],
            backing: .buffered,
            defer: false
        )
        window.title = "Mullion"
        window.minSize = NSSize(width: 900, height: 600)
        window.center()
        // Remembers the user's size/position across launches; centering
        // above only applies the very first time (no saved frame yet).
        window.setFrameAutosaveName("MullionMain")

        statusLabel = NSTextField(labelWithString: "Starting Mullion…")
        statusLabel.font = NSFont.systemFont(ofSize: 15)
        statusLabel.textColor = .secondaryLabelColor
        statusLabel.alignment = .center
        statusLabel.frame = rect
        statusLabel.autoresizingMask = [.width, .height]
        window.contentView = statusLabel

        window.makeKeyAndOrderFront(nil)
    }

    func showWebView() {
        // Register the JS bridge on the configuration BEFORE the web view
        // exists — adding a script message handler afterwards wouldn't
        // reach scripts already running in the page.
        let configuration = WKWebViewConfiguration()
        configuration.userContentController.add(self, name: "mullion")
        let webView = WKWebView(frame: window.contentView!.bounds, configuration: configuration)
        webView.autoresizingMask = [.width, .height]
        webView.navigationDelegate = self
        webView.uiDelegate = self
        window.contentView = webView
        self.webView = webView
        if let url = panelURL {
            webView.load(URLRequest(url: url))
        }
    }

    // MARK: Child process
    //
    // Contract with the Go side (internal/macapp): run
    // `<mullion> ui --window-host`, read stdout for a single
    // "MULLION_UI_URL=..." line once the server is listening, then keep the
    // child's stdin pipe open until we quit (closing it is the child's
    // signal to stop serving).

    func startChildProcess() {
        guard let binaryPath = locateMullionBinary() else {
            fail(reason: """
            Could not find the mullion binary. Checked:
              ~/.mullion/bin/mullion
              /opt/homebrew/bin/mullion
              /usr/local/bin/mullion
            """)
            return
        }

        let process = Process()
        process.executableURL = URL(fileURLWithPath: binaryPath)
        process.arguments = ["ui", "--window-host"]

        let stdinPipe = Pipe()
        let stdoutPipe = Pipe()
        let stderrPipe = Pipe()
        process.standardInput = stdinPipe
        process.standardOutput = stdoutPipe
        process.standardError = stderrPipe
        childStdin = stdinPipe
        childProcess = process

        var stdoutBuffer = Data()
        stdoutPipe.fileHandleForReading.readabilityHandler = { [weak self] handle in
            let data = handle.availableData
            guard !data.isEmpty else { return }
            stdoutBuffer.append(data)
            self?.scanForURL(in: &stdoutBuffer)
        }

        stderrPipe.fileHandleForReading.readabilityHandler = { [weak self] handle in
            let data = handle.availableData
            guard !data.isEmpty, let text = String(data: data, encoding: .utf8) else { return }
            DispatchQueue.main.async {
                self?.appendStderr(text)
            }
        }

        process.terminationHandler = { [weak self] proc in
            DispatchQueue.main.async {
                guard let self = self, !self.urlResolved else { return }
                self.fail(reason: "mullion exited before starting the control panel (status \(proc.terminationStatus)).")
            }
        }

        do {
            try process.run()
        } catch {
            fail(reason: "Failed to launch mullion: \(error.localizedDescription)")
            return
        }

        DispatchQueue.main.asyncAfter(deadline: .now() + 20) { [weak self] in
            guard let self = self, !self.urlResolved else { return }
            self.fail(reason: "Timed out waiting for the control panel to start.")
        }
    }

    // Runs on the readabilityHandler's background queue; only the URL
    // hand-off dispatches back to main.
    func scanForURL(in buffer: inout Data) {
        while let newline = buffer.firstIndex(of: 0x0A) {
            let lineData = buffer.subdata(in: buffer.startIndex..<newline)
            buffer.removeSubrange(buffer.startIndex...newline)
            guard let line = String(data: lineData, encoding: .utf8) else { continue }
            let trimmed = line.trimmingCharacters(in: .whitespacesAndNewlines)
            guard trimmed.hasPrefix(mullionURLPrefix) else { continue }
            guard let url = URL(string: String(trimmed.dropFirst(mullionURLPrefix.count))) else { continue }
            DispatchQueue.main.async { [weak self] in
                self?.handleResolvedURL(url)
            }
        }
    }

    func handleResolvedURL(_ url: URL) {
        guard !urlResolved else { return }
        urlResolved = true
        panelURL = url
        showWebView()
    }

    func appendStderr(_ text: String) {
        stderrTail.append(text)
        // Keep only a reasonable tail so a runaway child can't balloon the
        // eventual error alert.
        let joined = stderrTail.joined()
        if joined.utf8.count > 4000 {
            stderrTail = [String(joined.suffix(4000))]
        }
    }

    func fail(reason: String) {
        guard !urlResolved else { return } // a working panel outlives stray stderr/exit noise
        stopChildProcess()
        let alert = NSAlert()
        alert.alertStyle = .critical
        alert.messageText = "Mullion couldn't start"
        var text = reason
        if !stderrTail.isEmpty {
            text += "\n\n" + stderrTail.joined()
        }
        alert.informativeText = text
        alert.addButton(withTitle: "Quit")
        alert.runModal()
        NSApp.terminate(nil)
    }

    func stopChildProcess() {
        childStdin?.fileHandleForWriting.closeFile()
        childStdin = nil
        if let process = childProcess {
            process.terminationHandler = nil
            if process.isRunning {
                process.terminate()
            }
        }
        childProcess = nil
    }
}

// MARK: - WKNavigationDelegate / WKUIDelegate

extension AppDelegate: WKNavigationDelegate, WKUIDelegate {
    // Keep the panel's own host+port inside the app window; send everything
    // else (site links like https://site.test, external http/https) to the
    // user's default browser instead.
    func webView(_ webView: WKWebView, decidePolicyFor navigationAction: WKNavigationAction, decisionHandler: @escaping (WKNavigationActionPolicy) -> Void) {
        guard let url = navigationAction.request.url, let panel = panelURL else {
            decisionHandler(.allow)
            return
        }
        let targetPort = url.port ?? (url.scheme == "https" ? 443 : 80)
        let panelPort = panel.port ?? (panel.scheme == "https" ? 443 : 80)
        if url.host == panel.host && targetPort == panelPort {
            decisionHandler(.allow)
        } else {
            decisionHandler(.cancel)
            NSWorkspace.shared.open(url)
        }
    }

    // window.open / target=_blank: never spawn a second webview, hand the
    // link to the default browser instead.
    func webView(_ webView: WKWebView, createWebViewWith configuration: WKWebViewConfiguration, for navigationAction: WKNavigationAction, windowFeatures: WKWindowFeatures) -> WKWebView? {
        if let url = navigationAction.request.url {
            NSWorkspace.shared.open(url)
        }
        return nil
    }

    func webView(_ webView: WKWebView, runJavaScriptAlertPanelWithMessage message: String, initiatedByFrame frame: WKFrameInfo, completionHandler: @escaping () -> Void) {
        let alert = NSAlert()
        alert.messageText = message
        alert.addButton(withTitle: "OK")
        alert.runModal()
        completionHandler()
    }

    func webView(_ webView: WKWebView, runJavaScriptConfirmPanelWithMessage message: String, initiatedByFrame frame: WKFrameInfo, completionHandler: @escaping (Bool) -> Void) {
        let alert = NSAlert()
        alert.messageText = message
        alert.addButton(withTitle: "OK")
        alert.addButton(withTitle: "Cancel")
        completionHandler(alert.runModal() == .alertFirstButtonReturn)
    }

    func webView(_ webView: WKWebView, runJavaScriptTextInputPanelWithPrompt prompt: String, defaultText: String?, initiatedByFrame frame: WKFrameInfo, completionHandler: @escaping (String?) -> Void) {
        let alert = NSAlert()
        alert.messageText = prompt
        alert.addButton(withTitle: "OK")
        alert.addButton(withTitle: "Cancel")
        let field = NSTextField(frame: NSRect(x: 0, y: 0, width: 260, height: 24))
        field.stringValue = defaultText ?? ""
        alert.accessoryView = field
        alert.window.initialFirstResponder = field
        let response = alert.runModal()
        completionHandler(response == .alertFirstButtonReturn ? field.stringValue : nil)
    }
}

// MARK: - JS bridge: native folder/file pickers
//
// The panel can't get a real filesystem path out of an <input type=file>
// in a browser — so when it's running inside this app, its JS calls
// window.webkit.messageHandlers.mullion.postMessage({type: 'pickFolder'
// | 'pickFile', id, title, types?}) and we answer with an NSOpenPanel
// sheet, handing the result back via window.__mullionPicked(id, pathOrNull).

extension AppDelegate: WKScriptMessageHandler {
    func userContentController(_ userContentController: WKUserContentController, didReceive message: WKScriptMessage) {
        guard message.name == "mullion",
              let body = message.body as? [String: Any],
              let type = body["type"] as? String
        else { return }

        switch type {
        case "pickFolder":
            presentFolderPicker(id: body["id"], title: body["title"] as? String)
        case "pickFile":
            presentFilePicker(id: body["id"], title: body["title"] as? String, types: body["types"] as? [String])
        default:
            break
        }
    }

    func presentFolderPicker(id: Any?, title: String?) {
        guard let id = id, let window = window else { return }

        let panel = NSOpenPanel()
        panel.canChooseDirectories = true
        panel.canChooseFiles = false
        panel.allowsMultipleSelection = false
        panel.prompt = "Choose"
        panel.message = (title?.isEmpty == false) ? title! : "Choose the project folder"

        panel.beginSheetModal(for: window) { [weak self] response in
            let path = (response == .OK) ? panel.url?.path : nil
            self?.sendPickedFolder(id: id, path: path)
        }
    }

    // Same idea as presentFolderPicker, but for a single file — used by
    // the "Import .sql…" restore button, which needs a real path to a
    // .sql/.sql.gz backup rather than a directory. types are extensions
    // without the leading dot (e.g. "sql", "gz"); unrecognized extensions
    // are simply skipped, leaving the picker unfiltered.
    func presentFilePicker(id: Any?, title: String?, types: [String]?) {
        guard let id = id, let window = window else { return }

        let panel = NSOpenPanel()
        panel.canChooseDirectories = false
        panel.canChooseFiles = true
        panel.allowsMultipleSelection = false
        panel.prompt = "Choose"
        panel.message = (title?.isEmpty == false) ? title! : "Choose a file"
        if let types = types {
            let allowed = types.compactMap { UTType(filenameExtension: $0) }
            if !allowed.isEmpty {
                panel.allowedContentTypes = allowed
            }
        }

        panel.beginSheetModal(for: window) { [weak self] response in
            let path = (response == .OK) ? panel.url?.path : nil
            self?.sendPickedFolder(id: id, path: path)
        }
    }

    // Encodes (id, path) as a JSON array and unwraps its brackets to build
    // the argument list — the safe way to splice arbitrary/possibly-nil
    // strings into a JS call without hand-rolling escaping.
    func sendPickedFolder(id: Any, path: String?) {
        guard let webView = webView,
              let data = try? JSONSerialization.data(withJSONObject: [id, path ?? NSNull()]),
              let json = String(data: data, encoding: .utf8)
        else { return }
        let args = json.dropFirst().dropLast() // "[1,\"/x\"]" -> "1,\"/x\""
        webView.evaluateJavaScript("window.__mullionPicked && window.__mullionPicked(\(args));")
    }
}

// MARK: - Entry point

let app = NSApplication.shared
let delegate = AppDelegate()
app.delegate = delegate
app.run()
