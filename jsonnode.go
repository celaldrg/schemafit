package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Kind is the type of a JSON value.
type Kind int

const (
	KNull Kind = iota
	KBool
	KNum
	KStr
	KArr
	KObj
)

// Member is one key/value pair of an object, in source order.
type Member struct {
	Key    string
	KeyOff int
	Val    *Node
}

// Node is a JSON value that remembers key order and source offsets,
// so findings can point at a line and column and fixes keep the layout.
type Node struct {
	Kind    Kind
	Str     string // string value, or the raw text of a number
	Bool    bool
	Items   []*Node
	Members []*Member
	Off     int
}

func (n *Node) IsObj() bool { return n != nil && n.Kind == KObj }

// Get returns the value for key k (the last one if the key is repeated).
func (n *Node) Get(k string) *Node {
	if m := n.Member(k); m != nil {
		return m.Val
	}
	return nil
}

func (n *Node) Member(k string) *Member {
	if n == nil || n.Kind != KObj {
		return nil
	}
	for i := len(n.Members) - 1; i >= 0; i-- {
		if n.Members[i].Key == k {
			return n.Members[i]
		}
	}
	return nil
}

// Set replaces the value of k, or appends k at the end.
func (n *Node) Set(k string, v *Node) {
	if m := n.Member(k); m != nil {
		m.Val = v
		return
	}
	n.Members = append(n.Members, &Member{Key: k, KeyOff: -1, Val: v})
}

func (n *Node) Delete(k string) {
	out := n.Members[:0]
	for _, m := range n.Members {
		if m.Key != k {
			out = append(out, m)
		}
	}
	n.Members = out
}

func newStr(s string) *Node    { return &Node{Kind: KStr, Str: s, Off: -1} }
func newBool(b bool) *Node     { return &Node{Kind: KBool, Bool: b, Off: -1} }
func newObj() *Node            { return &Node{Kind: KObj, Off: -1} }
func newArr(it ...*Node) *Node { return &Node{Kind: KArr, Items: it, Off: -1} }
func newNull() *Node           { return &Node{Kind: KNull, Off: -1} }

// SyntaxError is a JSON parse error at a byte offset.
type SyntaxError struct {
	Off int
	Msg string
}

func (e *SyntaxError) Error() string { return e.Msg }

// DupKey records a key that appears twice in one object.
type DupKey struct {
	Key string
	Off int
}

type parser struct {
	s    []byte
	i    int
	dups []DupKey
}

// Parse decodes JSON into a Node tree. A UTF-8 BOM is skipped.
func Parse(data []byte) (*Node, []DupKey, error) {
	p := &parser{s: data}
	if bytes.HasPrefix(data, []byte{0xEF, 0xBB, 0xBF}) {
		p.i = 3
	}
	p.ws()
	n, err := p.value(0)
	if err != nil {
		return nil, nil, err
	}
	p.ws()
	if p.i < len(p.s) {
		return nil, nil, &SyntaxError{p.i, "unexpected data after the JSON value"}
	}
	return n, p.dups, nil
}

func (p *parser) ws() {
	for p.i < len(p.s) {
		switch p.s[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
		default:
			return
		}
	}
}

func (p *parser) errf(format string, a ...any) error {
	return &SyntaxError{p.i, fmt.Sprintf(format, a...)}
}

