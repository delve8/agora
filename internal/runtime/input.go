package runtime

import (
	"io"
	"net"
	"strings"
)

// copyTerminalInput forwards terminal bytes unchanged while maintaining a
// small line buffer solely for detecting submitted provider commands. Escape
// sequences and editing keys affect detection only; they are never removed
// from the bytes written to the PTY.
func copyTerminalInput(master io.Writer, conn net.Conn, sessionID func() string, handler func(string, string)) {
	buf := make([]byte, 4096)
	line := make([]byte, 0, 256)
	escape := false
	csi := false
	for {
		n, err := conn.Read(buf)
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
					if len(line) > 0 {
						line = line[:len(line)-1]
					}
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
