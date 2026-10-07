package ui

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	_ "image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"time"

	"aside/internal/kakao"
)

// Photos are drawn inside the terminal when the terminal can show images.
// There are two protocols in the wild and no common one: iTerm2's inline
// images (iTerm2, WezTerm, xterm.js-based terminals such as Orca's) and
// kitty's graphics protocol (kitty, Ghostty). Anything else falls back to
// opening the picture in the system viewer.
type imageProtocol int

const (
	protoNone imageProtocol = iota
	protoITerm
	protoKitty
)

// detectImageProtocol picks a protocol from the environment the terminal
// advertises. ASIDE_IMAGES=iterm|kitty|off overrides the guess.
func detectImageProtocol() imageProtocol {
	switch strings.ToLower(os.Getenv("ASIDE_IMAGES")) {
	case "iterm":
		return protoITerm
	case "kitty":
		return protoKitty
	case "off", "none":
		return protoNone
	}
	program := strings.ToLower(os.Getenv("TERM_PROGRAM"))
	term := strings.ToLower(os.Getenv("TERM"))
	switch {
	case program == "ghostty" || strings.Contains(term, "ghostty"),
		strings.Contains(term, "kitty") || os.Getenv("KITTY_WINDOW_ID") != "":
		return protoKitty
	case program == "iterm.app" || program == "wezterm" || program == "orca":
		return protoITerm
	}
	return protoNone
}

type photoImage struct {
	png           []byte // always PNG: kitty's direct transfer only takes PNG
	width, height int
}

type hoverSettleMsg struct{ seq, photo int }
type drawPhotoMsg struct{ seq int }
type photoLoadedMsg struct {
	key   string
	image photoImage
	err   error
}

// loadPhoto captures a bubble to PNG and reads back its pixel size. The
// capture reads KakaoTalk's window by id, so this works while the app is
// hidden — looking at a picture never brings it on screen.
func loadPhoto(room string, msg kakao.Message) (photoImage, error) {
	path := filepath.Join(os.TempDir(), fmt.Sprintf("aside-photo-%d.png", time.Now().UnixNano()))
	saved, err := kakao.Capture(room, msg, path)
	if err != nil {
		return photoImage{}, err
	}
	defer os.Remove(saved)

	raw, err := os.ReadFile(saved)
	if err != nil {
		return photoImage{}, err
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return photoImage{}, err
	}
	return photoImage{png: raw, width: config.Width, height: config.Height}, nil
}

// figureRows is how many terminal rows an image of cols columns needs,
// assuming a cell roughly twice as tall as it is wide.
func photoRows(img photoImage, cols int) int {
	if img.width == 0 {
		return 6
	}
	rows := int(float64(cols)*float64(img.height)/float64(img.width)/2.1 + 0.5)
	return max(3, rows)
}

// drawSequence places the image with its top-left at (row, col), both
// 1-based screen coordinates, without disturbing the renderer's cursor.
func drawSequence(proto imageProtocol, img photoImage, row, col, cols, rows int) string {
	var b strings.Builder
	b.WriteString("\x1b7") // save cursor
	fmt.Fprintf(&b, "\x1b[%d;%dH", row, col)
	encoded := base64.StdEncoding.EncodeToString(img.png)
	switch proto {
	case protoITerm:
		fmt.Fprintf(&b, "\x1b]1337;File=inline=1;size=%d;width=%d;height=%d;preserveAspectRatio=1:%s\a",
			len(img.png), cols, rows, encoded)
	case protoKitty:
		// transmit-and-display in 4 KiB chunks; q=2 keeps the terminal from
		// replying (a reply would arrive as keyboard input), C=1 leaves the
		// cursor where it was
		const chunk = 4096
		for i := 0; i < len(encoded); i += chunk {
			end := min(i+chunk, len(encoded))
			more := 0
			if end < len(encoded) {
				more = 1
			}
			if i == 0 {
				fmt.Fprintf(&b, "\x1b_Ga=T,f=100,c=%d,r=%d,q=2,C=1,m=%d;%s\x1b\\", cols, rows, more, encoded[i:end])
			} else {
				fmt.Fprintf(&b, "\x1b_Gm=%d;%s\x1b\\", more, encoded[i:end])
			}
		}
	}
	b.WriteString("\x1b8") // restore cursor
	return b.String()
}

// clearSequence removes a drawn image. kitty keeps images on a separate
// layer and needs an explicit delete; iTerm2 images live in the cells and
// go away when the screen is redrawn.
func clearSequence(proto imageProtocol) string {
	if proto == protoKitty {
		return "\x1b_Ga=d,d=A,q=2\x1b\\"
	}
	return ""
}
