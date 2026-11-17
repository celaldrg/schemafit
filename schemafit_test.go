package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func both() map[string]bool { return map[string]bool{OpenAI: true, Anthropic: true} }

func run1(t *testing.T, src string) []Finding {
	t.Helper()
	root, dups, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return Check([]byte(src), root, dups, Extract(root), both())
}

// rules returns "provider:rule" for every finding.
func rules(fs []Finding) map[string]int {
	m := map[string]int{}
	for _, f := range fs {
		p := f.Target
		if p == "" {
			p = "all"
		}
		m[p+":"+f.Rule]++
	}
	return m
}

func expect(t *testing.T, src string, want ...string) {
	t.Helper()
	got := rules(run1(t, src))
	for _, w := range want {
		if got[w] == 0 {
			t.Errorf("expected %s, got %v", w, got)
		}
	}
}

func expectNot(t *testing.T, src string, not ...string) {
	t.Helper()
	got := rules(run1(t, src))
	for _, w := range not {
		if got[w] > 0 {
			t.Errorf("did not expect %s, got %v", w, got)
		}
	}
}

const strictOK = `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":["integer","null"]}},"required":["a","b"],"additionalProperties":false}`

func TestCleanSchemaHasNoFindings(t *testing.T) {
	if fs := run1(t, strictOK); len(fs) != 0 {
		t.Fatalf("expected no findings, got %+v", fs)
	}
}

func TestOpenAIRules(t *testing.T) {
	expect(t, `{"type":"object","properties":{"a":{"type":"string"}}}`, "openai:additional-properties", "openai:not-required", "anthropic:additional-properties")
	expect(t, `{"anyOf":[{"type":"object"},{"type":"string"}]}`, "openai:root-any-of")
	expect(t, `{"type":"array","items":{"type":"string"}}`, "openai:root-not-object")
	expect(t, `{"$ref":"#/$defs/x","$defs":{"x":{"type":"object","additionalProperties":false}}}`, "openai:root-not-object")
	expect(t, `{"type":"object","properties":{"u":{"type":"string","format":"uri"}},"required":["u"],"additionalProperties":false}`, "openai:unsupported-format")
	expectNot(t, `{"type":"object","properties":{"u":{"type":"string","format":"uri"}},"required":["u"],"additionalProperties":false}`, "anthropic:unsupported-format")
	expect(t, `{"type":"object","properties":{},"additionalProperties":false,"if":{},"then":{}}`, "openai:unsupported-keyword", "anthropic:undocumented-keyword")
	expect(t, `{"type":"object","properties":{"a":{"oneOf":[{"type":"string"},{"type":"integer"}]}},"required":["a"],"additionalProperties":false}`, "openai:undocumented-keyword")
}

func TestOpenAILimits(t *testing.T) {
	// 11 nested objects
	s := `{"type":"string"}`
	for i := 0; i < 11; i++ {
		s = `{"type":"object","properties":{"x":` + s + `},"required":["x"],"additionalProperties":false}`
	}
	expect(t, s, "openai:nesting-depth")
	// 10 levels is fine
	s = `{"type":"string"}`
	for i := 0; i < 10; i++ {
		s = `{"type":"object","properties":{"x":` + s + `},"required":["x"],"additionalProperties":false}`
	}
	expectNot(t, s, "openai:nesting-depth")

	var vals []string
	for i := 0; i < 1001; i++ {
		vals = append(vals, `"v`+strings.Repeat("x", i%5)+string(rune('a'+i%26))+`"`)
	}
	big := `{"type":"object","properties":{"e":{"type":"string","enum":[` + strings.Join(vals, ",") + `]}},"required":["e"],"additionalProperties":false}`
	expect(t, big, "openai:enum-budget")

	var long []string
	for i := 0; i < 300; i++ {
		long = append(long, `"`+strings.Repeat("y", 60)+string(rune('a'+i%26))+`"`)
	}
	expect(t, `{"type":"object","properties":{"e":{"type":"string","enum":[`+strings.Join(long, ",")+`]}},"required":["e"],"additionalProperties":false}`, "openai:enum-budget")
}

