package main

import (
	"fmt"
	"strconv"
	"strings"
)

// Target is one JSON Schema found in an input file.
type Target struct {
	Label    string // human label: `tool "get_weather"`, `response format "invoice"`, `schema`
	Ptr      string // JSON pointer of the schema inside the file
	Schema   *Node
	Tool     bool
	Strict   bool  // strict: true was set explicitly
	NameNode *Node // tool or format name, if any
	NameKind string
}

func ptrEsc(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

func ptrJoin(base string, parts ...string) string {
	for _, p := range parts {
		base += "/" + ptrEsc(p)
	}
	return base
}

func isTrue(n *Node) bool { return n != nil && n.Kind == KBool && n.Bool }

// looksLikeSchema is true for an object that is plausibly a bare JSON Schema.
func looksLikeSchema(n *Node) bool {
	if !n.IsObj() {
		return false
	}
	for _, k := range []string{"type", "properties", "$schema", "anyOf", "oneOf", "allOf", "$defs", "definitions", "$ref", "items", "enum"} {
		if n.Get(k) != nil {
			return true
		}
	}
	return false
}

// Extract finds every schema in a document: bare schemas, OpenAI response_format /
// text.format wrappers, OpenAI Chat and Responses tools, Anthropic tools and
// output_config.format, and MCP tools/list results.
func Extract(root *Node) []Target {
	var out []Target
	extractInto(root, "", &out, 0)
	return out
}

func extractInto(n *Node, ptr string, out *[]Target, depth int) {
	if depth > 3 || n == nil {
		return
	}
	switch n.Kind {
	case KArr:
		for i, it := range n.Items {
			if t, ok := toolTarget(it, ptrJoin(ptr, strconv.Itoa(i))); ok {
				*out = append(*out, t)
			}
		}
		return
	case KObj:
	default:
		return
	}
	before := len(*out)
	// JSON-RPC tools/list response
	if r := n.Get("result"); r.IsObj() && r.Get("tools") != nil {
		extractInto(r, ptrJoin(ptr, "result"), out, depth+1)
	}
	if tools := n.Get("tools"); tools != nil && tools.Kind == KArr {
		for i, it := range tools.Items {
			if t, ok := toolTarget(it, ptrJoin(ptr, "tools", strconv.Itoa(i))); ok {
				*out = append(*out, t)
			}
		}
	}
	if t, ok := toolTarget(n, ptr); ok && depth == 0 {
		*out = append(*out, t)
	}
	// Chat Completions: response_format: {type: json_schema, json_schema: {name, strict, schema}}
	if rf := n.Get("response_format"); rf.IsObj() {
		if js := rf.Get("json_schema"); js.IsObj() {
			addWrapper(js, ptrJoin(ptr, "response_format", "json_schema"), out)
		}
	}
	// Responses API: text: {format: {type: json_schema, name, strict, schema}}
	if tx := n.Get("text"); tx.IsObj() {
		if f := tx.Get("format"); f.IsObj() && f.Get("schema") != nil {
			addWrapper(f, ptrJoin(ptr, "text", "format"), out)
		}
	}
	// Anthropic: output_config: {format: {type: json_schema, schema}}
	if oc := n.Get("output_config"); oc.IsObj() {
		if f := oc.Get("format"); f.IsObj() && f.Get("schema") != nil {
			addWrapper(f, ptrJoin(ptr, "output_config", "format"), out)
		}
	}
	if len(*out) > before {
		return
	}
	if js := n.Get("json_schema"); js.IsObj() && js.Get("schema") != nil {
		addWrapper(js, ptrJoin(ptr, "json_schema"), out)
		return
	}
	if s := n.Get("schema"); s.IsObj() && !looksLikeSchema(n) {
		addWrapper(n, ptr, out)
		return
	}
	if looksLikeSchema(n) {
		*out = append(*out, Target{Label: "schema", Ptr: ptr, Schema: n})
	}
}

func addWrapper(w *Node, ptr string, out *[]Target) {
	s := w.Get("schema")
	if s == nil {
		return
	}
	t := Target{Label: "response format", Ptr: ptrJoin(ptr, "schema"), Schema: s, Strict: isTrue(w.Get("strict")), NameKind: "format"}
	if nm := w.Get("name"); nm != nil && nm.Kind == KStr {
		t.Label = fmt.Sprintf("response format %q", nm.Str)
		t.NameNode = nm
	}
	*out = append(*out, t)
}

// toolTarget recognises one tool definition in any of the common shapes.
func toolTarget(n *Node, ptr string) (Target, bool) {
	if !n.IsObj() {
		return Target{}, false
	}
	t := Target{Tool: true, NameKind: "tool"}
	src := n
	switch {
	case n.Get("function").IsObj(): // OpenAI Chat Completions
		src = n.Get("function")
		t.Schema = src.Get("parameters")
		t.Ptr = ptrJoin(ptr, "function", "parameters")
	case n.Get("input_schema") != nil: // Anthropic
		t.Schema = n.Get("input_schema")
		t.Ptr = ptrJoin(ptr, "input_schema")
	case n.Get("inputSchema") != nil: // MCP
		t.Schema = n.Get("inputSchema")
		t.Ptr = ptrJoin(ptr, "inputSchema")
	case n.Get("parameters") != nil && n.Get("name") != nil: // OpenAI Responses
		t.Schema = n.Get("parameters")
		t.Ptr = ptrJoin(ptr, "parameters")
	default:
		return Target{}, false
	}
	t.Strict = isTrue(src.Get("strict")) || isTrue(n.Get("strict"))
	t.Label = "tool"
	if nm := src.Get("name"); nm != nil && nm.Kind == KStr {
		t.Label = fmt.Sprintf("tool %q", nm.Str)
		t.NameNode = nm
	}
	return t, true
}
