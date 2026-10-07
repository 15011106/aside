package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"aside/internal/kakao"
	"aside/internal/ui"
)

// bridgeID is stamped by the Makefile with the Swift archive's checksum.
// Go's build cache does not hash externally linked libraries, so without
// this a bridge-only change silently reuses the previously linked binary.
var bridgeID string

// version is stamped by the Makefile at build time.
var version = "dev"

const usage = `aside — a terminal that happens to show your DMs

usage:
  aside          start the TUI
  aside doctor   check accessibility permission and KakaoTalk state
  aside ls [n]   list the latest n conversations (default 10)
  aside read <room> [n]    read last n messages (partial name ok;
                           opens the room if needed, which marks it read)
  aside open <room>        open a conversation window by (partial) name
  aside send <room> <text...>  send a message (careful: real send)
  aside photo <room> [n]   capture and open a photo from the last messages
`

// how many recent conversations name resolution scans
const resolveLimit = 30

func main() {
	if len(os.Args) < 2 {
		if err := ui.Run(); err != nil {
			fail(err)
		}
		return
	}

	switch os.Args[1] {
	case "ls", "read", "open", "send", "probe":
		if err := kakao.EnsureAppRunning(); err != nil {
			fail(err)
		}
	}

	switch os.Args[1] {
	case "doctor":
		info, err := kakao.Doctor()
		if err != nil {
			fail(err)
		}
		fmt.Printf("accessibility trusted : %v\n", info.Trusted)
		fmt.Printf("kakaotalk running     : %v\n", info.Running)
		fmt.Printf("main window found     : %v\n", info.MainWindow)
		fmt.Printf("open windows          : %s\n", strings.Join(info.Windows, ", "))
		if !info.Trusted {
			fmt.Println("\ngrant accessibility to your terminal app:")
			fmt.Println("  System Settings → Privacy & Security → Accessibility")
		}
	case "ls":
		limit := 10
		if len(os.Args) > 2 {
			if n, err := strconv.Atoi(os.Args[2]); err == nil {
				limit = n
			}
		}
		conversations, err := kakao.Conversations(limit)
		if err != nil {
			fail(err)
		}
		for _, c := range conversations {
			marker := " "
			if c.Unread {
				marker = "*"
			}
			fmt.Printf("%s %3d  %-24s %s\n", marker, c.Row, c.Title, firstLine(c.Preview))
		}
	case "read":
		if len(os.Args) < 3 {
			fail(fmt.Errorf("usage: aside read <room> [n]"))
		}
		limit := 15
		if len(os.Args) > 3 {
			if n, err := strconv.Atoi(os.Args[3]); err == nil {
				limit = n
			}
		}
		title, err := kakao.EnsureOpen(os.Args[2], resolveLimit)
		if err != nil {
			fail(err)
		}
		messages, err := kakao.Messages(title, limit)
		if err != nil {
			fail(err)
		}
		for _, m := range messages {
			if m.IsMedia() {
				label := m.Text
				if label == "" {
					label = "photo"
				}
				who := title
				if m.Mine {
					who = "me"
				} else if m.Sender != "" {
					who = m.Sender
				}
				fmt.Printf("%-12s [%s]\n", who, label)
				continue
			}
			sender := m.Sender
			if m.Mine {
				sender = "me"
			} else if sender == "" {
				// 1:1 rooms carry no sender labels; the room name is the peer.
				sender = title
			}
			fmt.Printf("%-12s %s\n", sender, m.Text)
		}
	case "open":
		if len(os.Args) < 3 {
			fail(fmt.Errorf("usage: aside open <room>"))
		}
		title, err := kakao.EnsureOpen(os.Args[2], resolveLimit)
		if err != nil {
			fail(err)
		}
		fmt.Println("opened:", title)
	case "send":
		if len(os.Args) < 4 {
			fail(fmt.Errorf("usage: aside send <room> <text...>"))
		}
		title, err := kakao.EnsureOpen(os.Args[2], resolveLimit)
		if err != nil {
			fail(err)
		}
		if err := kakao.Send(title, strings.Join(os.Args[3:], " ")); err != nil {
			fail(err)
		}
		fmt.Println("sent to:", title)
	case "probe":
		if len(os.Args) < 3 {
			fail(fmt.Errorf("usage: aside probe <room>"))
		}
		title, err := kakao.EnsureOpen(os.Args[2], resolveLimit)
		if err != nil {
			fail(err)
		}
		info, err := kakao.ProbeComposer(title)
		if err != nil {
			fail(err)
		}
		fmt.Printf("room          : %s\n", title)
		fmt.Printf("field found   : %v\n", info["fieldFound"])
		fmt.Printf("button found  : %v\n", info["buttonFound"])
		if info["buttonFound"] == true {
			fmt.Printf("chosen button : %q  x=%.0f w=%.0f opensMenu=%v\n",
				info["title"], info["x"], info["width"], info["opensMenu"])
		}
		if layout, ok := info["layout"].(map[string]any); ok {
			fmt.Printf("layout        : scrollAreas=%v field=(%.0f,%.0f %.0fx%.0f) messageTable=%v rows=%v\n",
				layout["scrollAreas"], layout["fieldX"], layout["fieldY"],
				layout["fieldW"], layout["fieldH"], layout["messageTable"], layout["messageRows"])
		}
		// every candidate (or every row button when none qualified) so a
		// remote user's layout can be diagnosed from this output alone
		for _, key := range []string{"candidates", "rowButtons"} {
			list, ok := info[key].([]any)
			if !ok || len(list) == 0 {
				continue
			}
			fmt.Printf("%s:\n", key)
			for _, item := range list {
				b, ok := item.(map[string]any)
				if !ok {
					continue
				}
				fmt.Printf("  - %q x=%.0f w=%.0f opensMenu=%v", b["title"], b["x"], b["width"], b["opensMenu"])
				if p, ok := b["pressable"]; ok {
					fmt.Printf(" pressable=%v", p)
				}
				fmt.Println()
			}
		}
	case "photo":
		if len(os.Args) < 3 {
			fail(fmt.Errorf("usage: aside photo <room> [n]"))
		}
		title, err := kakao.EnsureOpen(os.Args[2], resolveLimit)
		if err != nil {
			fail(err)
		}
		msgs, err := kakao.Messages(title, 20)
		if err != nil {
			fail(err)
		}
		var media []kakao.Message
		for _, mm := range msgs {
			if mm.IsMedia() {
				media = append(media, mm)
			}
		}
		if len(media) == 0 {
			fail(fmt.Errorf("no photos in the last 20 messages of %s", title))
		}
		index := len(media)
		if len(os.Args) > 3 {
			if n, err := strconv.Atoi(os.Args[3]); err == nil && n >= 1 && n <= len(media) {
				index = n
			}
		}
		out := filepath.Join(os.TempDir(), fmt.Sprintf("aside-photo-%d.png", time.Now().UnixNano()))
		saved, err := kakao.Capture(title, media[index-1], out)
		if err != nil {
			fail(err)
		}
		fmt.Println(saved)
		_ = exec.Command("open", saved).Start()

	case "version", "-v", "--version":
		fmt.Println("aside", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Print(usage)
		os.Exit(2)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "aside:", err)
	os.Exit(1)
}
