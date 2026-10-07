// aside — KakaoTalk accessibility bridge for macOS.
//
// The KakaoTalk desktop app exposes its UI through the system Accessibility
// (AX) tree. This file drives that tree: it reads the chat list and open
// conversations, and types into the composer to send. There is no protocol
// client and no message store — KakaoTalk is the only thing that ever talks
// to Kakao's servers.
//
// Every entry point is exported as a C function (@_cdecl) so the whole file
// links straight into the Go binary; requests arrive as one JSON line and
// leave as one JSON line.

import ApplicationServices
import AppKit
import Foundation

// MARK: - Constants

private enum Kakao {
  static let bundleID = "com.kakao.KakaoTalkMac"

  // KakaoTalk ships localized. Every label we match on is listed in both
  // Korean and English so the bridge behaves the same regardless of the
  // app's language.
  static let listWindowNames: Set<String> = ["카카오톡", "KakaoTalk"]
  static let windowMenuNames: Set<String> = ["창", "Window"]
  static let chatsItemNames: Set<String> = ["채팅", "Chats"]
  static let sendButtonNames: Set<String> = ["전송", "보내기", "Send"]
  static let editedMarks: Set<String> = ["수정됨", "Edited"]
  static let deletedMarks: Set<String> = ["삭제됨", "Deleted"]

  // Status/placeholder rows that are not real messages.
  static let syntheticRows: Set<String> = [
    "여기까지 읽었습니다.", "여기까지 읽었습니다",
    "메시지가 삭제되었습니다.", "메시지가 삭제되었습니다",
    "This message has been deleted.", "This message has been deleted",
    "Message deleted.", "Message deleted",
    "You've read up to here.", "You've read up to here",
  ]
}

private enum VKey {
  static let ret: CGKeyCode = 36
  static let space: CGKeyCode = 49
  static let delete: CGKeyCode = 51
}

// A line that is only a timestamp — "오전 10:57", "오후 3:02", "10:57 AM",
// "3:02 pm" — with an optional leading date fragment on its own line.
private let timestampLine = try! NSRegularExpression(
  pattern: #"^((오전|오후)\s?\d{1,2}:\d{2}|\d{1,2}:\d{2}\s?([AaPp][Mm]))$"#
)

private func looksLikeTimestamp(_ line: String) -> Bool {
  let span = NSRange(line.startIndex..., in: line)
  return timestampLine.firstMatch(in: line, range: span) != nil
}

enum BridgeError: Error, CustomStringConvertible {
  case message(String)
  var description: String {
    switch self { case .message(let text): return text }
  }
}

// MARK: - AXUIElement conveniences
//
// The raw AX API is a set of C calls that take an attribute name and hand
// back an untyped CFTypeRef. Wrapping them as element methods keeps the rest
// of the file readable — `row.children` instead of a copy-into-out-param.

private extension AXUIElement {
  func rawValue(_ attribute: String) -> CFTypeRef? {
    var out: CFTypeRef?
    return AXUIElementCopyAttributeValue(self, attribute as CFString, &out) == .success ? out : nil
  }

  func text(_ attribute: String) -> String? { rawValue(attribute) as? String }
  func flag(_ attribute: String) -> Bool { rawValue(attribute) as? Bool ?? false }

  @discardableResult
  func assign(_ attribute: String, _ value: CFTypeRef) -> Bool {
    AXUIElementSetAttributeValue(self, attribute as CFString, value) == .success
  }

  @discardableResult
  func invoke(_ action: String) -> Bool {
    AXUIElementPerformAction(self, action as CFString) == .success
  }

  var actionNames: [String] {
    var names: CFArray?
    guard AXUIElementCopyActionNames(self, &names) == .success else { return [] }
    return names as? [String] ?? []
  }

  var isPressable: Bool { actionNames.contains(kAXPressAction as String) }

  var children: [AXUIElement] {
    rawValue(kAXChildrenAttribute as String) as? [AXUIElement] ?? []
  }

  var axRole: String { text(kAXRoleAttribute as String) ?? "" }

  // KakaoTalk puts a node's text in AXTitle for some roles and AXValue for
  // others; callers just want "whatever text this shows".
  var caption: String {
    text(kAXTitleAttribute as String) ?? text(kAXValueAttribute as String) ?? ""
  }

  var origin: CGPoint? {
    guard let boxed = rawValue(kAXPositionAttribute as String) else { return nil }
    let value = unsafeBitCast(boxed, to: AXValue.self)
    guard AXValueGetType(value) == .cgPoint else { return nil }
    var point = CGPoint.zero
    return AXValueGetValue(value, .cgPoint, &point) ? point : nil
  }

  var extent: CGSize? {
    guard let boxed = rawValue(kAXSizeAttribute as String) else { return nil }
    let value = unsafeBitCast(boxed, to: AXValue.self)
    guard AXValueGetType(value) == .cgSize else { return nil }
    var size = CGSize.zero
    return AXValueGetValue(value, .cgSize, &size) ? size : nil
  }

  func firstChild(role: String) -> AXUIElement? {
    children.first { $0.axRole == role }
  }

  func childrenWith(role: String) -> [AXUIElement] {
    children.filter { $0.axRole == role }
  }

  // Pre-order depth-first walk, in document order. Callers depend on the
  // order (e.g. a chat-list cell's first static text is the room name, its
  // last is the timestamp), so this must not reorder.
  func collect(role wanted: String) -> [AXUIElement] {
    var found: [AXUIElement] = []
    var stack = children.reversed().map { $0 }
    while let node = stack.popLast() {
      if node.axRole == wanted { found.append(node) }
      stack.append(contentsOf: node.children.reversed())
    }
    return found
  }

