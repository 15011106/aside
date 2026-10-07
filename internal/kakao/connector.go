// Package kakao drives the running macOS KakaoTalk app through the
// accessibility tree. It never stores messages; KakaoTalk is the source of
// truth and everything here is read (or typed) live.
package kakao

import (
	"fmt"
	"os/exec"
	"strings"
	"time"
)

type DoctorInfo struct {
	Trusted    bool     `json:"trusted"`
	Running    bool     `json:"running"`
	MainWindow bool     `json:"mainWindow"`
	Windows    []string `json:"windows"`
}

type Conversation struct {
	Row     int    `json:"row"`
	Title   string `json:"title"`
	Preview string `json:"preview"`
	Time    string `json:"time"`
	Unread  bool   `json:"unread"`
}

type Message struct {
	// Kind is "text" or "media" — a photo, video or large emoticon, which
	// carries no text but does have a bubble we can capture.
	Kind   string  `json:"kind"`
	Text   string  `json:"text"`
	Sender string  `json:"sender"`
	Mine   bool    `json:"mine"`
	Edited bool    `json:"edited"`
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	W      float64 `json:"w"`
	H      float64 `json:"h"`
}

func (m Message) IsMedia() bool { return m.Kind == "media" }

func Doctor() (DoctorInfo, error) {
	var info DoctorInfo
	err := request("doctor", nil, &info)
	return info, err
}

// EnsureAppRunning launches KakaoTalk hidden (no focus steal, no visible
// window) when it is not running, and waits until it responds. Something has
// to hold the connection to Kakao's servers — this just makes sure the user
// never has to launch it themselves.
func EnsureAppRunning() error {
	info, err := Doctor()
	if err != nil {
		return err
	}
	if !info.Running {
		if err := exec.Command("open", "-g", "-j", "-a", "KakaoTalk").Run(); err != nil {
			return fmt.Errorf("kakao: failed to launch KakaoTalk: %w", err)
		}
		deadline := time.Now().Add(20 * time.Second)
		running := false
		for time.Now().Before(deadline) {
			time.Sleep(500 * time.Millisecond)
			if info, err := Doctor(); err == nil && info.Running {
				running = true
				break
			}
		}
		if !running {
			return fmt.Errorf("kakao: KakaoTalk did not start within 20s")
		}
	}
	// The process being alive is not enough: after a cold hidden launch the
	// UI (and auto-login) needs several seconds before the chat list exists,
	// and an already-running app can still be parked on its login window.
	// Nudge quietly until the main window appears — never activates the app.
	uiDeadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(uiDeadline) {
		if info, err := Doctor(); err == nil {
			for _, title := range info.Windows {
				if title == "Log in" || title == "로그인" {
					return fmt.Errorf("kakao: KakaoTalk is waiting for login — open it from the Dock, log in once, and enable auto-login")
				}
			}
		}
		var nudged struct {
			Ready bool `json:"ready"`
		}
		if err := request("nudgeMain", nil, &nudged); err == nil && nudged.Ready {
			// stealth posture by default: park KakaoTalk hidden (Cmd+H
			// equivalent) so no window is ever visible on screen
			_ = request("hideApp", nil, nil)
			return nil
		}
		time.Sleep(700 * time.Millisecond)
	}
	return fmt.Errorf("kakao: KakaoTalk started but its chat list never appeared — check that it is logged in")
}

func Conversations(limit int) ([]Conversation, error) {
	var conversations []Conversation
	err := request("conversations", map[string]any{"limit": limit}, &conversations)
	return conversations, err
}

