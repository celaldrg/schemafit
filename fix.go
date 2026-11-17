package main

import (
	"sort"
	"strconv"
	"strings"
)

// Fix rewrites every schema in place so it fits one provider, the same way the
// official SDK helpers do, and returns a short log of what changed.
// Things that cannot be rewritten safely (allOf, recursion, if/then/else) are left
// for the checker to report.
func Fix(targets []Target, provider string) []string {
	f := &fixer{provider: provider, seen: map[*Node]bool{}}
	for _, t := range targets {
		f.label = t.Label + " "
		if len(targets) == 1 {
			f.label = ""
		}
		f.walk(t.Schema, "#")
	}
	return f.log
}

type fixer struct {
	provider string
	seen     map[*Node]bool
	log      []string
	label    string
}

func (f *fixer) logf(where, msg string) {
	f.log = append(f.log, f.label+where+": "+msg)
}

// constraints the Anthropic API rejects; the SDKs move them into the description.
var anMove = []string{"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf", "minLength", "maxLength", "maxItems", "uniqueItems"}

func (f *fixer) walk(n *Node, where string) {
	if !n.IsObj() || f.seen[n] {
		return
	}
	f.seen[n] = true
	var moved []string
	move := func(k string) {
		v := n.Get(k)
		moved = append(moved, k+": "+strings.Trim(literal(v), `"`))
		n.Delete(k)
	}
	if fm := n.Get("format"); fm != nil && fm.Kind == KStr {
		ok := oaFormats[fm.Str]
		if f.provider == Anthropic {
			ok = anFormats[fm.Str]
		}
		if !ok {
			move("format")
		}
	}
	if f.provider == Anthropic {
		for _, k := range anMove {
			if n.Get(k) != nil {
				move(k)
			}
		}
		if mi := n.Get("minItems"); mi != nil && mi.Kind == KNum && mi.Str != "0" && mi.Str != "1" {
			move("minItems")
			n.Set("minItems", &Node{Kind: KNum, Str: "1", Off: -1})
		}
	}
	if len(moved) > 0 {
		note := "{" + strings.Join(moved, ", ") + "}"
		desc := ""
		if d := n.Get("description"); d != nil && d.Kind == KStr {
			desc = d.Str + " "
		}
		n.Set("description", newStr(desc+note))
		f.logf(where, "moved "+strings.Join(keysOf(moved), ", ")+" into the description")
	}
	if n.Get("nullable") != nil {
		if nb := n.Get("nullable"); nb.Kind == KBool && nb.Bool {
			addNull(n)
		}
		n.Delete("nullable")
		f.logf(where, "replaced OpenAPI nullable with a null type")
	}
	if isObjectSchema(n) {
		if ap := n.Get("additionalProperties"); ap == nil || !(ap.Kind == KBool && !ap.Bool) {
			n.Set("additionalProperties", newBool(false))
			if ap != nil && ap.Kind == KObj {
				f.logf(where, "set additionalProperties: false (it was a map, so now it can only be {})")
			} else {
				f.logf(where, "set additionalProperties: false")
			}
		}
		if f.provider == OpenAI {
			f.requireAll(n, where)
		}
	}
	for _, k := range schemaMapKeys {
		if mm := n.Get(k); mm.IsObj() {
			for _, m := range mm.Members {
				f.walk(m.Val, where+"/"+k+"/"+ptrEsc(m.Key))
			}
		}
	}
	for _, k := range schemaListKeys {
		if l := n.Get(k); l != nil && l.Kind == KArr {
			for i, it := range l.Items {
				f.walk(it, where+"/"+k+"/"+strconv.Itoa(i))
			}
		}
	}
	for _, k := range schemaOneKeys {
		if ch := n.Get(k); ch.IsObj() {
			f.walk(ch, where+"/"+k)
		}
	}
}

func keysOf(moved []string) []string {
	out := make([]string, len(moved))
	for i, m := range moved {
		out[i] = m[:strings.Index(m, ":")]
	}
	return out
}

// requireAll makes every property required and turns optional ones into
// "may be null", which is how OpenAI strict mode expresses optional fields.
func (f *fixer) requireAll(n *Node, where string) {
	props := n.Get("properties")
	if !props.IsObj() {
		return
	}
	req := n.Get("required")
	if req == nil || req.Kind != KArr {
		req = newArr()
		n.Set("required", req)
	}
	have := map[string]bool{}
	for _, it := range req.Items {
		if it.Kind == KStr {
			have[it.Str] = true
		}
	}
	var made []string
	for _, m := range props.Members {
		if have[m.Key] {
			continue
		}
		req.Items = append(req.Items, newStr(m.Key))
		if m.Val.IsObj() {
			if !addNull(m.Val) {
				m.Val = wrapNull(m.Val)
			}
		}
		made = append(made, m.Key)
	}
	if len(made) > 0 {
		sort.Strings(made)
		f.logf(where, "made "+strings.Join(made, ", ")+" required and nullable")
	}
}

// addNull widens a schema's type to include null. It returns false when the
// schema has no type to widen (a bare $ref, for example).
func addNull(s *Node) bool {
	if a := s.Get("anyOf"); a != nil && a.Kind == KArr {
		for _, it := range a.Items {
			if it.IsObj() && has(typesOf(it), "null") {
				return true
			}
		}
		nb := newObj()
		nb.Set("type", newStr("null"))
		a.Items = append(a.Items, nb)
		return true
	}
	t := s.Get("type")
	if t == nil {
		return false
	}
	switch t.Kind {
	case KStr:
		if t.Str != "null" {
			s.Set("type", newArr(newStr(t.Str), newStr("null")))
		}
	case KArr:
		if !has(typesOf(s), "null") {
			t.Items = append(t.Items, newStr("null"))
		}
	default:
		return false
	}
	if e := s.Get("enum"); e != nil && e.Kind == KArr {
		hasNull := false
		for _, it := range e.Items {
			if it.Kind == KNull {
				hasNull = true
			}
		}
		if !hasNull {
			e.Items = append(e.Items, newNull())
		}
	}
	return true
}

func wrapNull(s *Node) *Node {
	w := newObj()
	if d := s.Get("description"); d != nil && s.Get("$ref") != nil {
		w.Set("description", d)
		s.Delete("description")
	}
	nb := newObj()
	nb.Set("type", newStr("null"))
	w.Set("anyOf", newArr(s, nb))
	return w
}
