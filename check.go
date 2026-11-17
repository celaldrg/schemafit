package main

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Severity of a finding. Error means the provider rejects the schema with a 400.
type Severity int

const (
	Info Severity = iota
	Warning
	Error
)

func (s Severity) String() string {
	return [...]string{"info", "warning", "error"}[s]
}

const (
	OpenAI    = "openai"
	Anthropic = "anthropic"
)

// Finding is one problem in one schema for one provider.
type Finding struct {
	Line, Col int
	Off       int
	Sev       Severity
	Target    string // openai, anthropic or "" for both
	Rule      string
	Msg       string
	Where     string // JSON pointer inside the schema, "#/properties/x"
	Label     string // which schema in the file
	Idx       int    // index of the schema in the file, for ordering
}

// Limits documented by the providers (November 2026).
const (
	oaMaxProps        = 5000
	oaMaxDepth        = 10
	oaMaxStringBudget = 120000
	oaMaxEnumValues   = 1000
	oaBigEnum         = 250
	oaBigEnumChars    = 15000
	anMaxOptional     = 24
	anMaxUnion        = 16
	anMaxStrictTools  = 20
	anRegexBound      = 100
)

var oaFormats = set("date-time", "time", "date", "duration", "email", "hostname", "ipv4", "ipv6", "uuid")
var anFormats = set("date-time", "time", "date", "duration", "email", "hostname", "uri", "ipv4", "ipv6", "uuid")
var jsonTypes = set("string", "number", "integer", "boolean", "object", "array", "null")

// Keywords that never affect validation and are safe to leave in.
var annotations = set("title", "description", "default", "examples", "$schema", "$id", "$comment",
	"deprecated", "readOnly", "writeOnly", "$anchor", "contentMediaType", "contentEncoding", "propertyOrdering")

// JSON Schema validation keywords neither provider documents as supported.
var undocumented = set("propertyNames", "minProperties", "maxProperties", "unevaluatedProperties",
	"unevaluatedItems", "dependencies", "$dynamicRef", "$dynamicAnchor", "$recursiveRef", "$recursiveAnchor",
	"additionalItems", "minContains", "maxContains")

func set(xs ...string) map[string]bool {
	m := map[string]bool{}
	for _, x := range xs {
		m[x] = true
	}
	return m
}

type checker struct {
	loc     *Locator
	want    map[string]bool
	out     []Finding
	t       *Target
	root    *Node
	idx     int
	countAN bool // this schema counts toward Anthropic's per-request budgets

	// per-target OpenAI counters
	oaProps, oaEnum, oaChars, oaDepth int
	oaDepthNode                       *Node
	// per-file Anthropic counters (one request)
	anOptional, anUnion, anStrict int
	anOptNode, anUnionNode        *Node
	anOptLabel, anUnionLabel      string
}

func (c *checker) add(n *Node, off int, sev Severity, target, rule, where, format string, a ...any) {
	if target != "" && !c.want[target] {
		return
	}
	if off < 0 && n != nil {
		off = n.Off
	}
	l, col := c.loc.Pos(off)
	label := ""
	if c.t != nil {
		label = c.t.Label
	}
	c.out = append(c.out, Finding{Line: l, Col: col, Off: off, Sev: sev, Target: target, Rule: rule, Msg: fmt.Sprintf(format, a...), Where: where, Label: label, Idx: c.idx})
}

func memberOff(n *Node, key string) int {
	if m := n.Member(key); m != nil && m.KeyOff >= 0 {
		return m.KeyOff
	}
	return n.Off
}

func typesOf(n *Node) []string {
	t := n.Get("type")
	if t == nil {
		return nil
	}
	if t.Kind == KStr {
		return []string{t.Str}
	}
	var out []string
	if t.Kind == KArr {
		for _, it := range t.Items {
			if it.Kind == KStr {
				out = append(out, it.Str)
			}
		}
	}
	return out
}