// OpenRoom brings the conversation's chat window into existence, quietly
// (AX row selection + Return delivered to the KakaoTalk process). Returns
// whether it created the window — false means it was already open, so the
// caller should leave it alone when cleaning up.
func OpenRoom(row int, title string) (bool, error) {
	var result struct {
		Opened bool `json:"opened"`
	}
	err := request("openRow", map[string]any{"row": row, "title": title}, &result)
	if err != nil && retriableOpenError(err) {
		// The list likely reordered mid-open (a message arrived and moved the
		// room). Rescan for the fresh row number and try once more.
		if room, rerr := Resolve(title, 30); rerr == nil {
			err = request("openRow", map[string]any{"row": room.Row, "title": room.Title}, &result)
		}
	}
	if err != nil {
		return false, err
	}
	return result.Opened, request("prepareComposer", map[string]any{"title": title}, nil)
}

// CloseRoom closes a chat window aside opened (works while hidden). A
// window that is already gone counts as closed.
func CloseRoom(title string) error {
	var result struct {
		Closed bool `json:"closed"`
	}
	if err := request("closeWindow", map[string]any{"title": title}, &result); err != nil {
		return err
	}
	if !result.Closed {
		return fmt.Errorf("kakao: could not close window %q", title)
	}
	return nil
}

func retriableOpenError(err error) bool {
	message := err.Error()
	return strings.Contains(message, "order changed") ||
		strings.Contains(message, "Failed to open KakaoTalk chat window")
}

// Messages reads from an already-open chat window (see OpenRoom).
func Messages(title string, limit int) ([]Message, error) {
	var messages []Message
	err := request("messages", map[string]any{"title": title, "direction": "newer", "limit": limit}, &messages)
	return messages, err
}

func Send(title, text string) error {
	if text == "" {
		return fmt.Errorf("kakao: refusing to send an empty message")
	}
	var result struct {
		Confirmed bool `json:"confirmed"`
	}
	if err := request("send", map[string]any{"title": title, "text": text}, &result); err != nil {
		return err
	}
	if !result.Confirmed {
		return fmt.Errorf("kakao: send was not confirmed")
	}
	return nil
}

// Capture writes the bubble at a message's screen rect to a PNG. It works
// while KakaoTalk is hidden: the window is captured by id rather than from
// the screen, so nothing has to be brought into view.
func Capture(title string, m Message, path string) (string, error) {
	if !m.IsMedia() {
		return "", fmt.Errorf("kakao: that message is not a photo")
	}
	var out struct {
		Path string `json:"path"`
	}
	err := request("capture", map[string]any{
		"title": title, "path": path,
		"x": m.X, "y": m.Y, "w": m.W, "h": m.H,
	}, &out)
	if err != nil {
		return "", err
	}
	return out.Path, nil
}

func ScrollOlder(title string) error {
	return request("scrollOlder", map[string]any{"title": title}, nil)
}

// ProbeComposer reports how the send button resolves for a room, without
// sending anything. Diagnostic for `aside probe`.
func ProbeComposer(title string) (map[string]any, error) {
	var info map[string]any
	err := request("probeComposer", map[string]any{"title": title}, &info)
	return info, err
}

// Resolve finds the conversation whose title matches query: exact match
// first, then case-insensitive substring, most recent first.
func Resolve(query string, limit int) (Conversation, error) {
	rooms, err := Conversations(limit)
	if err != nil {
		return Conversation{}, err
	}
	for _, room := range rooms {
		if room.Title == query {
			return room, nil
		}
	}
	folded := strings.ToLower(query)
	for _, room := range rooms {
		if strings.Contains(strings.ToLower(room.Title), folded) {
			return room, nil
		}
	}
	return Conversation{}, fmt.Errorf("kakao: no conversation matching %q in the latest %d", query, limit)
}

// EnsureOpen returns the exact window title for query, opening the room
// first when its window does not exist yet. Note that opening a room marks
// it as read in KakaoTalk.
func EnsureOpen(query string, limit int) (string, error) {
	if info, err := Doctor(); err == nil {
		for _, title := range info.Windows {
			if title == query {
				return title, nil
			}
		}
	}
	room, err := Resolve(query, limit)
	if err != nil {
		return "", err
	}
	if _, err := OpenRoom(room.Row, room.Title); err != nil {
		return "", err
	}
	return room.Title, nil
}
