package sessionhost

import "strings"

// inputObserver keeps only enough terminal editing state to identify submitted
// lines. It never changes the bytes sent to the Agent.
type inputObserver struct {
	line   []byte
	escape bool
	csi    bool
	onLine func(string)
}

func (o *inputObserver) process(data []byte) {
	for _, b := range data {
		if o.escape {
			if !o.csi {
				if b == '[' {
					o.csi = true
					continue
				}
				o.escape = false
				continue
			}
			if b >= 0x40 && b <= 0x7e {
				o.escape, o.csi = false, false
			}
			continue
		}
		switch b {
		case 0x1b:
			o.escape, o.csi = true, false
		case 0x08, 0x7f:
			if len(o.line) > 0 {
				o.line = o.line[:len(o.line)-1]
			}
		case 0x15:
			o.line = o.line[:0]
		case '\r', '\n':
			if text := strings.TrimSpace(string(o.line)); text != "" && o.onLine != nil {
				o.onLine(text)
			}
			o.line = o.line[:0]
		default:
			if b >= 0x20 && b != 0x7f && len(o.line) < 4096 {
				o.line = append(o.line, b)
			}
		}
	}
}