func has(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func isObjectSchema(n *Node) bool {
	ts := typesOf(n)
	if len(ts) == 0 {
		return n.Get("properties") != nil
	}
	return has(ts, "object")
}

// Check runs every rule on one file's targets. want selects providers.
func Check(src []byte, root *Node, dups []DupKey, targets []Target, want map[string]bool) []Finding {
	c := &checker{loc: NewLocator(src), want: want, idx: -1}
	anyStrict := false
	for _, t := range targets {
		if t.Tool && t.Strict {
			anyStrict = true
		}
	}
	for _, d := range dups {
		c.add(nil, d.Off, Warning, "", "duplicate-key", "", "key %q appears twice in one object; most parsers silently keep the last one", d.Key)
	}
	for i := range targets {
		t := &targets[i]
		c.t = t
		c.idx = i
		// Tools only count toward the budgets when strict. If no tool in the file is
		// strict yet, assume the user is checking them for strict mode and count all.
		c.countAN = !t.Tool || t.Strict || !anyStrict
		c.root = t.Schema
		c.oaProps, c.oaEnum, c.oaChars, c.oaDepth, c.oaDepthNode = 0, 0, 0, 0, nil
		c.checkName(t)
		c.checkRoot(t.Schema)
		c.visit(t.Schema, "#", 0, map[*Node]bool{})
		c.checkRecursion(t.Schema)
		c.finishOpenAI(t.Schema)
		if t.Tool && t.Strict {
			c.anStrict++
		}
	}
	c.t = nil
	c.idx = len(targets)
	c.finishAnthropic(root)
	sort.SliceStable(c.out, func(i, j int) bool {
		if c.out[i].Idx != c.out[j].Idx {
			return c.out[i].Idx < c.out[j].Idx
		}
		return c.out[i].Off < c.out[j].Off
	})
	return c.out
}

var nameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

func (c *checker) checkName(t *Target) {
	if t.NameNode == nil || nameRe.MatchString(t.NameNode.Str) {
		return
	}
	c.add(t.NameNode, -1, Error, "", "invalid-name", "", "%s name %q may only use letters, digits, _ and - (max 64)", t.NameKind, t.NameNode.Str)
}

func (c *checker) checkRoot(s *Node) {
	if s == nil {
		return
	}
	if !s.IsObj() {
		if s.Kind == KBool {
			return // reported by visit
		}
		c.add(s, -1, Error, "", "invalid-schema", "#", "a schema must be a JSON object, got %s", literal(s))
		return
	}
	if s.Get("anyOf") != nil && len(typesOf(s)) == 0 {
		c.add(s, memberOff(s, "anyOf"), Error, OpenAI, "root-any-of", "#", "the root schema uses anyOf; OpenAI needs a single object at the root (wrap the union in a property)")
		return
	}
	ts := typesOf(s)
	if !isObjectSchema(s) || len(ts) > 1 {
		got := "no type"
		if len(ts) > 0 {
			got = "type " + strings.Join(ts, "|")
		}
		if s.Get("$ref") != nil && len(ts) == 0 {
			got = "a bare $ref"
		}
		c.add(s, -1, Error, OpenAI, "root-not-object", "#", "the root schema must be {\"type\": \"object\"}, found %s", got)
	}
}

var schemaMapKeys = []string{"properties", "$defs", "definitions", "patternProperties", "dependentSchemas"}
var schemaListKeys = []string{"anyOf", "allOf", "oneOf", "prefixItems"}
var schemaOneKeys = []string{"items", "not", "if", "then", "else", "contains", "additionalProperties", "propertyNames", "unevaluatedProperties", "additionalItems"}

func (c *checker) visit(n *Node, where string, depth int, seen map[*Node]bool) {
	if n == nil || seen[n] {
		return
	}
	seen[n] = true
	if n.Kind == KBool {
		c.add(n, -1, Error, OpenAI, "boolean-schema", where, "boolean schema %s is not supported; write an object schema", literal(n))
		c.add(n, -1, Warning, Anthropic, "boolean-schema", where, "boolean schema %s is not in the supported subset; write an object schema", literal(n))
		return
	}
	if !n.IsObj() {
		c.add(n, -1, Error, "", "invalid-schema", where, "a schema must be a JSON object, got %s", literal(n))
		return
	}
	// type
	if t := n.Get("type"); t != nil {
		ok := t.Kind == KStr || t.Kind == KArr
		for _, ty := range typesOf(n) {
			if !jsonTypes[ty] {
				ok = false
			}
		}
		if t.Kind == KArr && len(typesOf(n)) != len(t.Items) {
			ok = false
		}
		if !ok {
			c.add(t, -1, Error, "", "invalid-type", where, "type %s is not a JSON Schema type (string, number, integer, boolean, object, array, null)", literal(t))
		}
		if len(typesOf(n)) > 1 {
			c.unionParam(n, where)
		}
	}
	for _, m := range n.Members {
		c.keyword(n, m, where)
	}
	if isObjectSchema(n) {
		depth++
		if depth > c.oaDepth {
			c.oaDepth, c.oaDepthNode = depth, n
		}
		c.objectRules(n, where)
	}
	// children
	for _, k := range schemaMapKeys {
		if mm := n.Get(k); mm.IsObj() {
			for _, m := range mm.Members {
				c.visit(m.Val, where+"/"+k+"/"+ptrEsc(m.Key), depth, seen)
			}
		}
	}
	for _, k := range schemaListKeys {
		if l := n.Get(k); l != nil && l.Kind == KArr {
			for i, it := range l.Items {
				c.visit(it, where+"/"+k+"/"+strconv.Itoa(i), depth, seen)
			}
		}
	}
	for _, k := range schemaOneKeys {
		if ch := n.Get(k); ch.IsObj() {
			c.visit(ch, where+"/"+k, depth, seen)
		} else if k == "items" && ch != nil && ch.Kind == KArr {
			for i, it := range ch.Items {
				c.visit(it, where+"/items/"+strconv.Itoa(i), depth, seen)
			}
		}
	}
}

func (c *checker) unionParam(n *Node, where string) {
	if !c.countAN {
		return
	}
	c.anUnion++
	if c.anUnionNode == nil {
		c.anUnionNode, c.anUnionLabel = n, c.t.Label
	}
}

func (c *checker) keyword(n *Node, m *Member, where string) {
	k, v := m.Key, m.Val
	off := m.KeyOff
	switch k {
	case "type", "properties", "required", "items", "$defs", "definitions", "additionalProperties":
		// handled elsewhere
	case "anyOf":
		if v.Kind == KArr && len(v.Items) > 1 {
			c.unionParam(n, where)
		}
	case "enum":
		c.enumRules(v, where)
	case "const":
		if v.Kind == KStr {
			c.oaChars += len(v.Str)
		}
		if v.Kind == KArr || v.Kind == KObj {
			c.add(v, off, Warning, Anthropic, "complex-enum", where, "const holds an %s; only strings, numbers, booleans and null are documented", map[Kind]string{KArr: "array", KObj: "object"}[v.Kind])
		}
	case "format":
		if v.Kind != KStr {
			break
		}
		if !oaFormats[v.Str] {
			c.add(v, -1, Error, OpenAI, "unsupported-format", where, "format %q is not supported (date-time, time, date, duration, email, hostname, ipv4, ipv6, uuid)", v.Str)
		}
		if !anFormats[v.Str] {
			c.add(v, -1, Error, Anthropic, "unsupported-format", where, "format %q is not supported (date-time, time, date, duration, email, hostname, uri, ipv4, ipv6, uuid)", v.Str)
		}
	case "pattern":
		if v.Kind == KStr {
			c.regexRules(v, where)
		}
	case "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf":
		c.add(n, off, Error, Anthropic, "numeric-constraint", where, "%s is rejected; move it into the description", k)
	case "minLength", "maxLength":
		c.add(n, off, Error, Anthropic, "string-length", where, "%s is rejected; move it into the description", k)
	case "minItems":
		if v.Kind == KNum && v.Str != "0" && v.Str != "1" {
			c.add(v, off, Error, Anthropic, "array-constraint", where, "minItems %s is not supported; only 0 and 1 are allowed", v.Str)
		}
	case "maxItems", "uniqueItems", "contains", "prefixItems":
		c.add(n, off, Error, Anthropic, "array-constraint", where, "%s is rejected; the only array constraint allowed is minItems 0 or 1", k)
		if k != "maxItems" {
			c.add(n, off, Warning, OpenAI, "undocumented-keyword", where, "%s is not in the documented subset (arrays support items, minItems, maxItems)", k)
		}
	case "allOf":
		c.add(n, off, Error, OpenAI, "unsupported-keyword", where, "allOf is not supported; merge the parts into one object")
		if v.Kind == KArr {
			for _, it := range v.Items {
				if it.IsObj() && it.Get("$ref") != nil {
					c.add(it, -1, Error, Anthropic, "allof-ref", where, "allOf with $ref is not supported; inline the referenced schema")
					break
				}
			}
		}
	case "not", "if", "then", "else", "dependentRequired", "dependentSchemas":
		c.add(n, off, Error, OpenAI, "unsupported-keyword", where, "%s is not supported in strict mode", k)
		c.add(n, off, Warning, Anthropic, "undocumented-keyword", where, "%s is not in the documented subset", k)
	case "oneOf":
		c.add(n, off, Warning, OpenAI, "undocumented-keyword", where, "oneOf is not in the documented subset; use anyOf")
		c.add(n, off, Warning, Anthropic, "undocumented-keyword", where, "oneOf is not in the documented subset; use anyOf")
		if v.Kind == KArr && len(v.Items) > 1 {
			c.unionParam(n, where)
		}
	case "patternProperties":
		c.add(n, off, Warning, OpenAI, "undocumented-keyword", where, "patternProperties is not in the documented subset; strict mode only returns listed properties")
		c.add(n, off, Error, Anthropic, "unsupported-keyword", where, "patternProperties cannot work: additionalProperties must be false, so only listed keys are produced (use an array of {key, value})")
	case "$ref":
		c.refRules(n, v, off, where)
	case "nullable":
		c.add(n, off, Warning, "", "openapi-nullable", where, "nullable is OpenAPI 3.0, not JSON Schema; write \"type\": [\"<type>\", \"null\"]")
	default:
		if undocumented[k] {
			c.add(n, off, Warning, "", "undocumented-keyword", where, "%s is not in the documented subset of either provider", k)
		}
	}
}

func (c *checker) enumRules(v *Node, where string) {
	if v.Kind != KArr {
		c.add(v, -1, Error, "", "invalid-enum", where, "enum must be an array")
		return
	}
	c.oaEnum += len(v.Items)
	chars, nstr := 0, 0
	lower := map[string]string{}
	for _, it := range v.Items {
		switch it.Kind {
		case KStr:
			chars += len(it.Str)
			nstr++
			l := strings.ToLower(it.Str)
			if prev, ok := lower[l]; ok && prev != it.Str {
				c.add(it, -1, Warning, Anthropic, "enum-case", where, "enum values %q and %q differ only in case; Claude may return either", prev, it.Str)
			}
			lower[l] = it.Str
		case KArr, KObj:
			c.add(it, -1, Error, Anthropic, "complex-enum", where, "enum value %s is not allowed; enums may hold only strings, numbers, booleans or null", literal(it))
		}
	}
	c.oaChars += chars
	if nstr > oaBigEnum && chars > oaBigEnumChars {
		c.add(v, -1, Error, OpenAI, "enum-budget", where, "an enum with %d string values may total at most %d characters, this one has %d", nstr, oaBigEnumChars, chars)
	}
}

var backrefRe = regexp.MustCompile(`\\[1-9]|\\k<`)
var boundRe = regexp.MustCompile(`\{(\d+)(,(\d*))?\}`)

func (c *checker) regexRules(v *Node, where string) {
	p := v.Str
	// strip escaped backslashes so "\\b" (a literal backslash then b) is not read as \b
	clean := strings.ReplaceAll(p, `\\`, "")
	switch {
	case backrefRe.MatchString(clean):
		c.add(v, -1, Error, Anthropic, "regex-feature", where, "pattern %s uses a backreference, which is not supported", quote(p))
	case strings.Contains(clean, "(?=") || strings.Contains(clean, "(?!") || strings.Contains(clean, "(?<=") || strings.Contains(clean, "(?<!"):
		c.add(v, -1, Error, Anthropic, "regex-feature", where, "pattern %s uses a lookaround, which is not supported", quote(p))
	case strings.Contains(clean, `\b`) || strings.Contains(clean, `\B`):
		c.add(v, -1, Error, Anthropic, "regex-feature", where, "pattern %s uses a word boundary (\\b), which is not supported", quote(p))
	}
	for _, m := range boundRe.FindAllStringSubmatch(clean, -1) {
		hi := m[1]
		if m[3] != "" {
			hi = m[3]
		}
		if n, err := strconv.Atoi(hi); err == nil && n > anRegexBound {
			c.add(v, -1, Warning, Anthropic, "regex-feature", where, "pattern quantifier %s has a large range; complex patterns can fail to compile", m[0])
			break
		}
	}
}

func (c *checker) refRules(n, v *Node, off int, where string) {
	if v.Kind != KStr {
		c.add(v, -1, Error, "", "invalid-ref", where, "$ref must be a string")
		return
	}
	if !strings.HasPrefix(v.Str, "#") {
		c.add(v, -1, Error, "", "external-ref", where, "$ref %q points outside the schema; only local refs (#/$defs/...) work, inline it", v.Str)
		return
	}
	if resolve(c.root, v.Str) == nil {
		c.add(v, -1, Error, "", "unresolved-ref", where, "$ref %q does not resolve inside this schema", v.Str)
	}
	if len(n.Members) > 1 {
		for _, m := range n.Members {
			if m.Key != "$ref" && !annotations[m.Key] {
				c.add(n, off, Info, "", "ref-siblings", where, "keywords next to $ref (%s) may be ignored; put them in the referenced schema", m.Key)
				break
			}
		}
	}
}

// resolve follows a local JSON pointer ref ("#", "#/$defs/x").
func resolve(root *Node, ref string) *Node {
	if ref == "#" {
		return root
	}
	if !strings.HasPrefix(ref, "#/") {
		return nil
	}
	cur := root
	for _, part := range strings.Split(ref[2:], "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		if dec, err := unescapePct(part); err == nil {
			part = dec
		}
		switch {
		case cur.IsObj():
			cur = cur.Get(part)
		case cur != nil && cur.Kind == KArr:
			i, err := strconv.Atoi(part)
			if err != nil || i < 0 || i >= len(cur.Items) {
				return nil
			}
			cur = cur.Items[i]
		default:
			return nil
		}
		if cur == nil {
			return nil
		}
	}
	return cur
}