func (p *parser) value(depth int) (*Node, error) {
	if depth > 512 {
		return nil, p.errf("nesting is too deep")
	}
	if p.i >= len(p.s) {
		return nil, p.errf("unexpected end of input")
	}
	start := p.i
	switch c := p.s[p.i]; {
	case c == '{':
		p.i++
		n := &Node{Kind: KObj, Off: start}
		seen := map[string]bool{}
		p.ws()
		if p.i < len(p.s) && p.s[p.i] == '}' {
			p.i++
			return n, nil
		}
		for {
			p.ws()
			if p.i >= len(p.s) || p.s[p.i] != '"' {
				return nil, p.errf("expected a string key")
			}
			koff := p.i
			k, err := p.str()
			if err != nil {
				return nil, err
			}
			if seen[k] {
				p.dups = append(p.dups, DupKey{k, koff})
			}
			seen[k] = true
			p.ws()
			if p.i >= len(p.s) || p.s[p.i] != ':' {
				return nil, p.errf("expected ':' after key %q", k)
			}
			p.i++
			p.ws()
			v, err := p.value(depth + 1)
			if err != nil {
				return nil, err
			}
			n.Members = append(n.Members, &Member{Key: k, KeyOff: koff, Val: v})
			p.ws()
			if p.i < len(p.s) && p.s[p.i] == ',' {
				p.i++
				continue
			}
			if p.i < len(p.s) && p.s[p.i] == '}' {
				p.i++
				return n, nil
			}
			return nil, p.errf("expected ',' or '}' in object")
		}
	case c == '[':
		p.i++
		n := &Node{Kind: KArr, Off: start}
		p.ws()
		if p.i < len(p.s) && p.s[p.i] == ']' {
			p.i++
			return n, nil
		}
		for {
			p.ws()
			v, err := p.value(depth + 1)
			if err != nil {
				return nil, err
			}
			n.Items = append(n.Items, v)
			p.ws()
			if p.i < len(p.s) && p.s[p.i] == ',' {
				p.i++
				continue
			}
			if p.i < len(p.s) && p.s[p.i] == ']' {
				p.i++
				return n, nil
			}
			return nil, p.errf("expected ',' or ']' in array")
		}
	case c == '"':
		s, err := p.str()
		if err != nil {
			return nil, err
		}
		return &Node{Kind: KStr, Str: s, Off: start}, nil
	case c == 't' || c == 'f' || c == 'n':
		for _, lit := range []string{"true", "false", "null"} {
			if bytes.HasPrefix(p.s[p.i:], []byte(lit)) {
				p.i += len(lit)
				switch lit {
				case "null":
					return &Node{Kind: KNull, Off: start}, nil
				default:
					return &Node{Kind: KBool, Bool: lit == "true", Off: start}, nil
				}
			}
		}
		return nil, p.errf("invalid literal")
	case c == '-' || (c >= '0' && c <= '9'):
		for p.i < len(p.s) && strings.IndexByte("+-0123456789.eE", p.s[p.i]) >= 0 {
			p.i++
		}
		raw := string(p.s[start:p.i])
		var num json.Number
		if err := json.Unmarshal([]byte(raw), &num); err != nil {
			p.i = start
			return nil, p.errf("invalid number %q", raw)
		}
		return &Node{Kind: KNum, Str: raw, Off: start}, nil
	default:
		return nil, p.errf("unexpected character %q", string(c))
	}
}

func (p *parser) str() (string, error) {
	start := p.i
	p.i++
	for p.i < len(p.s) {
		switch p.s[p.i] {
		case '\\':
			p.i += 2
			continue
		case '"':
			p.i++
			var s string
			if err := json.Unmarshal(p.s[start:p.i], &s); err != nil {
				p.i = start
				return "", p.errf("invalid string")
			}
			return s, nil
		}
		p.i++
	}
	p.i = start
	return "", p.errf("unterminated string")
}

// Locator turns byte offsets into 1-based line and column numbers (columns count runes).
type Locator struct {
	src    []byte
	starts []int
}

func NewLocator(src []byte) *Locator {
	l := &Locator{src: src, starts: []int{0}}
	for i, b := range src {
		if b == '\n' {
			l.starts = append(l.starts, i+1)
		}
	}
	return l
}

func (l *Locator) Pos(off int) (int, int) {
	if off < 0 {
		return 1, 1
	}
	i := sort.Search(len(l.starts), func(i int) bool { return l.starts[i] > off }) - 1
	col := len([]rune(string(l.src[l.starts[i]:off]))) + 1
	return i + 1, col
}

// Encode writes n as indented JSON, keeping key order.
func Encode(n *Node) string {
	var b strings.Builder
	encode(&b, n, "")
	b.WriteByte('\n')
	return b.String()
}

func quote(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimRight(buf.String(), "\n")
}

func encode(b *strings.Builder, n *Node, ind string) {
	switch n.Kind {
	case KNull:
		b.WriteString("null")
	case KBool:
		if n.Bool {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case KNum:
		b.WriteString(n.Str)
	case KStr:
		b.WriteString(quote(n.Str))
	case KArr:
		if len(n.Items) == 0 {
			b.WriteString("[]")
			return
		}
		simple := true
		for _, it := range n.Items {
			if it.Kind == KArr || it.Kind == KObj {
				simple = false
			}
		}
		if simple {
			b.WriteByte('[')
			for i, it := range n.Items {
				if i > 0 {
					b.WriteString(", ")
				}
				encode(b, it, ind)
			}
			b.WriteByte(']')
			return
		}
		b.WriteString("[\n")
		for i, it := range n.Items {
			b.WriteString(ind + "  ")
			encode(b, it, ind+"  ")
			if i < len(n.Items)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		b.WriteString(ind + "]")
	case KObj:
		if len(n.Members) == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteString("{\n")
		for i, m := range n.Members {
			b.WriteString(ind + "  " + quote(m.Key) + ": ")
			encode(b, m.Val, ind+"  ")
			if i < len(n.Members)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		b.WriteString(ind + "}")
	}
}

// literal renders a small value for messages, e.g. 100 or "x".
func literal(n *Node) string {
	if n == nil {
		return "null"
	}
	switch n.Kind {
	case KStr:
		return quote(n.Str)
	case KNum:
		return n.Str
	case KBool:
		if n.Bool {
			return "true"
		}
		return "false"
	case KNull:
		return "null"
	case KArr:
		return "[…]"
	default:
		return "{…}"
	}
}
