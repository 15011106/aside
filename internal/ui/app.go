// Package ui renders DMs as something that passes for an agent CLI session.
// The disguise imitates Claude Code's visual grammar: a welcome box, a
// /resume-style session picker for the room list, tool-call bullets with ⎿
// result lines for incoming messages, a thinking spinner with a token
// counter, and a rounded-border composer.
//
// Built on Bubble Tea v2 for its real-cursor support: the terminal cursor is
// positioned at the actual typing point, which is the only way macOS IMEs
// draw Korean composition (preedit) text in the right place.
package ui

import (
	"fmt"
	"os"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/lipgloss"

	"aside/internal/kakao"
)

const (
	modeRooms = iota
	modeChat
)

const pollInterval = 3 * time.Second
const spinInterval = 250 * time.Millisecond

var (
	dim       = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	claude    = lipgloss.NewStyle().Foreground(lipgloss.Color("173")) // Claude orange
	toolGreen = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	errStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	boldStyle = lipgloss.NewStyle().Bold(true)
	selStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("173")).Bold(true)
	boxStyle  = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("240")).
			Padding(0, 1)
)

var spinnerFrames = []string{"✻", "✼", "✽", "✼"}

type roomsMsg []kakao.Conversation
type openedMsg struct {
	title     string
	ownedByUs bool
}
type messagesMsg struct {
	title string
	items []kakao.Message
}
type sentMsg struct{}
type errMsg struct{ err error }
type reopenFailedMsg struct{ err error }
type tickMsg struct{}
type spinMsg struct{}

type model struct {
	mode    int
	width   int
	height  int
	rooms   []kakao.Conversation
	cursor  int
	room    string
	msgs    []kakao.Message
	vp      viewport.Model
	input   textinput.Model
	errText string
	busy    bool
	polling bool

	filtering bool
	filter    string
	showHints bool

	// cover is the panic screen: the whole view is replaced by a canned
	// English agent session so a passer-by sees code talk, never Hangul.
	// Real state stays intact underneath; polling keeps running.
	cover        bool
	stashedInput string

	// spinner disguise: an elapsed clock and a token counter that only ever
	// goes up, like the real thing.
	busyVerb   string
	busyStart  time.Time
	fakeTokens int
	spinFrame  int

	// reopening guards the window-closed recovery so it can't loop; a failed
	// send's text is parked in pendingSend and restored to the composer.
	reopening   bool
	pendingSend string

	// ownedWindow is true when aside created the current room's window, so
	// leaving the room closes it again; windows the user opened themselves
	// are left alone.
	ownedWindow bool
}

func (m model) filteredRooms() []kakao.Conversation {
	if m.filter == "" {
		return m.rooms
	}
	query := strings.ToLower(m.filter)
	var out []kakao.Conversation
	for _, room := range m.rooms {
		if strings.Contains(strings.ToLower(room.Title), query) {
			out = append(out, room)
		}
	}
	return out
}

func Run() error {
	input := textinput.New()
	input.Prompt = "> "
	input.CharLimit = 2000
	input.SetVirtualCursor(false)

	m := model{
		mode:      modeRooms,
		input:     input,
		busy:      true,
		busyVerb:  "Rummaging",
		busyStart: time.Now(),
		vp:        viewport.New(viewport.WithWidth(80), viewport.WithHeight(20)),
	}
	program := tea.NewProgram(m)
	_, err := program.Run()
	return err
}

func fetchRooms() tea.Msg {
	// launches KakaoTalk hidden if the user hasn't started it themselves
	if err := kakao.EnsureAppRunning(); err != nil {
		return errMsg{err}
	}
	rooms, err := kakao.Conversations(20)
	if err != nil {
		return errMsg{err}
	}
	return roomsMsg(rooms)
}

func openRoom(row int, title string) tea.Cmd {
	return func() tea.Msg {
		opened, err := kakao.OpenRoom(row, title)
		if err != nil {
			return errMsg{err}
		}
		return openedMsg{title: title, ownedByUs: opened}
	}
}

func fetchMessages(title string) tea.Cmd {
	return func() tea.Msg {
		items, err := kakao.Messages(title, 20)
		if err != nil {
			return errMsg{err}
		}
		return messagesMsg{title: title, items: items}
	}
}

func sendMessage(title, text string) tea.Cmd {
	return func() tea.Msg {
		if err := kakao.Send(title, text); err != nil {
			return errMsg{err}
		}
		return sentMsg{}
	}
}