func TestAnthropicRules(t *testing.T) {
	obj := func(prop string) string {
		return `{"type":"object","properties":{"p":` + prop + `},"required":["p"],"additionalProperties":false}`
	}
	expect(t, obj(`{"type":"integer","minimum":1,"maximum":5}`), "anthropic:numeric-constraint")
	expect(t, obj(`{"type":"string","maxLength":5}`), "anthropic:string-length")
	expect(t, obj(`{"type":"array","items":{"type":"string"},"minItems":2}`), "anthropic:array-constraint")
	expectNot(t, obj(`{"type":"array","items":{"type":"string"},"minItems":1}`), "anthropic:array-constraint")
	expect(t, obj(`{"type":"array","items":{"type":"string"},"uniqueItems":true}`), "anthropic:array-constraint")
	expect(t, obj(`{"type":"string","enum":["a",["b"]]}`), "anthropic:complex-enum")
	expect(t, obj(`{"type":"string","enum":["Open","open"]}`), "anthropic:enum-case")
	expect(t, obj(`{"type":"string","pattern":"(a)\\1"}`), "anthropic:regex-feature")
	expect(t, obj(`{"type":"string","pattern":"^(?=.*\\d).+$"}`), "anthropic:regex-feature")
	expect(t, obj(`{"type":"string","pattern":"\\bword\\b"}`), "anthropic:regex-feature")
	expectNot(t, obj(`{"type":"string","pattern":"^a\\\\b$"}`), "anthropic:regex-feature")
	expect(t, obj(`{"type":"string","format":"iri"}`), "anthropic:unsupported-format", "openai:unsupported-format")
	expect(t, obj(`{"allOf":[{"$ref":"#/$defs/x"}]}`), "anthropic:allof-ref")
	expect(t, obj(`{"type":"object","properties":{},"additionalProperties":true}`), "anthropic:additional-properties", "openai:additional-properties")
}

func TestRecursion(t *testing.T) {
	rec := `{"type":"object","properties":{"root":{"$ref":"#/$defs/n"}},"required":["root"],"additionalProperties":false,
	"$defs":{"n":{"type":"object","properties":{"kids":{"type":"array","items":{"$ref":"#/$defs/n"}}},"required":["kids"],"additionalProperties":false}}}`
	expect(t, rec, "anthropic:recursive-schema")
	expectNot(t, rec, "openai:recursive-schema", "all:unresolved-ref")
	self := `{"type":"object","properties":{"next":{"anyOf":[{"$ref":"#"},{"type":"null"}]}},"required":["next"],"additionalProperties":false}`
	expect(t, self, "anthropic:recursive-schema")
	// a shared definition used twice is not recursion
	shared := `{"type":"object","properties":{"a":{"$ref":"#/$defs/x"},"b":{"$ref":"#/$defs/x"}},"required":["a","b"],"additionalProperties":false,
	"$defs":{"x":{"type":"string"}}}`
	expectNot(t, shared, "anthropic:recursive-schema")
}

func TestRefs(t *testing.T) {
	expect(t, `{"type":"object","properties":{"a":{"$ref":"#/$defs/missing"}},"required":["a"],"additionalProperties":false}`, "all:unresolved-ref")
	expect(t, `{"type":"object","properties":{"a":{"$ref":"https://example.com/a.json"}},"required":["a"],"additionalProperties":false}`, "all:external-ref")
	expect(t, `{"type":"object","properties":{"a":{"type":"strng"}},"required":["a","b"],"additionalProperties":false}`, "all:invalid-type", "all:required-unknown")
}

func TestAnthropicBudgets(t *testing.T) {
	var props, unions []string
	for i := 0; i < 25; i++ {
		props = append(props, `"o`+string(rune('a'+i))+`":{"type":"string"}`)
	}
	expect(t, `{"type":"object","properties":{`+strings.Join(props, ",")+`},"additionalProperties":false}`, "anthropic:optional-budget")
	for i := 0; i < 17; i++ {
		unions = append(unions, `"u`+string(rune('a'+i))+`":{"type":["string","null"]}`)
	}
	names := `"ua","ub","uc","ud","ue","uf","ug","uh","ui","uj","uk","ul","um","un","uo","up","uq"`
	expect(t, `{"type":"object","properties":{`+strings.Join(unions, ",")+`},"required":[`+names+`],"additionalProperties":false}`, "anthropic:union-budget")

	var tools []string
	for i := 0; i < 21; i++ {
		tools = append(tools, `{"name":"t`+string(rune('a'+i))+`","strict":true,"input_schema":`+strictOK+`}`)
	}
	expect(t, `[`+strings.Join(tools, ",")+`]`, "anthropic:strict-tool-budget")
	expectNot(t, `[`+strings.Join(tools[:20], ",")+`]`, "anthropic:strict-tool-budget")
}

