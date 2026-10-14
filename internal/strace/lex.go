package strace

import (
	"fmt"
	"strconv"
	"strings"
)

// call is one completed system call, rebuilt from strace's output (an
// interrupted call's "<unfinished ...>" and "<... resumed>" halves are
// joined back together).
type call struct {
	pid  int
	name string
	args []string
	ret  string
	line int
}

// retInt is the numeric return value, or -1 when the call failed or never
// returned.
func (c *call) retInt() int64 {
	f := strings.Fields(c.ret)
	if len(f) == 0 {
		return -1
	}
	s := f[0]
	if i := strings.IndexByte(s, '<'); i >= 0 {
		s = s[:i]
	}
	n, err := strconv.ParseInt(s, 0, 64)
	if err != nil {
		return -1
	}
	return n
}

// retPath is the path strace printed for a returned file descriptor.
func (c *call) retPath() string {
	_, p := fdArg(strings.Fields(c.ret + " ")[0])
	return p
}

func (c *call) arg(i int) string {
	if i < len(c.args) {
		return c.args[i]
	}
	return ""
}

// lineParser joins interrupted calls back together.
type lineParser struct {
	pending map[int]string
}

func (lp *lineParser) parse(s string, lineNo int) (*call, error) {
	s = strings.TrimRight(s, "\n")
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	pid, err := strconv.Atoi(s[:i])
	if err != nil {
		return nil, fmt.Errorf("line %d: no pid: %.60q", lineNo, s)
	}
	body := strings.TrimLeft(s[i:], " ")
	switch {
	case strings.HasPrefix(body, "+++"), strings.HasPrefix(body, "---"):
		return nil, nil
	case strings.HasPrefix(body, "<... "):
		j := strings.Index(body, " resumed>")
		if j < 0 {
			return nil, fmt.Errorf("line %d: bad resume: %.60q", lineNo, s)
		}
		head, ok := lp.pending[pid]
		if !ok {
			return nil, fmt.Errorf("line %d: resume without a start", lineNo)
		}
		delete(lp.pending, pid)
		body = head + body[j+len(" resumed>"):]
	case strings.HasSuffix(body, " <unfinished ...>"):
		lp.pending[pid] = strings.TrimSuffix(body, " <unfinished ...>")
		return nil, nil
	}
	open := strings.IndexByte(body, '(')
	if open < 0 {
		return nil, fmt.Errorf("line %d: not a call: %.60q", lineNo, s)
	}
	c := &call{pid: pid, name: body[:open], line: lineNo}
	args, rest, err := splitArgs(body[open+1:])
	if err != nil {
		return nil, fmt.Errorf("line %d: %v", lineNo, err)
	}
	c.args = args
	rest = strings.TrimLeft(rest, " ")
	if !strings.HasPrefix(rest, "= ") {
		// "= ?" is printed for calls that never return (exit, execve's
		// old image); anything else is unexpected.
		return nil, fmt.Errorf("line %d: no return value: %.60q", lineNo, s)
	}
	c.ret = rest[2:]
	return c, nil
}

// splitArgs splits "a, b, {c, d}, [e]) = 0" into its top-level arguments
// and returns what follows the closing parenthesis.
func splitArgs(s string) ([]string, string, error) {
	var args []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			i++
			for i < len(s) && s[i] != '"' {
				if s[i] == '\\' {
					i++
				}
				i++
			}
		case c == '(' || c == '[' || c == '{':
			depth++
		case c == '<' && i > 0 && isWord(s[i-1]):
			// fd decoration: 3</path>, AT_FDCWD</path>. With -xx its
			// contents are all \x escapes, so the next '>' closes it.
			j := strings.IndexByte(s[i:], '>')
			if j < 0 {
				return nil, "", fmt.Errorf("unclosed decoration")
			}
			i += j
		case c == ']' || c == '}':
			depth--
		case c == ')':
			if depth == 0 {
				if a := strings.TrimSpace(s[start:i]); a != "" || len(args) > 0 {
					args = append(args, a)
				}
				return args, s[i+1:], nil
			}
			depth--
		case c == ',' && depth == 0:
			args = append(args, strings.TrimSpace(s[start:i]))
			start = i + 1
		}
	}
	return nil, "", fmt.Errorf("unterminated argument list")
}

func isWord(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '_'
}

// fdArg decodes "3</tmp/x>", "AT_FDCWD</tmp>" or "3". AT_FDCWD is -100.
func fdArg(s string) (int, string) {
	path := ""
	if i := strings.IndexByte(s, '<'); i >= 0 && strings.HasSuffix(s, ">") {
		b, _, err := unquote(`"` + s[i+1:len(s)-1] + `"`)
		if err == nil {
			path = string(b)
		}
		s = s[:i]
	}
	if s == "AT_FDCWD" {
		return -100, path
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return -1, path
	}
	return n, path
}

// unquote decodes a C-style string as strace prints it. truncated is true
// when strace cut it short (it was longer than -s).
func unquote(s string) (b []byte, truncated bool, err error) {
	if strings.HasSuffix(s, "...") {
		truncated = true
		s = strings.TrimSuffix(s, "...")
	}
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return nil, false, fmt.Errorf("not a string: %.40q", s)
	}
	s = s[1 : len(s)-1]
	out := make([]byte, 0, len(s)/4)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' {
			out = append(out, c)
			continue
		}
		i++
		if i >= len(s) {
			return nil, false, fmt.Errorf("trailing backslash")
		}
		switch s[i] {
		case 'x':
			if i+3 > len(s) {
				return nil, false, fmt.Errorf("short \\x escape")
			}
			v, err := strconv.ParseUint(s[i+1:i+3], 16, 8)
			if err != nil {
				return nil, false, err
			}
			out = append(out, byte(v))
			i += 2
		case 'n':
			out = append(out, '\n')
		case 't':
			out = append(out, '\t')
		case 'r':
			out = append(out, '\r')
		case 'v':
			out = append(out, '\v')
		case 'f':
			out = append(out, '\f')
		case '0', '1', '2', '3', '4', '5', '6', '7':
			j := i
			for j < len(s) && j < i+3 && s[j] >= '0' && s[j] <= '7' {
				j++
			}
			v, _ := strconv.ParseUint(s[i:j], 8, 8)
			out = append(out, byte(v))
			i = j - 1
		default:
			out = append(out, s[i])
		}
	}
	return out, truncated, nil
}

func flags(s string) map[string]bool {
	m := map[string]bool{}
	for _, f := range strings.Split(s, "|") {
		m[strings.TrimSpace(f)] = true
	}
	return m
}