// reopenRoom recovers a chat whose KakaoTalk window was closed behind our
// back: resolve the room in the list again, reopen it, reread messages.
func reopenRoom(title string) tea.Cmd {
	return func() tea.Msg {
		resolved, err := kakao.EnsureOpen(title, 30)
		if err != nil {
			return reopenFailedMsg{err}
		}
		items, err := kakao.Messages(resolved, 20)
		if err != nil {
			return reopenFailedMsg{err}
		}
		return messagesMsg{title: resolved, items: items}
	}
}

func tick() tea.Cmd {
	return tea.Tick(pollInterval, func(time.Time) tea.Msg { return tickMsg{} })
}

func spin() tea.Cmd {
	return tea.Tick(spinInterval, func(time.Time) tea.Msg { return spinMsg{} })
}

// startBusy flips the spinner on with a Claude-flavored gerund.
func (m *model) startBusy(verb string) tea.Cmd {
	m.busy = true
	m.busyVerb = verb
	m.busyStart = time.Now()
	m.errText = ""
	return spin()
}

func (m model) Init() tea.Cmd {
	return tea.Batch(fetchRooms, tick(), spin())
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.vp.SetWidth(msg.Width)
		m.vp.SetHeight(max(3, msg.Height-10))
		if m.room != "" {
			m.refreshViewport()
		}
		return m, nil

	case tea.KeyPressMsg:
		return m.handleKey(msg)

	case roomsMsg:
		m.rooms = msg
		m.busy = false
		if m.cursor >= len(m.rooms) {
			m.cursor = max(0, len(m.rooms)-1)
		}
		return m, nil

	case openedMsg:
		m.room = msg.title
		m.mode = modeChat
		m.ownedWindow = msg.ownedByUs
		return m, tea.Batch(m.input.Focus(), fetchMessages(msg.title))

	case messagesMsg:
		if msg.title != m.room {
			return m, nil
		}
		m.busy = false
		m.polling = false
		m.reopening = false
		m.msgs = msg.items
		m.refreshViewport()
		return m, nil

	case sentMsg:
		m.pendingSend = ""
		return m, fetchMessages(m.room)

	case errMsg:
		m.busy = false
		m.polling = false
		if m.mode == modeChat && m.pendingSend != "" {
			m.input.SetValue(m.pendingSend)
			m.input.CursorEnd()
			m.pendingSend = ""
		}
		if m.mode == modeChat && m.room != "" && !m.reopening &&
			strings.Contains(msg.err.Error(), "window not found") {
			m.reopening = true
			m.ownedWindow = true // the reopened window will be ours
			cmd := m.startBusy("Resuming")
			return m, tea.Batch(cmd, reopenRoom(m.room))
		}
		m.errText = msg.err.Error()
		return m, nil

	case reopenFailedMsg:
		m.busy = false
		m.reopening = false
		m.mode = modeRooms
		m.room = ""
		m.msgs = nil
		m.input.Blur()
		m.errText = msg.err.Error()
		return m, func() tea.Msg { return fetchRooms() }

	case spinMsg:
		if !m.busy {
			return m, nil
		}
		m.spinFrame++
		m.fakeTokens += 41 + (m.spinFrame*37)%89
		return m, spin()

	case tickMsg:
		if m.mode == modeChat && m.room != "" && !m.busy && !m.polling {
			m.polling = true
			title := m.room
			return m, tea.Batch(tick(), func() tea.Msg {
				items, err := kakao.Messages(title, 20)
				if err != nil {
					return errMsg{err}
				}
				return messagesMsg{title: title, items: items}
			})
		}
		return m, tick()
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m model) handleKey(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// Panic key: swap the whole screen for a canned English session, and
	// back. While covered every other key is swallowed so nothing can be
	// typed or sent by accident.
	switch key.String() {
	case "f1", "f2", "f3", "ctrl+b":
		m.cover = !m.cover
		if m.cover {
			m.stashedInput = m.input.Value()
			m.input.Reset()
		} else {
			m.input.SetValue(m.stashedInput)
			m.input.CursorEnd()
			m.stashedInput = ""
		}
		m.refreshViewport()
		return m, nil
	}
	if m.cover {
		if key.String() == "ctrl+q" {
			return m, tea.Quit
		}
		return m, nil
	}

	switch key.String() {
	case "ctrl+c", "ctrl+q":
		// tidy up: close the chat window we created before exiting; a quick
		// synchronous call, the process is ending anyway
		if m.mode == modeChat && m.ownedWindow && m.room != "" {
			_ = kakao.CloseRoom(m.room)
		}
		return m, tea.Quit
	}

	if m.mode == modeRooms {
		if m.filtering {
			switch key.String() {
			case "esc":
				m.filtering = false
				m.filter = ""
				m.cursor = 0
				return m, nil
			case "enter":
				return m.openSelected()
			case "backspace":
				if runes := []rune(m.filter); len(runes) > 0 {
					m.filter = string(runes[:len(runes)-1])
				}
				m.cursor = 0
				return m, nil
			case "up":
				if m.cursor > 0 {
					m.cursor--
				}
				return m, nil
			case "down":
				if m.cursor < len(m.filteredRooms())-1 {
					m.cursor++
				}
				return m, nil
			}
			if key.Text != "" {
				m.filter += key.Text
				m.cursor = 0
			}
			return m, nil
		}
		switch key.String() {
		case "esc", "q":
			return m, tea.Quit
		case "?":
			m.showHints = !m.showHints
		case "/":
			m.filtering = true
			m.filter = ""
			m.cursor = 0
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.rooms)-1 {
				m.cursor++
			}
		case "r":
			cmd := m.startBusy("Rummaging")
			return m, tea.Batch(cmd, func() tea.Msg { return fetchRooms() })
		case "enter":
			return m.openSelected()
		}
		return m, nil
	}

	// chat mode
	switch key.String() {
	case "esc", "tab":
		leavingRoom := m.room
		closeIt := m.ownedWindow
		m.mode = modeRooms
		m.room = ""
		m.msgs = nil
		m.ownedWindow = false
		m.input.Reset()
		m.input.Blur()
		cmd := m.startBusy("Rummaging")
		return m, tea.Batch(cmd, func() tea.Msg {
			if closeIt {
				_ = kakao.CloseRoom(leavingRoom)
			}
			return fetchRooms()
		})
	case "?":
		if m.input.Value() == "" {
			m.showHints = !m.showHints
			return m, nil
		}
	case "pgup":
		m.vp.HalfPageUp()
		return m, nil
	case "pgdown":
		m.vp.HalfPageDown()
		return m, nil
	case "ctrl+o":
		if m.busy {
			return m, nil
		}
		title := m.room
		cmd := m.startBusy("Reading")
		return m, tea.Batch(cmd, func() tea.Msg {
			if err := kakao.ScrollOlder(title); err != nil {
				return errMsg{err}
			}
			items, err := kakao.Messages(title, 20)
			if err != nil {
				return errMsg{err}
			}
			return messagesMsg{title: title, items: items}
		})
	case "enter":
		text := strings.TrimSpace(m.input.Value())
		if text == "" || m.busy {
			return m, nil
		}
		m.pendingSend = text
		m.input.Reset()
		cmd := m.startBusy("Dispatching")
		return m, tea.Batch(cmd, sendMessage(m.room, text))
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(key)
	return m, cmd
}

