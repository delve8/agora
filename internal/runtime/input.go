package runtime

import (
	"io"
	"net"
	"strings"
	"unicode/utf8"

	"github.com/delve8/agora/internal/terminal"
)

// copyTerminalInput forwards terminal bytes unchanged while maintaining a
// small line buffer solely for detecting submitted provider commands. Escape
// sequences and editing keys affect detection only; they are never removed
// from the bytes written to the PTY. Resize frames are applied through
// onResize and never appear as keystrokes.
func copyTerminalInput(master io.Writer, conn net.Conn, sessionID func() string, handler func(string, string), onResize func(int, int)) {
	stream := terminal.NewAttachStream(conn, onResize)
	buf := make([]byte, 4096)
	line := make([]byte, 0, 256)
	escape := false
	csi := false
	for {
		n, err := stream.Read(buf)
		if n > 0 {
			if _, writeErr := master.Write(buf[:n]); writeErr != nil {
				return
			}
			for _, b := range buf[:n] {
				if escape {
					if !csi {
						if b == '[' {
							csi = true
							continue
						}
						escape = false
						continue
					}
					// CSI sequences end in a final byte in the 0x40-0x7e
					// range. Do not treat letters inside parameters as input.
					if b >= 0x40 && b <= 0x7e {
						escape = false
						csi = false
					}
					continue
				}
				switch b {
				case 0x1b:
					escape = true
					csi = false
				case 0x08, 0x7f:
					line = trimLastRune(line)
				case 0x15:
					line = line[:0]
				case '\r', '\n':
					if text := strings.TrimSpace(string(line)); text != "" && handler != nil {
						handler(sessionID(), text)
					}
					line = line[:0]
				default:
					if b >= 0x20 && b != 0x7f && len(line) < 4096 {
						line = append(line, b)
					}
				}
			}
		}
		if err != nil {
			return
		}
	}
}

// trimLastRune removes one complete rune from a keystroke buffer. Removing a
// single byte would split a multi-byte character and leave invalid UTF-8 behind,
// which can then never match the message the provider stored. Providers and
// input methods also emit replacement characters for unrepresentable input; the
// matcher strips those separately.
func trimLastRune(value []byte) []byte {
	if len(value) == 0 {
		return value
	}
	size := 1
	if _, decoded := utf8.DecodeLastRune(value); decoded > 0 {
		size = decoded
	}
	if size > len(value) {
		size = len(value)
	}
	return value[:len(value)-size]
}
