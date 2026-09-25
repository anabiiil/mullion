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

// A NSWindow.tabbingIdentifier shared by the main panel window and every
// secondary window (terminal / project) so AppKit's native tabs can merge
// them together when the user has "Prefer tabs" on.
private let windowTabbingIdentifier = "mullion"

// Returns whether two URLs share a host+port — the same test the
// navigation delegate uses to decide "stay in the app" vs. "hand off to
// the default browser". Pulled out as a pure function so it's easy to
// exercise on its own (e.g. from a scratch test harness) without spinning
// up any WebKit/AppKit objects.
func isSameOrigin(_ a: URL, _ b: URL) -> Bool {
    let portA = a.port ?? (a.scheme == "https" ? 443 : 80)
    let portB = b.port ?? (b.scheme == "https" ? 443 : 80)
    return a.host == b.host && portA == portB
}

// One entry per secondary window (terminal / project popouts opened via
// window.open). Holds the window and its webview alive for as long as the
// window is open, plus the KVO observation that mirrors the page's
// document.title into the window's title bar. Dropping the entry (when
// the window closes) tears all of that down together.
final class SecondaryWindowEntry {
    let window: NSWindow
    let webView: WKWebView
    var titleObservation: NSKeyValueObservation?

    init(window: NSWindow, webView: WKWebView) {
        self.window = window
        self.webView = webView
    }
}

final class AppDelegate: NSObject, NSApplicationDelegate {
    var window: NSWindow!
    var webView: WKWebView!
    var statusLabel: NSTextField!

    var childProcess: Process?
    var childStdin: Pipe?
    var panelURL: URL?
    var stderrTail: [String] = []
    var urlResolved = false

    // Terminal/project windows opened from the panel via window.open. The
    // main window isn't in here — it's tracked separately via `window`.
    var secondaryWindows: [SecondaryWindowEntry] = []

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

