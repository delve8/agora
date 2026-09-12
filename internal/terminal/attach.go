package terminal

import (
	"encoding/binary"
	"errors"
	"io"
)

const (
	// DefaultCols and DefaultRows are the PTY size used before a wrapper
	// reports the real terminal. Pi exits on a 0x0 window; 80x24 is also too
	// narrow for a typical wrapper terminal, so start from a usable viewport.
	DefaultCols = 120
	DefaultRows = 40

	FrameData   byte = 0
	FrameResize byte = 1

	maxAttachFrame = 1 << 20
)

var errAttachFrameTooLarge = errors.New("attach frame too large")

// ClampSize rejects empty dimensions and caps values at the PTY uint16 range.
func ClampSize(cols, rows int) (int, int, bool) {
	if cols < 1 || rows < 1 {
		return 0, 0, false
	}
	if cols > 65535 {
		cols = 65535
	}
	if rows > 65535 {
		rows = 65535
	}
	return cols, rows, true
}

func WriteData(w io.Writer, data []byte) error {
	return writeFrame(w, FrameData, data)
}

func WriteResize(w io.Writer, cols, rows int) error {
	cols, rows, ok := ClampSize(cols, rows)
	if !ok {
		return errors.New("invalid terminal size")
	}
	payload := make([]byte, 4)
	binary.BigEndian.PutUint16(payload[0:], uint16(cols))
	binary.BigEndian.PutUint16(payload[2:], uint16(rows))
	return writeFrame(w, FrameResize, payload)
}

func writeFrame(w io.Writer, typ byte, payload []byte) error {
	if len(payload) > maxAttachFrame {
		return errAttachFrameTooLarge
	}
	header := [5]byte{typ}
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	_, err := w.Write(payload)
	return err
}

func decodeResize(payload []byte) (int, int, bool) {
	if len(payload) != 4 {
		return 0, 0, false
	}
	cols := int(binary.BigEndian.Uint16(payload[0:]))
	rows := int(binary.BigEndian.Uint16(payload[2:]))
	return ClampSize(cols, rows)
}

// AttachStream reads framed wrapper input. Data frames become PTY keystrokes;
// resize frames are applied through onResize and never reach the PTY.
type AttachStream struct {
	r        io.Reader
	onResize func(cols, rows int)
	raw      []byte
	pending  []byte
}

func NewAttachStream(r io.Reader, onResize func(cols, rows int)) *AttachStream {
	return &AttachStream{r: r, onResize: onResize}
}

func (s *AttachStream) Read(p []byte) (int, error) {
	if s == nil {
		return 0, io.EOF
	}
	for {
		if len(s.pending) > 0 {
			n := copy(p, s.pending)
			s.pending = s.pending[n:]
			return n, nil
		}
		typ, payload, err := s.readFrame()
		if err != nil {
			return 0, err
		}
		switch typ {
		case FrameData:
			if len(payload) == 0 {
				continue
			}
			n := copy(p, payload)
			if n < len(payload) {
				s.pending = append(s.pending, payload[n:]...)
			}
			return n, nil
		case FrameResize:
			if cols, rows, ok := decodeResize(payload); ok && s.onResize != nil {
				s.onResize(cols, rows)
			}
		}
	}
}

func (s *AttachStream) fill() (int, error) {
	tmp := make([]byte, 4096)
	n, err := s.r.Read(tmp)
	if n > 0 {
		s.raw = append(s.raw, tmp[:n]...)
	}
	return n, err
}

func (s *AttachStream) readFull(n int) ([]byte, error) {
	for len(s.raw) < n {
		got, err := s.fill()
		if got == 0 && err != nil {
			return nil, err
		}
	}
	out := append([]byte(nil), s.raw[:n]...)
	s.raw = s.raw[n:]
	return out, nil
}

func (s *AttachStream) readFrame() (byte, []byte, error) {
	header, err := s.readFull(5)
	if err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(header[1:])
	if n > maxAttachFrame {
		return 0, nil, errAttachFrameTooLarge
	}
	payload, err := s.readFull(int(n))
	if err != nil {
		return 0, nil, err
	}
	return header[0], payload, nil
}
