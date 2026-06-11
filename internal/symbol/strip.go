package symbol

// stripCLike blanks comments and string/char/template literals in C-family
// source (Java, TS/JS, Go fallback), preserving length and newlines so byte
// offsets and line numbers stay valid.
func stripCLike(src []byte) []byte {
	out := make([]byte, len(src))
	copy(out, src)
	const (
		code = iota
		lineComment
		blockComment
		dquote
		squote
		backtick
	)
	state := code
	for i := 0; i < len(out); i++ {
		c := out[i]
		switch state {
		case code:
			switch {
			case c == '/' && i+1 < len(out) && out[i+1] == '/':
				state = lineComment
				out[i] = ' '
			case c == '/' && i+1 < len(out) && out[i+1] == '*':
				state = blockComment
				out[i] = ' '
			case c == '"':
				state = dquote
			case c == '\'':
				state = squote
			case c == '`':
				state = backtick
			}
		case lineComment:
			if c == '\n' {
				state = code
			} else {
				out[i] = ' '
			}
		case blockComment:
			if c == '*' && i+1 < len(out) && out[i+1] == '/' {
				out[i], out[i+1] = ' ', ' '
				i++
				state = code
			} else if c != '\n' {
				out[i] = ' '
			}
		case dquote, squote:
			switch {
			case c == '\\' && i+1 < len(out):
				out[i] = ' '
				if out[i+1] != '\n' {
					out[i+1] = ' '
				}
				i++
			case state == dquote && c == '"' || state == squote && c == '\'':
				state = code
			case c == '\n':
				state = code // unterminated literal: resync at newline
			default:
				out[i] = ' '
			}
		case backtick:
			if c == '`' {
				state = code
			} else if c != '\n' {
				out[i] = ' '
			}
		}
	}
	return out
}

// stripPython blanks #-comments and string literals (incl. triple-quoted)
// in Python source, preserving length and newlines.
func stripPython(src []byte) []byte {
	out := make([]byte, len(src))
	copy(out, src)
	i := 0
	for i < len(out) {
		c := out[i]
		switch {
		case c == '#':
			for i < len(out) && out[i] != '\n' {
				out[i] = ' '
				i++
			}
		case c == '"' || c == '\'':
			q := c
			triple := i+2 < len(out) && out[i+1] == q && out[i+2] == q
			if triple {
				i += 3
				for i < len(out) {
					if out[i] == q && i+2 < len(out) && out[i+1] == q && out[i+2] == q {
						i += 3
						break
					}
					if out[i] != '\n' {
						out[i] = ' '
					}
					i++
				}
			} else {
				i++
				for i < len(out) && out[i] != '\n' {
					if out[i] == '\\' {
						out[i] = ' '
						if i+1 < len(out) && out[i+1] != '\n' {
							out[i+1] = ' '
						}
						i += 2
						continue
					}
					if out[i] == q {
						i++
						break
					}
					out[i] = ' '
					i++
				}
			}
			continue
		default:
			i++
		}
	}
	return out
}

// Strip blanks comments and strings for the given language key.
// Unknown languages return src unchanged.
func Strip(lang string, src []byte) []byte {
	switch lang {
	case "python":
		return stripPython(src)
	case "go", "java", "typescript":
		return stripCLike(src)
	}
	return src
}