        let fileMenuItem = NSMenuItem()
        let fileMenu = NSMenu(title: "File")
        fileMenu.addItem(withTitle: "New Window", action: #selector(newMainWindow(_:)), keyEquivalent: "n")
        fileMenuItem.submenu = fileMenu
        mainMenu.addItem(fileMenuItem)

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
        windowMenu.addItem(NSMenuItem.separator())
        // Standard AppKit tabbing actions: since we build the menu bar by
        // hand (no nib/storyboard to wire this up implicitly), these need
        // to be added explicitly. AppKit still handles enabling/disabling
        // them and flipping "Show Tab Bar" to "Hide Tab Bar" on its own.
        windowMenu.addItem(withTitle: "Show Tab Bar", action: #selector(NSWindow.toggleTabBar(_:)), keyEquivalent: "")
        windowMenu.addItem(withTitle: "Merge All Windows", action: #selector(NSWindow.mergeAllWindows(_:)), keyEquivalent: "")
        windowMenuItem.submenu = windowMenu
        mainMenu.addItem(windowMenuItem)
        // Lets AppKit append the standard "Bring All to Front" etc. items.
        NSApp.windowsMenu = windowMenu

        NSApp.mainMenu = mainMenu
    }

    @objc func reload(_ sender: Any?) {
        webView?.reload()
    }

    // Cmd+N: open a fresh window onto the same panel root (a new tab if
    // native tabbing is on). Independent WKWebViewConfiguration/session
    // from the main window, same as any other "new window" in a browser.
    @objc func newMainWindow(_ sender: Any?) {
        guard let url = panelURL else { return }
        let configuration = WKWebViewConfiguration()
        configuration.userContentController.add(self, name: "mullion")
        let newWebView = openSecondaryWindow(configuration: configuration, windowFeatures: nil)
        newWebView.load(URLRequest(url: url))
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
        window.tabbingIdentifier = windowTabbingIdentifier
        window.tabbingMode = .automatic
        window.delegate = self

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

    // MARK: Secondary windows (terminal / project popouts)
    //
    // The panel opens these with window.open(sameOriginURL, name,
    // 'popup,width=…,height=…') for a standalone terminal or a project in
    // its own window. createWebViewWith (below) is what calls this; it
    // hands us the WKWebViewConfiguration WebKit already built for the
    // popup so window.opener and the shared data store/process pool keep
    // working, same as a real browser's popup windows.
    @discardableResult
    func openSecondaryWindow(configuration: WKWebViewConfiguration, windowFeatures: WKWindowFeatures?) -> WKWebView {
        let width = windowFeatures?.width?.doubleValue ?? 1100
        let height = windowFeatures?.height?.doubleValue ?? 720
        let rect = NSRect(x: 0, y: 0, width: width, height: height)

        let newWindow = NSWindow(
            contentRect: rect,
            styleMask: [.titled, .closable, .miniaturizable, .resizable],
            backing: .buffered,
            defer: false
        )
        newWindow.minSize = NSSize(width: 700, height: 450)
        newWindow.tabbingIdentifier = windowTabbingIdentifier
        newWindow.tabbingMode = .automatic
        newWindow.delegate = self
        // window.isReleasedWhenClosed defaults to true, which has AppKit
        // release the window itself the moment it closes. We manage this
        // window's lifetime ourselves via `secondaryWindows` (torn down in
        // windowWillClose below), so that default would fight our own
        // bookkeeping — turn it off.
        newWindow.isReleasedWhenClosed = false

        // Cascade from whichever window was frontmost (falling back to the
        // main panel window) so successive popups don't stack exactly on
        // top of each other.
        let anchor = NSApp.keyWindow ?? window
        if let anchor = anchor {
            let topLeft = NSPoint(x: anchor.frame.minX, y: anchor.frame.maxY)
            _ = newWindow.cascadeTopLeft(from: topLeft)
        }

        let newWebView = WKWebView(frame: rect, configuration: configuration)
        newWebView.autoresizingMask = [.width, .height]
        newWebView.navigationDelegate = self
        newWebView.uiDelegate = self
        newWindow.contentView = newWebView

        let entry = SecondaryWindowEntry(window: newWindow, webView: newWebView)
        entry.titleObservation = newWebView.observe(\.title, options: [.new]) { [weak newWindow] _, change in
            guard let title = change.newValue.flatMap({ $0 }), !title.isEmpty else { return }
            newWindow?.title = title
        }
        secondaryWindows.append(entry)

        newWindow.makeKeyAndOrderFront(nil)
        return newWebView
    }

    // Which of our windows (main or secondary) a given webview lives in —
    // used to route JS dialogs and the native-picker bridge to the right
    // window instead of always the main one.
    func windowFor(_ webView: WKWebView) -> NSWindow? {
        if webView === self.webView { return window }
        return secondaryWindows.first(where: { $0.webView === webView })?.window
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

// MARK: - NSWindowDelegate
//
// Secondary (terminal/project) windows depend on the main window's child
// `mullion ui` server, so closing the main window takes all of them down
// too — at which point applicationShouldTerminateAfterLastWindowClosed
// (already true) quits the app for us. Closing a secondary window on its
// own just stops tracking it so it can be freed.
extension AppDelegate: NSWindowDelegate {
    func windowWillClose(_ notification: Notification) {
        guard let closedWindow = notification.object as? NSWindow else { return }

        if closedWindow === window {
            for entry in secondaryWindows {
                entry.window.close()
            }
            return
        }

        if let index = secondaryWindows.firstIndex(where: { $0.window === closedWindow }) {
            secondaryWindows[index].titleObservation?.invalidate()
            secondaryWindows.remove(at: index)
        }
    }
}

// MARK: - WKNavigationDelegate / WKUIDelegate

extension AppDelegate: WKNavigationDelegate, WKUIDelegate {
    // Keep the panel's own host+port inside the app window; send everything
    // else (site links like https://site.test, external http/https) to the
    // user's default browser instead. Applies to every webview we own
    // (main window and secondary windows alike), since they all share the
    // same navigation delegate.
    func webView(_ webView: WKWebView, decidePolicyFor navigationAction: WKNavigationAction, decisionHandler: @escaping (WKNavigationActionPolicy) -> Void) {
        guard let url = navigationAction.request.url, let panel = panelURL else {
            decisionHandler(.allow)
            return
        }
        if isSameOrigin(url, panel) {
            decisionHandler(.allow)
        } else {
            decisionHandler(.cancel)
            NSWorkspace.shared.open(url)
        }
    }

    // window.open / target=_blank. The panel uses this for two same-origin
    // popups — a standalone terminal window and a project window — which
    // get a real NSWindow + WKWebView of their own (sharing the
    // configuration WebKit hands us, so window.opener/postMessage and the
    // session/cookies keep working). Anything not same-origin as the panel
    // (external links) still goes to the default browser, same as before.
    func webView(_ webView: WKWebView, createWebViewWith configuration: WKWebViewConfiguration, for navigationAction: WKNavigationAction, windowFeatures: WKWindowFeatures) -> WKWebView? {
        guard let url = navigationAction.request.url, let panel = panelURL, isSameOrigin(url, panel) else {
            if let url = navigationAction.request.url {
                NSWorkspace.shared.open(url)
            }
            return nil
        }
        // WebKit loads navigationAction's request into whatever webview we
        // return here, so we don't load it ourselves.
        return openSecondaryWindow(configuration: configuration, windowFeatures: windowFeatures)
    }

    // JS called window.close() on one of our popup windows.
    func webViewDidClose(_ webView: WKWebView) {
        windowFor(webView)?.close()
    }

    func webView(_ webView: WKWebView, runJavaScriptAlertPanelWithMessage message: String, initiatedByFrame frame: WKFrameInfo, completionHandler: @escaping () -> Void) {
        let alert = NSAlert()
        alert.messageText = message
        alert.addButton(withTitle: "OK")
        if let hostWindow = windowFor(webView) {
            alert.beginSheetModal(for: hostWindow) { _ in completionHandler() }
        } else {
            alert.runModal()
            completionHandler()
        }
    }

    func webView(_ webView: WKWebView, runJavaScriptConfirmPanelWithMessage message: String, initiatedByFrame frame: WKFrameInfo, completionHandler: @escaping (Bool) -> Void) {
        let alert = NSAlert()
        alert.messageText = message
        alert.addButton(withTitle: "OK")
        alert.addButton(withTitle: "Cancel")
        if let hostWindow = windowFor(webView) {
            alert.beginSheetModal(for: hostWindow) { response in
                completionHandler(response == .alertFirstButtonReturn)
            }
        } else {
            completionHandler(alert.runModal() == .alertFirstButtonReturn)
        }
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
        if let hostWindow = windowFor(webView) {
            alert.beginSheetModal(for: hostWindow) { response in
                completionHandler(response == .alertFirstButtonReturn ? field.stringValue : nil)
            }
        } else {
            let response = alert.runModal()
            completionHandler(response == .alertFirstButtonReturn ? field.stringValue : nil)
        }
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
              let type = body["type"] as? String,
              // Whichever webview posted this — main panel, a terminal
              // window, or a project window — is the one whose reply
              // (__mullionPicked) and sheet the answer must go to.
              let sourceWebView = message.webView
        else { return }

        switch type {
        case "pickFolder":
            presentFolderPicker(id: body["id"], title: body["title"] as? String, webView: sourceWebView)
        case "pickFile":
            presentFilePicker(id: body["id"], title: body["title"] as? String, types: body["types"] as? [String], webView: sourceWebView)
        default:
            break
        }
    }

    func presentFolderPicker(id: Any?, title: String?, webView: WKWebView) {
        guard let id = id, let hostWindow = windowFor(webView) else { return }

        let panel = NSOpenPanel()
        panel.canChooseDirectories = true
        panel.canChooseFiles = false
        panel.allowsMultipleSelection = false
        panel.prompt = "Choose"
        panel.message = (title?.isEmpty == false) ? title! : "Choose the project folder"

        panel.beginSheetModal(for: hostWindow) { [weak self] response in
            let path = (response == .OK) ? panel.url?.path : nil
            self?.sendPickedFolder(id: id, path: path, webView: webView)
        }
    }

    // Same idea as presentFolderPicker, but for a single file — used by
    // the "Import .sql…" restore button, which needs a real path to a
    // .sql/.sql.gz backup rather than a directory. types are extensions
    // without the leading dot (e.g. "sql", "gz"); unrecognized extensions
    // are simply skipped, leaving the picker unfiltered.
    func presentFilePicker(id: Any?, title: String?, types: [String]?, webView: WKWebView) {
        guard let id = id, let hostWindow = windowFor(webView) else { return }

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

        panel.beginSheetModal(for: hostWindow) { [weak self] response in
            let path = (response == .OK) ? panel.url?.path : nil
            self?.sendPickedFolder(id: id, path: path, webView: webView)
        }
    }

    // Encodes (id, path) as a JSON array and unwraps its brackets to build
    // the argument list — the safe way to splice arbitrary/possibly-nil
    // strings into a JS call without hand-rolling escaping.
    func sendPickedFolder(id: Any, path: String?, webView: WKWebView) {
        guard let data = try? JSONSerialization.data(withJSONObject: [id, path ?? NSNull()]),
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