  func firstDescendant(role wanted: String) -> AXUIElement? {
    var stack = children.reversed().map { $0 }
    while let node = stack.popLast() {
      if node.axRole == wanted { return node }
      stack.append(contentsOf: node.children.reversed())
    }
    return nil
  }
}

// A chat-list label that could name a sender, filtering out timestamps,
// edit/delete marks, and bare unread counts.
private func candidateSender(_ raw: String) -> String? {
  let trimmed = raw.trimmingCharacters(in: .whitespacesAndNewlines)
  guard !trimmed.isEmpty else { return nil }
  if Kakao.editedMarks.contains(trimmed) || Kakao.deletedMarks.contains(trimmed) { return nil }
  let lines = trimmed.split(whereSeparator: \.isNewline).map {
    $0.trimmingCharacters(in: .whitespaces)
  }
  if lines.contains(where: looksLikeTimestamp) { return nil }
  if trimmed.range(of: #"^\d+\+?$"#, options: .regularExpression) != nil { return nil }
  return trimmed
}

// MARK: - Driver

final class KakaoDriver {
  // Resolving the composer walks a fair chunk of tree, so cache it per room.
  private var composerByWindow: [String: (field: AXUIElement, send: AXUIElement)] = [:]

  // MARK: process / app element

  private func kakaoApp() throws -> NSRunningApplication {
    // NSWorkspace.shared.runningApplications is a snapshot that only refreshes
    // on a runloop turn, which never happens in this Go-hosted process. The
    // class query below hits live state on every call.
    guard let app = NSRunningApplication
      .runningApplications(withBundleIdentifier: Kakao.bundleID).first else {
      throw BridgeError.message("KakaoTalk is not running.")
    }
    return app
  }

  private func appElement() throws -> AXUIElement {
    AXUIElementCreateApplication(try kakaoApp().processIdentifier)
  }

  // Live frontmost app, for the same runloop reason: read it off the AX
  // system-wide element instead of NSWorkspace.frontmostApplication.
  private func currentFrontApp() -> NSRunningApplication? {
    let system = AXUIElementCreateSystemWide()
    guard let focused = system.rawValue("AXFocusedApplication") else { return nil }
    var pid: pid_t = 0
    guard AXUIElementGetPid(unsafeBitCast(focused, to: AXUIElement.self), &pid) == .success else {
      return nil
    }
    return NSRunningApplication(processIdentifier: pid)
  }

  private func openWindows() throws -> [AXUIElement] {
    try appElement().rawValue(kAXWindowsAttribute as String) as? [AXUIElement] ?? []
  }

  private func window(titled name: String) throws -> AXUIElement {
    guard let match = try openWindows().first(where: { $0.caption == name }) else {
      throw BridgeError.message("KakaoTalk window not found: \(name)")
    }
    return match
  }

  private func listWindow() throws -> AXUIElement? {
    try openWindows().first { Kakao.listWindowNames.contains($0.caption) }
  }

  // The chat list lives in a scroll area holding a table and its scroll bar.
  private func chatList() throws -> (window: AXUIElement, table: AXUIElement, bar: AXUIElement) {
    guard let win = try listWindow() else {
      throw BridgeError.message("KakaoTalk main window not found.")
    }
    guard let scroll = win.firstChild(role: kAXScrollAreaRole as String),
          let table = scroll.firstChild(role: kAXTableRole as String),
          let bar = scroll.firstChild(role: kAXScrollBarRole as String) else {
      throw BridgeError.message("KakaoTalk conversation list not found.")
    }
    return (win, table, bar)
  }

  private func listRows(_ table: AXUIElement) -> [AXUIElement] {
    table.childrenWith(role: kAXRowRole as String)
  }

  // MARK: diagnostics / lifecycle

  func doctor() -> [String: Any] {
    var running = false
    var titles: [String] = []
    if let app = try? kakaoApp() {
      running = true
      let element = AXUIElementCreateApplication(app.processIdentifier)
      let windows = element.rawValue(kAXWindowsAttribute as String) as? [AXUIElement] ?? []
      titles = windows.map { $0.caption }
    }
    return [
      "trusted": AXIsProcessTrusted(),
      "running": running,
      "mainWindow": titles.contains { Kakao.listWindowNames.contains($0) },
      "windows": titles,
    ]
  }

  // Sends KakaoTalk to the hidden state (the Cmd+H effect). Every window
  // leaves the screen but the AX tree stays fully live, so we can keep
  // reading and typing. NSRunningApplication.hide() is refused here, but
  // setting the AX hidden attribute is honoured. Idempotent.
  func hide() -> Bool {
    guard let app = try? kakaoApp() else { return false }
    let element = AXUIElementCreateApplication(app.processIdentifier)
    return element.assign(kAXHiddenAttribute as String, kCFBooleanTrue)
  }

  // Presses Window ▸ Chats in the menu bar. Menu items accept AXPress from
  // the background, so no activation is required.
  private func pressChatsMenuItem() -> Bool {
    guard let app = try? appElement(),
          let menuBar = app.firstChild(role: kAXMenuBarRole as String),
          let windowMenu = menuBar.children.first(where: { Kakao.windowMenuNames.contains($0.caption) }),
          let dropdown = windowMenu.children.first,
          let chats = dropdown.collect(role: kAXMenuItemRole as String)
            .first(where: { Kakao.chatsItemNames.contains($0.caption) }) else {
      return false
    }
    return chats.invoke(kAXPressAction as String)
  }

  // For cold starts: nudge the list open (background only) and report whether
  // it exists yet. Callers poll this while KakaoTalk boots and auto-logs-in.
  func pokeListWindow() -> Bool {
    if (try? listWindow()) ?? nil != nil { return true }
    guard pressChatsMenuItem() else { return false }
    usleep(400_000)
    return (try? listWindow()) ?? nil != nil
  }

  // Guarantees the chat list exists. Tries the quiet background press first;
  // only if that fails does it briefly bring KakaoTalk forward, restoring
  // focus to whatever app was in front afterwards.
  //
  // The window existing is not enough: on the Friends tab, the lock screen,
  // or the login view the main window is present but holds no chat table —
  // so the success test is chatList() resolving, not the window title. The
  // Window ▸ Chats press also switches the tab, which covers the Friends case.
  func ensureList() throws {
    if (try? chatList()) != nil { return }
    guard pressChatsMenuItem() else {
      throw BridgeError.message("KakaoTalk Chats menu item not found.")
    }
    usleep(500_000)
    if (try? chatList()) != nil { return }

    let front = currentFrontApp()
    let app = try kakaoApp()
    app.activate()
    usleep(200_000)
    let pressed = pressChatsMenuItem()
    usleep(500_000)
    if let front, front != app { front.activate() }

    if (try? chatList()) == nil {
      throw BridgeError.message(pressed
        ? "KakaoTalk chat list did not appear — locked or waiting for login?"
        : "Failed to open the KakaoTalk chat list.")
    }
  }

  // Closes a conversation window aside opened. The list window is never
  // touched; a window that is already gone is treated as success.
  func close(window name: String) -> Bool {
    if name.isEmpty || Kakao.listWindowNames.contains(name) { return false }
    guard let windows = try? openWindows() else { return false }
    guard let target = windows.first(where: { $0.caption == name }) else { return true }
    guard let button = target.rawValue(kAXCloseButtonAttribute as String) else { return false }
    return unsafeBitCast(button, to: AXUIElement.self).invoke(kAXPressAction as String)
  }

  // MARK: keyboard delivery

  // Delivers one key press (down+up) straight to the KakaoTalk process, so
  // it works while the app is hidden and never disturbs the foreground.
  private func tap(_ key: CGKeyCode) throws {
    let pid = try kakaoApp().processIdentifier
    for down in [true, false] {
      guard let event = CGEvent(keyboardEventSource: nil, virtualKey: key, keyDown: down) else {
        throw BridgeError.message("Failed to create KakaoTalk key event.")
      }
      event.postToPid(pid)
      usleep(30_000)
    }
  }

  private func awaitWindow(_ name: String, timeout: TimeInterval) -> Bool {
    let end = Date().addingTimeInterval(timeout)
    repeat {
      if let windows = try? openWindows(), windows.contains(where: { $0.caption == name }) {
        return true
      }
      usleep(150_000)
    } while Date() < end
    return false
  }

  // MARK: opening a room

  // Scrolls a row into view and returns its live row element by matching the
  // title. Row objects are recycled as the list virtualizes, so we always
  // re-read and match by name rather than trusting an index.
  private func revealRow(_ table: AXUIElement, bar: AXUIElement, index: Int, title: String) throws {
    let count = listRows(table).count
    if index > 5 {
      let span = max(1, count - 5)
      table.assign(kAXValueAttribute as String, NSNumber(value: 0)) // no-op guard
      bar.assign(kAXValueAttribute as String, NSNumber(value: Double(index - 4) / Double(span)))
    } else {
      bar.assign(kAXValueAttribute as String, NSNumber(value: 0))
    }
    usleep(350_000)
  }

  private func rowLabel(_ row: AXUIElement) -> String {
    row.children.first?.firstDescendant(role: kAXStaticTextRole as String)?.caption ?? ""
  }

  // Selects the target row and presses Return, entirely in the background.
  // Selection binds to the row object, so if the list reorders between now
  // and the key press we still open the intended room (never a coordinate
  // click, which was the source of "wrong room opened" races). Returns true
  // once the room's window appears.
  private func selectThenEnter(table: AXUIElement, hint: Int, title: String) throws -> Bool {
    let rows = listRows(table)
    var target: AXUIElement?
    let hintIndex = hint - 1
    if hintIndex >= 0, hintIndex < rows.count, rowLabel(rows[hintIndex]) == title {
      target = rows[hintIndex]
    } else {
      target = rows.first { rowLabel($0) == title }
    }
    guard let row = target else {
      throw BridgeError.message("KakaoTalk conversation order changed; retry.")
    }

    var selected = table.assign("AXSelectedRows", [row] as CFArray)
    if !selected { selected = row.assign(kAXSelectedAttribute as String, kCFBooleanTrue) }
    guard selected else { return false }

    table.assign(kAXFocusedAttribute as String, kCFBooleanTrue)
    usleep(150_000)
    // Only press Return if the list actually holds keyboard focus — otherwise
    // the key could fall into an already-open composer and mis-send.
    guard let focused = try appElement().rawValue(kAXFocusedUIElementAttribute as String),
          CFEqual(focused, table) else {
      return false
    }
    try tap(VKey.ret)
    return awaitWindow(title, timeout: 2.5)
  }

  // Opens a conversation. Returns true if this call created the window, false
  // if it was already open (so the caller knows whether to close it later).
  // Ladder: two quiet background attempts, then one with KakaoTalk briefly
  // activated for the case where it withholds keyboard focus in the
  // background. The stealth hide is restored at the end.
  func open(row index: Int, title: String) throws -> Bool {
    try ensureList()
    if try openWindows().contains(where: { $0.caption == title }) { return false }

    let (listWin, table, bar) = try chatList()
    let rows = listRows(table)
    guard index > 0, index <= rows.count else {
      throw BridgeError.message("KakaoTalk conversation row missing.")
    }
    try revealRow(table, bar: bar, index: index, title: title)

    // Selecting/focusing the list can pull KakaoTalk out of the hidden
    // state, so whichever branch opens the room, re-hide before returning.
    for _ in 0..<2 {
      if try selectThenEnter(table: table, hint: index, title: title) {
        _ = hide()
        return true
      }
      usleep(250_000)
    }

    let front = currentFrontApp()
    let app = try kakaoApp()
    app.activate()
    listWin.assign(kAXMinimizedAttribute as String, kCFBooleanFalse)
    listWin.invoke(kAXRaiseAction as String)
    usleep(250_000)
    let opened = try selectThenEnter(table: table, hint: index, title: title)
    if let front, front != app { front.activate() }
    _ = hide()

    guard opened else {
      throw BridgeError.message("Failed to open KakaoTalk chat window: \(title)")
    }
    return true
  }

  // MARK: reading

  func conversations(limit: Int) throws -> [[String: Any]] {
    try ensureList()
    let (_, table, bar) = try chatList()
    bar.assign(kAXValueAttribute as String, NSNumber(value: 0))
    defer { bar.assign(kAXValueAttribute as String, NSNumber(value: 0)) }
    usleep(250_000)

    var rows = listRows(table)
    let wanted = min(max(1, limit), rows.count)
    let page = 8
    var out: [[String: Any]] = []

    for i in 0..<wanted {
      // Rows outside the viewport are virtualized — their child labels can
      // still read as a neighbouring room's until scrolled in. Page down and
      // re-read the row set before trusting labels past the first screen.
      if i > 0 && i % page == 0 {
        let span = max(1, rows.count - 5)
        let fraction = min(1, Double(max(0, i + 1 - 4)) / Double(span))
        bar.assign(kAXValueAttribute as String, NSNumber(value: fraction))
        usleep(250_000)
        rows = listRows(table)
      }
      guard i < rows.count, let cell = rows[i].children.first else { continue }

      let labels = cell.collect(role: kAXStaticTextRole as String)
      guard let name = labels.first?.caption, !name.isEmpty else { continue }
      let preview = cell.firstDescendant(role: kAXTextAreaRole as String)?.caption ?? ""
      out.append([
        "row": i + 1,
        "title": name,
        "preview": preview,
        "time": labels.last?.caption ?? "",
        "unread": labels.count >= 3,
      ])
    }
    return out
  }

  // Whether a message row carries readable text (media rows do not).
  private func readableText(in row: AXUIElement) -> String? {
    guard let cell = row.children.first else { return nil }
    guard let field = cell.children.last(where: { $0.axRole == kAXTextAreaRole as String }),
          let raw = field.text(kAXValueAttribute as String) else { return nil }
    let value = raw.trimmingCharacters(in: .whitespacesAndNewlines)
    if value.isEmpty || Kakao.syntheticRows.contains(value) { return nil }
    return value
  }

  func messages(window name: String, direction: String, limit: Int) throws -> [[String: Any]] {
    let chat = try window(titled: name)
    guard let winOrigin = chat.origin, let winSize = chat.extent else {
      throw BridgeError.message("Failed to read KakaoTalk chat window coordinates.")
    }
    guard let table = chat.childrenWith(role: kAXScrollAreaRole as String)
      .first?.firstChild(role: kAXTableRole as String) else {
      throw BridgeError.message("KakaoTalk message table not found.")
    }

    let rows = listRows(table)
    let want = max(1, min(20, limit))
    // Photos, videos and big emoticons occupy their own text-less rows.
    // Counting raw rows undershoots readable messages, so gather rows until
    // `want` of them actually contain text.
    let ordered = direction == "older" ? rows : Array(rows.reversed())
    var picked: [AXUIElement] = []
    var readable = 0
    for row in ordered {
      picked.append(row)
      if readableText(in: row) != nil { readable += 1 }
      if readable >= want { break }
    }
    if direction != "older" { picked.reverse() }

    var out: [[String: Any]] = []
    var lastSender = ""
    for row in picked {
      guard let cell = row.children.first else { continue }
      var fields: [AXUIElement] = []
      var labels: [AXUIElement] = []
      let textAreaRole = kAXTextAreaRole as String
      let staticTextRole = kAXStaticTextRole as String
      for node in cell.children {
        let r = node.axRole
        if r == textAreaRole { fields.append(node) }
        else if r == staticTextRole { labels.append(node) }
      }
      // A row with no text area is media — a photo, a video or a large
      // emoticon. Those used to be dropped, which made conversations read
      // as if nothing had been sent; emit them with their on-screen rect
      // so the caller can show a placeholder and capture the bubble.
      guard let field = fields.last,
            let raw = field.text(kAXValueAttribute as String), !raw.isEmpty,
            let msgOrigin = field.origin, let msgSize = field.extent else {
        if let media = mediaEntry(cell: cell, winOrigin: winOrigin, winSize: winSize, lastSender: &lastSender) {
          out.append(media)
        }
        continue
      }

      let body = raw.trimmingCharacters(in: .whitespacesAndNewlines)
      if Kakao.syntheticRows.contains(body) { continue }

      let edited = labels.contains {
        Kakao.editedMarks.contains($0.caption.trimmingCharacters(in: .whitespacesAndNewlines))
      }

      // Sender is decided by which edge the bubble hugs — right is mine, left
      // is theirs. Geometry is the same in every locale, unlike text labels.
      let leftGap = msgOrigin.x - winOrigin.x
      let rightGap = (winOrigin.x + winSize.width) - (msgOrigin.x + msgSize.width)
      var mine = true
      var sender = ""
      if leftGap <= rightGap {
        mine = false
        for label in labels {
          if let name = candidateSender(label.caption) { lastSender = name }
        }
        sender = lastSender
      }
      out.append(["kind": "text", "text": body, "sender": sender, "mine": mine, "edited": edited])
    }
    return out
  }

  // mediaEntry describes a text-less row: what kind of thing it is, who
  // sent it, and where its bubble sits on screen.
  private func mediaEntry(cell: AXUIElement, winOrigin: CGPoint, winSize: CGSize,
                          lastSender: inout String) -> [String: Any]? {
    let image = cell.firstDescendant(role: kAXImageRole as String)
    let anchor = image ?? cell
    guard let origin = anchor.origin, let size = anchor.extent,
          size.width > 24, size.height > 24 else { return nil }

    let leftGap = origin.x - winOrigin.x
    let rightGap = (winOrigin.x + winSize.width) - (origin.x + size.width)
    var mine = true
    var sender = ""
    if leftGap <= rightGap {
      mine = false
      for label in cell.children where label.axRole == kAXStaticTextRole as String {
        if let name = candidateSender(label.caption) { lastSender = name }
      }
      sender = lastSender
    }

    // AXDescription usually carries the word KakaoTalk uses for the kind
    let described = (image?.text(kAXDescriptionAttribute as String) ?? "")
      .trimmingCharacters(in: .whitespacesAndNewlines)
    return [
      "kind": "media",
      "text": described,
      "sender": sender,
      "mine": mine,
      "edited": false,
      "x": Double(origin.x), "y": Double(origin.y),
      "w": Double(size.width), "h": Double(size.height),
    ]
  }

  // windowNumber finds the CoreGraphics window id for one of KakaoTalk's
  // windows by matching the frame the accessibility tree reports.
  private func windowNumber(for chat: AXUIElement) -> CGWindowID? {
    guard let origin = chat.origin, let size = chat.extent,
          let pid = try? kakaoApp().processIdentifier else { return nil }
    let infos = CGWindowListCopyWindowInfo([.optionAll, .excludeDesktopElements], kCGNullWindowID)
      as? [[String: Any]] ?? []
    for info in infos {
      guard (info[kCGWindowOwnerPID as String] as? pid_t) == pid,
            let bounds = info[kCGWindowBounds as String] as? [String: Any],
            let x = bounds["X"] as? Double, let y = bounds["Y"] as? Double,
            let w = bounds["Width"] as? Double, let h = bounds["Height"] as? Double else { continue }
      if abs(x - Double(origin.x)) < 6, abs(y - Double(origin.y)) < 6,
         abs(w - Double(size.width)) < 6, abs(h - Double(size.height)) < 6 {
        return CGWindowID(info[kCGWindowNumber as String] as? Int ?? 0)
      }
    }
    return nil
  }

  // capture writes the bubble at the given screen rect to a PNG. The window
  // is captured by id, which works while KakaoTalk is hidden — nothing is
  // brought on screen, so the disguise holds.
  func capture(window name: String, x: Double, y: Double, w: Double, h: Double,
               to path: String) throws -> [String: Any] {
    let chat = try window(titled: name)
    guard let winOrigin = chat.origin, let winSize = chat.extent else {
      throw BridgeError.message("Failed to read the chat window frame.")
    }
    guard let id = windowNumber(for: chat) else {
      throw BridgeError.message("Could not find the chat window to capture.")
    }

    let shot = NSTemporaryDirectory() + "aside-shot-\(id).png"
    let task = Process()
    task.executableURL = URL(fileURLWithPath: "/usr/sbin/screencapture")
    task.arguments = ["-x", "-o", "-l", String(id), shot]
    try task.run()
    task.waitUntilExit()
    defer { try? FileManager.default.removeItem(atPath: shot) }

    guard task.terminationStatus == 0,
          let source = CGImageSourceCreateWithURL(URL(fileURLWithPath: shot) as CFURL, nil),
          let full = CGImageSourceCreateImageAtIndex(source, 0, nil) else {
      throw BridgeError.message("Screen capture failed — grant Screen Recording to this terminal.")
    }

    // the capture may be at Retina scale; map the screen rect into it
    let scale = winSize.width > 0 ? Double(full.width) / Double(winSize.width) : 1
    var crop = CGRect(
      x: (x - Double(winOrigin.x)) * scale,
      y: (y - Double(winOrigin.y)) * scale,
      width: w * scale, height: h * scale
    ).integral
    crop = crop.intersection(CGRect(x: 0, y: 0, width: full.width, height: full.height))
    guard !crop.isEmpty, let cropped = full.cropping(to: crop) else {
      throw BridgeError.message("The bubble is outside the captured window.")
    }

    let rep = NSBitmapImageRep(cgImage: cropped)
    guard let png = rep.representation(using: .png, properties: [:]) else {
      throw BridgeError.message("Could not encode the capture.")
    }
    try png.write(to: URL(fileURLWithPath: path))
    return ["path": path, "width": cropped.width, "height": cropped.height]
  }

  func scrollBack(window name: String) throws {
    let chat = try window(titled: name)
    guard let scroll = chat.firstChild(role: kAXScrollAreaRole as String),
          let bar = scroll.firstChild(role: kAXScrollBarRole as String) else {
      throw BridgeError.message("KakaoTalk message scroll bar not found.")
    }
    let at = (bar.rawValue(kAXValueAttribute as String) as? NSNumber)?.doubleValue ?? 1
    bar.assign(kAXValueAttribute as String, NSNumber(value: max(0, at - 0.25)))
    usleep(250_000)
  }

  // MARK: composing / sending

  // Locates the send button next to a composer field. Different KakaoTalk
  // builds label it differently (전송 / 보내기 / Send) and newer ones use a
  // split "Send ⌄" control whose AX title is empty, so a name match alone is
  // brittle. Falls back to geometry: the rightmost pressable button sitting
  // on the composer's row, past its left-edge toolbar — locale/version proof.
  private func sendButton(in chat: AXUIElement, field: AXUIElement) -> AXUIElement? {
    let buttons = chat.collect(role: kAXButtonRole as String)
    if let named = buttons.first(where: { Kakao.sendButtonNames.contains($0.caption) }) {
      return named
    }
    return sendButtonCandidates(in: chat, field: field).first?.element
  }

  // Candidate send buttons on the composer row, best first.
  //
  // Newer builds render a split control — a wide "send" body plus a narrow
  // chevron that only opens a menu. Picking the rightmost button grabs the
  // chevron, which opens a dropdown and sends nothing (the composer then
  // never clears, surfacing as a confirmation timeout), so rank by body
  // width and push menu-opening buttons last.
  private func sendButtonCandidates(
    in chat: AXUIElement, field: AXUIElement
  ) -> [(element: AXUIElement, caption: String, x: CGFloat, width: CGFloat, opensMenu: Bool)] {
    guard let fieldPos = field.origin, let fieldSize = field.extent else { return [] }
    let bandTop = fieldPos.y - 20
    let bandBottom = fieldPos.y + fieldSize.height + 60
    let rightOfToolbar = fieldPos.x + fieldSize.width * 0.4

    return chat.collect(role: kAXButtonRole as String)
      .compactMap { button in
        guard let p = button.origin, let s = button.extent else { return nil }
        let midY = p.y + s.height / 2
        guard midY >= bandTop, midY <= bandBottom else { return nil }
        guard p.x >= rightOfToolbar else { return nil } // left-edge toolbar
        guard s.width >= 30 else { return nil }         // chevrons, scroll arrows
        guard button.isPressable else { return nil }
        let opensMenu = button.actionNames.contains("AXShowMenu")
        return (button, button.caption, p.x, s.width, opensMenu)
      }
      .sorted { a, b in
        // a known label wins outright
        let aNamed = Kakao.sendButtonNames.contains(a.caption)
        let bNamed = Kakao.sendButtonNames.contains(b.caption)
        if aNamed != bNamed { return aNamed }
        // then: not a menu opener
        if a.opensMenu != b.opensMenu { return b.opensMenu }
        // then: the wider body of a split control
        if a.width != b.width { return a.width > b.width }
        return a.x > b.x
      }
  }

  private func composer(window name: String) throws -> (field: AXUIElement, send: AXUIElement) {
    if let cached = composerByWindow[name] { return cached }
    // The composer's input field and send button can render a beat after the
    // window itself, so give them a short window to appear before failing.
    let deadline = Date().addingTimeInterval(2.0)
    var lastMissing = "KakaoTalk composer not found."
    repeat {
      let chat = try window(titled: name)
      if let field = chat.childrenWith(role: kAXScrollAreaRole as String)
        .last?.firstDescendant(role: kAXTextAreaRole as String) {
        if let button = sendButton(in: chat, field: field) {
          let pair = (field: field, send: button)
          composerByWindow[name] = pair
          return pair
        }
        lastMissing = "KakaoTalk send button not found."
      }
      usleep(150_000)
    } while Date() < deadline
    throw BridgeError.message(lastMissing)
  }

  // Best-effort warm-up so the first send is snappy. Never fatal: if the
  // composer is still settling, send() resolves it later on its own.
  func warmComposer(window name: String) {
    _ = try? composer(window: name)
  }

  // Diagnostic: reports how the send button was resolved for a window,
  // without sending anything. Used by `aside probe`.
  func probeComposer(window name: String) throws -> [String: Any] {
    composerByWindow.removeValue(forKey: name)
    let chat = try window(titled: name)
    guard let field = chat.childrenWith(role: kAXScrollAreaRole as String)
      .last?.firstDescendant(role: kAXTextAreaRole as String) else {
      return ["fieldFound": false, "buttonFound": false]
    }
    // Layout facts that tell us whether the *field* and message table were
    // resolved correctly — a wrong field looks identical to a wrong button
    // from the error message alone.
    let scrollAreas = chat.childrenWith(role: kAXScrollAreaRole as String)
    let messageTable = scrollAreas.first?.firstChild(role: kAXTableRole as String)
    let fieldPos = field.origin ?? .zero
    let fieldSize = field.extent ?? .zero
    let layout: [String: Any] = [
      "scrollAreas": scrollAreas.count,
      "fieldX": Double(fieldPos.x),
      "fieldY": Double(fieldPos.y),
      "fieldW": Double(fieldSize.width),
      "fieldH": Double(fieldSize.height),
      "fieldEditable": field.flag(kAXEnabledAttribute as String),
      "messageTable": messageTable != nil,
      "messageRows": messageTable.map { listRows($0).count } ?? 0,
    ]

    let candidates = sendButtonCandidates(in: chat, field: field)
    let listed: [[String: Any]] = candidates.map {
      ["title": $0.caption, "x": Double($0.x), "width": Double($0.width), "opensMenu": $0.opensMenu]
    }
    guard let chosen = candidates.first else {
      // nothing qualified — dump every button on the row so the layout can be
      // diagnosed from the user's machine
      var all: [[String: Any]] = []
      if let fp = field.origin, let fs = field.extent {
        for b in chat.collect(role: kAXButtonRole as String) {
          guard let p = b.origin, let s = b.extent else { continue }
          let midY = p.y + s.height / 2
          guard midY >= fp.y - 40, midY <= fp.y + fs.height + 80 else { continue }
          all.append([
            "title": b.caption, "x": Double(p.x), "width": Double(s.width),
            "pressable": b.isPressable, "opensMenu": b.actionNames.contains("AXShowMenu"),
          ])
        }
      }
      return ["fieldFound": true, "buttonFound": false, "rowButtons": all, "layout": layout]
    }
    return [
      "fieldFound": true,
      "buttonFound": true,
      "title": chosen.caption,
      "x": Double(chosen.x),
      "width": Double(chosen.width),
      "opensMenu": chosen.opensMenu,
      "candidates": listed,
      "layout": layout,
    ]
  }

  private func waitEnabled(_ button: AXUIElement, timeout: TimeInterval) -> Bool {
    let end = Date().addingTimeInterval(timeout)
    repeat {
      if button.flag(kAXEnabledAttribute as String) { return true }
      usleep(20_000)
    } while Date() < end
    return false
  }

  // Writing the field via AXValue updates what's displayed but sometimes
  // doesn't trigger KakaoTalk's own change handler, leaving the send button
  // disabled while AXPress silently no-ops. Typing a space and immediately
  // deleting it forces a genuine edit event without altering the final text,
  // touching the clipboard, or pulling the app forward.
  private func wakeComposer(_ field: AXUIElement, text: String) throws {
    field.assign(kAXFocusedAttribute as String, kCFBooleanTrue)
    var caret = CFRange(location: text.utf16.count, length: 0)
    guard let caretValue = AXValueCreate(.cfRange, &caret) else {
      throw BridgeError.message("Failed to build KakaoTalk composer cursor position.")
    }
    field.assign(kAXSelectedTextRangeAttribute as String, caretValue)

    let pid = try kakaoApp().processIdentifier
    usleep(100_000)
    var spaceChar: [UniChar] = [32]
    for down in [true, false] {
      guard let e = CGEvent(keyboardEventSource: nil, virtualKey: VKey.space, keyDown: down) else {
        throw BridgeError.message("Failed to create KakaoTalk input event.")
      }
      e.keyboardSetUnicodeString(stringLength: spaceChar.count, unicodeString: &spaceChar)
      e.postToPid(pid)
    }
    usleep(100_000)
    for down in [true, false] {
      guard let e = CGEvent(keyboardEventSource: nil, virtualKey: VKey.delete, keyDown: down) else {
        throw BridgeError.message("Failed to create KakaoTalk input event.")
      }
      e.postToPid(pid)
    }
    usleep(100_000)
    guard field.text(kAXValueAttribute as String) == text else {
      throw BridgeError.message("Failed to prepare the KakaoTalk composer safely.")
    }
  }

  // Fresh read of the composer field's current text (not the cached element,
  // which can go stale after a send recreates the text area).
  private func liveComposerText(window name: String) -> String? {
    guard let chat = try? window(titled: name) else { return nil }
    return chat.childrenWith(role: kAXScrollAreaRole as String)
      .last?.firstDescendant(role: kAXTextAreaRole as String)?
      .text(kAXValueAttribute as String)
  }

  // Whether the newest readable message in the window is our own outgoing
  // copy of `text`. Second, version-proof confirmation signal for a send:
  // some builds don't clear the composer the way the primary check expects.
  private func lastMessageIsOurs(window name: String, text: String) -> Bool {
    guard let chat = try? window(titled: name),
          let winPos = chat.origin, let winSize = chat.extent,
          let table = chat.childrenWith(role: kAXScrollAreaRole as String)
            .first?.firstChild(role: kAXTableRole as String) else { return false }
    let target = text.trimmingCharacters(in: .whitespacesAndNewlines)
    for row in listRows(table).reversed() {
      guard let cell = row.children.first else { continue }
      let fields = cell.children.filter { $0.axRole == kAXTextAreaRole as String }
      guard let field = fields.last,
            let raw = field.text(kAXValueAttribute as String),
            let mp = field.origin, let ms = field.extent else { continue }
      let body = raw.trimmingCharacters(in: .whitespacesAndNewlines)
      if body.isEmpty || Kakao.syntheticRows.contains(body) { continue }
      // newest readable row only
      let leftGap = mp.x - winPos.x
      let rightGap = (winPos.x + winSize.width) - (mp.x + ms.width)
      let mine = leftGap > rightGap
      return mine && body == target
    }
    return false
  }

  func send(window name: String, text: String) throws -> [String: Bool] {
    var parts = try composer(window: name)
    if !parts.field.assign(kAXValueAttribute as String, text as CFString) {
      composerByWindow.removeValue(forKey: name)
      parts = try composer(window: name)
      guard parts.field.assign(kAXValueAttribute as String, text as CFString) else {
        throw BridgeError.message("Failed to set accessibility value.")
      }
    }

    if !waitEnabled(parts.send, timeout: 0.15) {
      try wakeComposer(parts.field, text: text)
    }
    if !waitEnabled(parts.send, timeout: 0.6) {
      // A cached element can outlive a recreated chat window; re-resolve once
      // before concluding the button is stuck.
      composerByWindow.removeValue(forKey: name)
      parts = try composer(window: name)
    }
    guard parts.field.text(kAXValueAttribute as String) == text else {
      throw BridgeError.message("KakaoTalk composer content does not match the outgoing message.")
    }
    guard waitEnabled(parts.send, timeout: 0.25) else {
      throw BridgeError.message("KakaoTalk send button did not become enabled.")
    }

    let pressed = parts.send.invoke(kAXPressAction as String)

    // Either signal confirms: the composer emptied, or our text is now the
    // newest outgoing message. Two signals because builds differ in how they
    // clear the composer.
    if confirmSent(window: name, text: text, within: 4) { return ["confirmed": true] }

    // The button press did nothing — some builds expose a send control whose
    // AXPress is inert (their label is empty too, so it may not even be the
    // real send button). Fall back to what a person does: press Return in the
    // composer. Guarded so this can never duplicate a message: it only runs
    // when our text is still sitting in the composer AND no outgoing copy has
    // appeared, i.e. nothing was sent.
    if liveComposerText(window: name) == text, !lastMessageIsOurs(window: name, text: text) {
      try pressReturnInComposer(window: name, field: parts.field, text: text)
      if confirmSent(window: name, text: text, within: 8) { return ["confirmed": true] }
    }

    throw BridgeError.message(pressed
      ? "KakaoTalk did not confirm the send within 12 seconds."
      : "KakaoTalk send action failed.")
  }

  private func confirmSent(window name: String, text: String, within seconds: TimeInterval) -> Bool {
    let deadline = Date().addingTimeInterval(seconds)
    repeat {
      if (liveComposerText(window: name) ?? "").isEmpty { return true }
      if lastMessageIsOurs(window: name, text: text) { return true }
      usleep(100_000)
    } while Date() < deadline
    return false
  }

  // Focuses the composer, verifies the keystroke will land there (never in
  // the room list or another window), and delivers Return to the KakaoTalk
  // process — the same path a person's Enter takes.
  private func pressReturnInComposer(window name: String, field: AXUIElement, text: String) throws {
    field.assign(kAXFocusedAttribute as String, kCFBooleanTrue)
    var caret = CFRange(location: text.utf16.count, length: 0)
    if let caretValue = AXValueCreate(.cfRange, &caret) {
      field.assign(kAXSelectedTextRangeAttribute as String, caretValue)
    }
    usleep(150_000)
    guard let focused = try appElement().rawValue(kAXFocusedUIElementAttribute as String),
          CFEqual(focused, field) else {
      return // composer never took focus; do not fire a stray Return
    }
    try tap(VKey.ret)
  }
}

// MARK: - C entry points

private let driver = KakaoDriver()
// AX calls aren't thread-safe against the driver's caches; serialize every
// request through one queue. (Go holds a mutex too — this guards any other
// caller.)
private let serialQueue = DispatchQueue(label: "aside.kakao.bridge")

private func dispatch(_ action: String, _ p: [String: Any]) throws -> Any {
  switch action {
  case "ping": return ["ready": true]
  case "doctor": return driver.doctor()
  case "hideApp": return ["hidden": driver.hide()]
  case "nudgeMain": return ["ready": driver.pokeListWindow()]
  case "ensureMain": try driver.ensureList(); return [:] as [String: Any]
  case "conversations":
    return try driver.conversations(limit: p["limit"] as? Int ?? 10)
  case "openRow":
    return ["opened": try driver.open(row: p["row"] as? Int ?? 0, title: p["title"] as? String ?? "")]
  case "closeWindow":
    return ["closed": driver.close(window: p["title"] as? String ?? "")]
  case "messages":
    return try driver.messages(
      window: p["title"] as? String ?? "",
      direction: p["direction"] as? String ?? "newer",
      limit: p["limit"] as? Int ?? 15)
  case "prepareComposer":
    driver.warmComposer(window: p["title"] as? String ?? ""); return [:] as [String: Any]
  case "probeComposer":
    return try driver.probeComposer(window: p["title"] as? String ?? "")
  case "send":
    return try driver.send(window: p["title"] as? String ?? "", text: p["text"] as? String ?? "")
  case "capture":
    return try driver.capture(
      window: p["title"] as? String ?? "",
      x: p["x"] as? Double ?? 0, y: p["y"] as? Double ?? 0,
      w: p["w"] as? Double ?? 0, h: p["h"] as? Double ?? 0,
      to: p["path"] as? String ?? "")
  case "scrollOlder":
    try driver.scrollBack(window: p["title"] as? String ?? ""); return [:] as [String: Any]
  default:
    throw BridgeError.message("Unsupported action: \(action)")
  }
}

private func jsonReply(_ payload: [String: Any]) -> UnsafeMutablePointer<CChar>? {
  guard let data = try? JSONSerialization.data(withJSONObject: payload),
        let string = String(data: data, encoding: .utf8) else {
    return strdup(#"{"ok":false,"error":"bridge response encoding failed"}"#)
  }
  return strdup(string)
}

@_cdecl("aside_request")
public func aside_request(_ raw: UnsafePointer<CChar>?) -> UnsafeMutablePointer<CChar>? {
  guard let raw else { return jsonReply(["ok": false, "error": "empty request"]) }
  let line = String(cString: raw)
  return serialQueue.sync {
    guard let data = line.data(using: .utf8),
          let request = (try? JSONSerialization.jsonObject(with: data)) as? [String: Any],
          let action = request["action"] as? String else {
      return jsonReply(["ok": false, "error": "malformed request: \(line)"])
    }
    do {
      return jsonReply(["ok": true, "result": try dispatch(action, request)])
    } catch {
      return jsonReply(["ok": false, "error": String(describing: error)])
    }
  }
}

@_cdecl("aside_free")
public func aside_free(_ pointer: UnsafeMutablePointer<CChar>?) {
  free(pointer)
}