// openSelected opens the room under the cursor in the (possibly filtered)
// list and leaves filter state behind.
func (m model) openSelected() (tea.Model, tea.Cmd) {
	rooms := m.filteredRooms()
	if len(rooms) == 0 || m.busy || m.cursor >= len(rooms) {
		return m, nil
	}
	room := rooms[m.cursor]
	m.filtering = false
	m.filter = ""
	cmd := m.startBusy("Reading")
	return m, tea.Batch(cmd, openRoom(room.Row, room.Title))
}

func (m model) View() tea.View {
	var b strings.Builder
	b.WriteString(m.renderWelcome())
	b.WriteString("\n")

	// Panic screen: canned English session, no live input, no cursor — so a
	// glance shows code talk and a stray keystroke can't type into a chat.
	if m.cover {
		if m.mode == modeChat {
			b.WriteString(m.vp.View())
			b.WriteString("\n\n")
			b.WriteString(boxStyle.Width(max(20, m.width-2)).
				Render("> " + dim.Render("try \"add a metric for the dead-letter path\"")))
		} else {
			b.WriteString(coverRooms(m.height, m.width))
			b.WriteString("\n")
		}
		b.WriteString("\n")
		b.WriteString(m.renderShortcuts())
		view := tea.NewView(b.String())
		view.AltScreen = true
		return view
	}

	var cursor *tea.Cursor
	switch m.mode {
	case modeRooms:
		b.WriteString(m.renderRooms())
		b.WriteString(m.renderSpinnerLine())
		b.WriteString("\n")
		if m.filtering {
			line := "  " + claude.Render("/") + m.filter
			b.WriteString(line + "\n")
			cursor = tea.NewCursor(lipgloss.Width(line), strings.Count(b.String(), "\n")-1)
		}
		b.WriteString(m.renderShortcuts())
	case modeChat:
		b.WriteString(m.vp.View())
		b.WriteString("\n")
		b.WriteString(m.renderSpinnerLine())
		b.WriteString("\n")
		box := boxStyle.Width(max(20, m.width-2)).Render(m.input.View())
		b.WriteString(box)
		b.WriteString("\n")
		b.WriteString(m.renderShortcuts())
		// Cursor on the composer's content line: 2 lines up from the bottom
		// (shortcuts, bottom border), X inside "│ " plus prompt and text.
		if c := m.input.Cursor(); c != nil {
			runes := []rune(m.input.Value())
			pos := min(m.input.Position(), len(runes))
			c.Position.X = 2 + lipgloss.Width(m.input.Prompt) + lipgloss.Width(string(runes[:pos]))
			c.Position.Y = strings.Count(b.String(), "\n") - 2
			cursor = c
		}
	}

	view := tea.NewView(b.String())
	view.AltScreen = true
	view.Cursor = cursor
	return view
}

