package daemon

import (
	"unicode/utf16"
	"unicode/utf8"
)

// decodeUTF16 transcodes UTF-16 content (detected by BOM) to UTF-8 so
// such files index and search like any text file instead of being
// classified binary by the NUL probe. ok=false means b is not UTF-16.
func decodeUTF16(b []byte) (out []byte, ok bool) {
	var be bool
	switch {
	case len(b) >= 2 && b[0] == 0xFF && b[1] == 0xFE:
		be = false
	case len(b) >= 2 && b[0] == 0xFE && b[1] == 0xFF:
		be = true
	default:
		return nil, false
	}
	b = b[2:]
	u16 := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 { // a trailing odd byte is dropped
		if be {
			u16 = append(u16, uint16(b[i])<<8|uint16(b[i+1]))
		} else {
			u16 = append(u16, uint16(b[i+1])<<8|uint16(b[i]))
		}
	}
	runes := utf16.Decode(u16)
	out = make([]byte, 0, len(runes)*2)
	var buf [utf8.UTFMax]byte
	for _, r := range runes {
		n := utf8.EncodeRune(buf[:], r)
		out = append(out, buf[:n]...)
	}
	return out, true
}