func TestExtractShapes(t *testing.T) {
	cases := map[string]string{
		"bare":            strictOK,
		"chat format":     `{"model":"m","response_format":{"type":"json_schema","json_schema":{"name":"x","strict":true,"schema":` + strictOK + `}}}`,
		"responses text":  `{"model":"m","text":{"format":{"type":"json_schema","name":"x","schema":` + strictOK + `}}}`,
		"anthropic out":   `{"model":"m","output_config":{"format":{"type":"json_schema","schema":` + strictOK + `}}}`,
		"wrapper":         `{"name":"x","strict":true,"schema":` + strictOK + `}`,
		"chat tools":      `[{"type":"function","function":{"name":"f","strict":true,"parameters":` + strictOK + `}}]`,
		"responses tools": `{"tools":[{"type":"function","name":"f","strict":true,"parameters":` + strictOK + `}]}`,
		"anthropic tools": `{"tools":[{"name":"f","input_schema":` + strictOK + `}]}`,
		"mcp tools/list":  `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"f","inputSchema":` + strictOK + `}]}}`,
	}
	for name, src := range cases {
		root, _, err := Parse([]byte(src))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		ts := Extract(root)
		if len(ts) != 1 {
			t.Errorf("%s: want 1 schema, got %d", name, len(ts))
			continue
		}
		if fs := Check([]byte(src), root, nil, ts, both()); len(fs) != 0 {
			t.Errorf("%s: want clean, got %+v", name, fs)
		}
	}
}

func TestPositions(t *testing.T) {
	src := "{\n  \"type\": \"object\",\n  \"properties\": {\n    \"name\": {\"type\": \"string\"}\n  },\n  \"additionalProperties\": false\n}"
	for _, f := range run1(t, src) {
		if f.Rule == "not-required" {
			if f.Line != 4 || f.Col != 5 {
				t.Fatalf("not-required at %d:%d, want 4:5", f.Line, f.Col)
			}
			return
		}
	}
	t.Fatal("no not-required finding")
}

func TestDuplicateKeyAndInvalidJSON(t *testing.T) {
	expect(t, `{"type":"object","type":"string"}`, "all:duplicate-key")
	if _, _, err := Parse([]byte(`{"a":}`)); err == nil {
		t.Fatal("expected a syntax error")
	}
}

func fixAndCheck(t *testing.T, src, provider string) (string, []Finding) {
	t.Helper()
	root, _, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	Fix(Extract(root), provider)
	out := Encode(root)
	root2, dups, err := Parse([]byte(out))
	if err != nil {
		t.Fatalf("fix produced invalid JSON: %v\n%s", err, out)
	}
	return out, Check([]byte(out), root2, dups, Extract(root2), map[string]bool{provider: true})
}

func TestFixOpenAI(t *testing.T) {
	src := `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"integer","format":"int32"},"c":{"$ref":"#/$defs/c"},"d":{"type":"string","enum":["x","y"]}},"required":["a"],
	"$defs":{"c":{"type":"object","properties":{"z":{"type":"string"}}}}}`
	out, fs := fixAndCheck(t, src, OpenAI)
	if len(fs) != 0 {
		t.Fatalf("still has findings: %+v\n%s", fs, out)
	}
	for _, want := range []string{`"type": ["integer", "null"]`, `"enum": ["x", "y", null]`, `{format: int32}`, `"anyOf": [`} {
		if !strings.Contains(out, want) {
			t.Errorf("fixed output lacks %s:\n%s", want, out)
		}
	}
}