func (m model) renderWelcome() string {
	home, _ := os.UserHomeDir()
	inner := claude.Render("✻ ") + boldStyle.Render("Welcome to Claude Code!") + "\n" +
		dim.Render("  /help for help · cwd: "+shortPath(home+"/projects"))
	return boxStyle.Width(max(20, m.width-2)).Render(inner)
}

func (m model) renderSpinnerLine() string {
	if m.errText != "" {
		return errStyle.Render("  ⎿  Error: " + clip(m.errText, max(20, m.width-12)))
	}
	if !m.busy {
		return ""
	}
	frame := spinnerFrames[m.spinFrame%len(spinnerFrames)]
	elapsed := int(time.Since(m.busyStart).Seconds())
	return claude.Render(frame+" "+m.busyVerb+"… ") +
		dim.Render(fmt.Sprintf("(%ds · ↓ %s tokens · esc to interrupt)", elapsed, humanTokens(m.fakeTokens)))
}

func (m model) renderShortcuts() string {
	left := dim.Render("  ? for shortcuts")
	if m.cover {
		// nothing about the panic key on screen while it is engaged
		left = dim.Render("  ? for shortcuts")
	} else if m.showHints {
		if m.mode == modeChat {
			left = dim.Render("  enter send · F1 hide · ctrl+o older · esc back · ctrl+q quit")
		} else {
			left = dim.Render("  ↑/↓ move · enter open · / search · F1 hide · r refresh · q quit")
		}
	}
	right := dim.Render("⏵⏵ accept edits on (shift+tab to cycle)")
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right) - 1
	if gap < 1 {
		return left
	}
	return left + strings.Repeat(" ", gap) + right
}