func unescapePct(s string) (string, error) {
	if !strings.Contains(s, "%") {
		return s, nil
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			v, err := strconv.ParseUint(s[i+1:i+3], 16, 8)
			if err != nil {
				return s, err
			}
			b.WriteByte(byte(v))
			i += 2
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String(), nil
}

func (c *checker) objectRules(n *Node, where string) {
	ap := n.Get("additionalProperties")
	switch {
	case ap == nil:
		c.add(n, -1, Error, OpenAI, "additional-properties", where, "object needs \"additionalProperties\": false")
		c.add(n, -1, Warning, Anthropic, "additional-properties", where, "object needs \"additionalProperties\": false (SDK helpers add it, raw requests do not)")
	case !(ap.Kind == KBool && !ap.Bool):
		c.add(ap, memberOff(n, "additionalProperties"), Error, OpenAI, "additional-properties", where, "additionalProperties must be false, found %s", literal(ap))
		c.add(ap, memberOff(n, "additionalProperties"), Error, Anthropic, "additional-properties", where, "additionalProperties must be false, found %s (use an array of {key, value})", literal(ap))
	}
	req := map[string]bool{}
	if r := n.Get("required"); r != nil {
		if r.Kind != KArr {
			c.add(r, -1, Error, "", "invalid-required", where, "required must be an array of property names")
		} else {
			for _, it := range r.Items {
				if it.Kind == KStr {
					req[it.Str] = true
				}
			}
		}
	}
	props := n.Get("properties")
	if props != nil && !props.IsObj() {
		c.add(props, -1, Error, "", "invalid-properties", where, "properties must be an object")
		props = nil
	}
	if props == nil || len(props.Members) == 0 {
		c.add(n, -1, Warning, "", "empty-object", where, "object lists no properties, so strict mode can only return {}")
	}
	var missing []*Member
	if props != nil {
		c.oaProps += len(props.Members)
		for _, m := range props.Members {
			c.oaChars += len(m.Key)
			if !req[m.Key] {
				missing = append(missing, m)
				if !c.countAN {
					continue
				}
				c.anOptional++
				if c.anOptNode == nil {
					c.anOptNode, c.anOptLabel = m.Val, c.t.Label
				}
			}
		}
	}
	for _, m := range missing {
		pw := where + "/properties/" + ptrEsc(m.Key)
		c.add(nil, m.KeyOff, Error, OpenAI, "not-required", pw, "%q is optional; strict mode needs every property in required (allow null instead)", m.Key)
	}
	if r := n.Get("required"); r != nil && r.Kind == KArr {
		for _, it := range r.Items {
			if it.Kind == KStr && (props == nil || props.Get(it.Str) == nil) {
				c.add(it, -1, Error, "", "required-unknown", where, "required lists %q, which is not in properties", it.Str)
			}
		}
	}
	for _, k := range []string{"$defs", "definitions"} {
		if d := n.Get(k); d.IsObj() {
			for _, m := range d.Members {
				c.oaChars += len(m.Key)
			}
		}
	}
}

func (c *checker) finishOpenAI(s *Node) {
	if c.oaDepth > oaMaxDepth {
		c.add(c.oaDepthNode, -1, Error, OpenAI, "nesting-depth", "", "objects are nested %d levels deep; the limit is %d", c.oaDepth, oaMaxDepth)
	}
	if c.oaProps > oaMaxProps {
		c.add(s, -1, Error, OpenAI, "too-many-properties", "#", "schema has %d object properties; the limit is %d", c.oaProps, oaMaxProps)
	}
	if c.oaEnum > oaMaxEnumValues {
		c.add(s, -1, Error, OpenAI, "enum-budget", "#", "schema has %d enum values in total; the limit is %d", c.oaEnum, oaMaxEnumValues)
	}
	if c.oaChars > oaMaxStringBudget {
		c.add(s, -1, Error, OpenAI, "string-budget", "#", "property names, definition names, enum and const values total %d characters; the limit is %d", c.oaChars, oaMaxStringBudget)
	}
}

// checkRecursion reports the first $ref that makes the schema recursive (DFS with colours).
func (c *checker) checkRecursion(root *Node) {
	if !c.want[Anthropic] || !root.IsObj() {
		return
	}
	const (
		white = 0
		grey  = 1
		black = 2
	)
	colour := map[*Node]int{}
	var found *Node
	var where string
	var dfs func(n *Node, w string)
	dfs = func(n *Node, w string) {
		if found != nil || !n.IsObj() {
			return
		}
		colour[n] = grey
		if r := n.Get("$ref"); r != nil && r.Kind == KStr {
			if t := resolve(root, r.Str); t != nil {
				if colour[t] == grey {
					found, where = r, w
				} else if colour[t] == white {
					dfs(t, r.Str)
				}
			}
		}
		for _, k := range schemaMapKeys {
			if k == "$defs" || k == "definitions" {
				continue // definitions only matter when referenced
			}
			if mm := n.Get(k); mm.IsObj() {
				for _, m := range mm.Members {
					if colour[m.Val] != black {
						dfs(m.Val, w+"/"+k+"/"+ptrEsc(m.Key))
					}
				}
			}
		}
		for _, k := range schemaListKeys {
			if l := n.Get(k); l != nil && l.Kind == KArr {
				for i, it := range l.Items {
					if colour[it] != black {
						dfs(it, w+"/"+k+"/"+strconv.Itoa(i))
					}
				}
			}
		}
		for _, k := range schemaOneKeys {
			if ch := n.Get(k); ch.IsObj() && colour[ch] != black {
				dfs(ch, w+"/"+k)
			}
		}
		colour[n] = black
	}
	dfs(root, "#")
	if found != nil {
		c.add(found, -1, Error, Anthropic, "recursive-schema", where, "$ref %q makes the schema recursive; unroll a few levels instead", found.Str)
	}
}

func (c *checker) finishAnthropic(root *Node) {
	if c.anOptional > anMaxOptional {
		c.t = &Target{Label: c.anOptLabel}
		c.add(c.anOptNode, -1, Error, Anthropic, "optional-budget", "", "%d optional properties across the schemas in this file; one request allows %d (each one roughly doubles part of the grammar)", c.anOptional, anMaxOptional)
	}
	if c.anUnion > anMaxUnion {
		c.t = &Target{Label: c.anUnionLabel}
		c.add(c.anUnionNode, -1, Error, Anthropic, "union-budget", "", "%d parameters use anyOf or type arrays across this file; one request allows %d", c.anUnion, anMaxUnion)
	}
	if c.anStrict > anMaxStrictTools {
		c.t = nil
		c.add(root, -1, Error, Anthropic, "strict-tool-budget", "", "%d tools set strict: true; one request allows %d", c.anStrict, anMaxStrictTools)
	}
	c.t = nil
}