func TestFixAnthropic(t *testing.T) {
	src := `{"type":"object","properties":{"n":{"type":"integer","description":"Count.","minimum":1,"maximum":9},"s":{"type":"string","maxLength":20},"l":{"type":"array","items":{"type":"string"},"minItems":3,"maxItems":5}},"required":["n"]}`
	out, fs := fixAndCheck(t, src, Anthropic)
	if len(fs) != 0 {
		t.Fatalf("still has findings: %+v\n%s", fs, out)
	}
	for _, want := range []string{`"description": "Count. {minimum: 1, maximum: 9}"`, `{maxLength: 20}`, `"minItems": 1`, `"additionalProperties": false`} {
		if !strings.Contains(out, want) {
			t.Errorf("fixed output lacks %s:\n%s", want, out)
		}
	}
	// optional properties stay optional for Claude
	if strings.Contains(out, `"required": ["n", "s"`) {
		t.Errorf("anthropic fix should not make properties required:\n%s", out)
	}
}

func TestEncodeRoundTripKeepsOrder(t *testing.T) {
	src := `{"z":1,"a":[1,2],"m":{"b":true,"a":null},"s":"é<>&"}`
	root, _, _ := Parse([]byte(src))
	out := Encode(root)
	if strings.Index(out, `"z"`) > strings.Index(out, `"a"`) {
		t.Fatalf("key order lost: %s", out)
	}
	var a, b any
	_ = json.Unmarshal([]byte(src), &a)
	_ = json.Unmarshal([]byte(out), &b)
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if !bytes.Equal(ja, jb) {
		t.Fatalf("round trip changed value:\n%s\n%s", ja, jb)
	}
}

func TestCLIExamples(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"--no-color", filepath.Join("examples", "good.json")}, nil, &out, &errb)
	if code != 0 || !strings.Contains(out.String(), "OpenAI strict: fits") {
		t.Fatalf("good.json: exit %d\n%s%s", code, out.String(), errb.String())
	}
	for _, f := range []string{"invoice-request.json", "claude-tools.json"} {
		out.Reset()
		code = run([]string{"--no-color", filepath.Join("examples", f)}, nil, &out, &errb)
		if code != 1 || !strings.Contains(out.String(), "rejected") {
			t.Fatalf("%s: exit %d\n%s", f, code, out.String())
		}
	}
	out.Reset()
	code = run([]string{"--json", "--target", "anthropic", filepath.Join("examples", "claude-tools.json")}, nil, &out, &errb)
	var doc struct {
		Files []struct {
			Verdict  map[string]string `json:"verdict"`
			Findings []jsonFinding     `json:"findings"`
		} `json:"files"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("bad json: %v\n%s", err, out.String())
	}
	if code != 1 || doc.Files[0].Verdict["anthropic"] != "rejected" || doc.Files[0].Verdict["openai"] != "" {
		t.Fatalf("unexpected verdict %v (exit %d)", doc.Files[0].Verdict, code)
	}
	for _, f := range doc.Files[0].Findings {
		if f.Provider == "openai" {
			t.Fatalf("--target anthropic leaked an openai finding: %+v", f)
		}
	}
}

func TestCLIStdinAndUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"-"}, strings.NewReader(strictOK), &out, &errb); code != 0 {
		t.Fatalf("stdin: exit %d\n%s", code, out.String())
	}
	if code := run(nil, nil, &out, &errb); code != 2 {
		t.Fatalf("no args: exit %d", code)
	}
	if code := run([]string{"fix", "examples/good.json"}, nil, &out, &errb); code != 2 {
		t.Fatalf("fix without target: exit %d", code)
	}
	if code := run([]string{"does-not-exist.json"}, nil, &out, &errb); code != 2 {
		t.Fatalf("missing file: exit %d", code)
	}
	tmp := filepath.Join(t.TempDir(), "x.json")
	_ = os.WriteFile(tmp, []byte(`{"hello":"world"}`), 0o644)
	out.Reset()
	if code := run([]string{"--no-color", tmp}, nil, &out, &errb); code != 1 || !strings.Contains(out.String(), "no-schema") {
		t.Fatalf("no schema: exit %d\n%s", code, out.String())
	}
}