func (m model) renderRooms() string {
	rooms := m.filteredRooms()
	var b strings.Builder
	b.WriteString(boldStyle.Render(" Resume Session") + "\n")
	b.WriteString(dim.Render("     Modified      Summary") + "\n")
	if len(rooms) == 0 {
		if m.filter != "" {
			b.WriteString(dim.Render("   no sessions match “" + m.filter + "”\n"))
		} else {
			b.WriteString(dim.Render("   no sessions yet — press r\n"))
		}
		return b.String()
	}
	visible := max(3, m.height-11)
	start := 0
	if m.cursor >= visible {
		start = m.cursor - visible + 1
	}
	for i := start; i < len(rooms) && i < start+visible; i++ {
		room := rooms[i]
		title := clip(room.Title, 24)
		if room.Unread {
			title = boldStyle.Render(title) + claude.Render(" ●")
		}
		when := room.Time
		if when == "" {
			when = "—"
		}
		line := fmt.Sprintf(" %2d. %-12s %s %s",
			i+1, clip(when, 12), title,
			dim.Render(clip(firstLine(room.Preview), max(10, m.width-46))))
		if i == m.cursor {
			line = selStyle.Render("❯") + line
		} else {
			line = " " + line
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

// refreshViewport repaints the transcript, honouring cover mode.
func (m *model) refreshViewport() {
	if m.cover {
		m.vp.SetContent(coverTranscript(max(30, m.width-8)))
		m.vp.GotoBottom()
		return
	}
	m.vp.SetContent(m.renderMessages())
	m.vp.GotoBottom()
}

// coverTranscript is the panic screen's fake session: an ordinary-looking
// English exchange about code, in the same visual grammar as the real view.
func coverTranscript(width int) string {
	wrap := lipgloss.NewStyle().Width(width)
	line := func(parts ...string) string { return strings.Join(parts, "") }

	var b strings.Builder
	b.WriteString(dim.Render("> ") + wrap.Render("the webhook handler retries forever when the provider 502s. what's the right backoff here?") + "\n\n")
	b.WriteString(toolGreen.Render("● ") + "Search" + dim.Render("(pattern: \"retry|backoff\", path: internal/)") + "\n")
	b.WriteString(dim.Render("  ⎿  ") + wrap.Render("14 matches across 6 files") + "\n\n")
	b.WriteString(toolGreen.Render("● ") + "Read" + dim.Render("(internal/webhook/handler.go)") + "\n")
	b.WriteString(dim.Render("  ⎿  ") + wrap.Render("Read 212 lines") + "\n\n")
	b.WriteString(line(wrap.Render("The loop in handler.go:88 retries on any non-2xx with a fixed 500ms sleep and no ceiling, so a provider outage turns into an unbounded hot loop. Three things to change:"), "\n\n"))
	b.WriteString(wrap.Render("  1. Exponential backoff with full jitter, capped at 30s") + "\n")
	b.WriteString(wrap.Render("  2. A max attempt count (5) after which the event goes to the dead-letter table") + "\n")
	b.WriteString(wrap.Render("  3. Retry only on 429/5xx and network errors — 4xx will never succeed") + "\n\n")
	b.WriteString(toolGreen.Render("● ") + "Update" + dim.Render("(internal/webhook/handler.go)") + "\n")
	b.WriteString(dim.Render("  ⎿  ") + wrap.Render("Updated with 3 additions and 1 removal") + "\n\n")
	b.WriteString(dim.Render("> ") + wrap.Render("does the dead-letter path need its own metric?") + "\n\n")
	b.WriteString(wrap.Render("Yes — otherwise a silent drain looks identical to healthy traffic. I'd add a counter incremented at the point of insert, labelled by provider and last status code, and alert on any non-zero rate over a 5 minute window.") + "\n\n")
	b.WriteString(toolGreen.Render("● ") + "Bash" + dim.Render("(go test ./internal/webhook/...)") + "\n")
	b.WriteString(dim.Render("  ⎿  ") + wrap.Render("ok  	aside/internal/webhook	1.284s") + "\n\n")
	return b.String()
}

// coverRooms is the panic screen for the picker: plausible English sessions
// in place of the room list, which would otherwise show Hangul names.
func coverRooms(height, width int) string {
	rows := []struct{ when, what string }{
		{"2 minutes ago", "webhook retry backoff + dead-letter metric"},
		{"1 hour ago", "flaky TestConsumerRebalance — fix ordering assumption"},
		{"3 hours ago", "drop the redundant index on orders(created_at)"},
		{"Yesterday", "migrate config loader to embed.FS"},
		{"Yesterday", "trace ctx cancellation through the payment path"},
		{"2 days ago", "review: batch writer PR #418"},
		{"3 days ago", "cut allocations in the JSON decode hot path"},
	}
	var b strings.Builder
	b.WriteString(boldStyle.Render(" Resume Session") + "\n")
	b.WriteString(dim.Render("     Modified      Summary") + "\n")
	visible := max(3, height-11)
	for i, r := range rows {
		if i >= visible {
			break
		}
		line := fmt.Sprintf(" %2d. %-12s %s", i+1, clip(r.when, 12), dim.Render(clip(r.what, max(10, width-24))))
		if i == 0 {
			line = selStyle.Render("❯") + line
		} else {
			line = " " + line
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

func (m model) renderMessages() string {
	width := max(30, m.width-8)
	wrap := lipgloss.NewStyle().Width(width)
	var b strings.Builder
	lastSender := ""
	for _, msg := range m.msgs {
		text := msg.Text
		if msg.Edited {
			text += dim.Render(" (edited)")
		}
		if msg.Mine {
			// own messages read as the user's prompts
			b.WriteString(dim.Render("> ") + wrap.Render(text) + "\n\n")
			lastSender = ""
			continue
		}
		// incoming messages read as tool output: ● Task(sender) + ⎿ lines;
		// 1:1 rooms (no sender label) read as plain agent prose.
		sender := msg.Sender
		if sender == "" && m.room != "" {
			sender = m.room
		}
		if sender != lastSender {
			b.WriteString(toolGreen.Render("● ") + "Task" + dim.Render("("+sender+")") + "\n")
			lastSender = sender
		}
		indented := strings.ReplaceAll(wrap.Render(text), "\n", "\n     ")
		b.WriteString(dim.Render("  ⎿  ") + indented + "\n\n")
	}
	if len(m.msgs) == 0 {
		b.WriteString(dim.Render("  (no readable messages)"))
	}
	return b.String()
}

func humanTokens(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	return fmt.Sprintf("%.1fk", float64(n)/1000)
}

func shortPath(p string) string {
	home, err := os.UserHomeDir()
	if err == nil && strings.HasPrefix(p, home) {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
