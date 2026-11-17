// Command schemafit checks JSON Schemas against the strict-mode rules of
// OpenAI Structured Outputs and Claude structured outputs / strict tool use,
// so a schema fails in CI instead of as a 400 in production.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

var version = "0.1.0"

const usage = `schemafit: will your JSON Schema survive strict mode?

Usage:
  schemafit [options] <file | ->...        check schemas
  schemafit fix --target <p> <file | ->    print the file rewritten to fit one provider

Input can be a bare JSON Schema, an OpenAI response_format / text.format wrapper,
a tools array (OpenAI Chat, OpenAI Responses, Anthropic), a full request body,
or an MCP tools/list result. Every schema inside is checked.

Options:
  --target <list>   providers to check: openai, anthropic (default: both)
  --json            machine-readable output
  --strict          exit 1 on warnings too
  --quiet           only show errors
  --no-color        disable colors (also NO_COLOR=1)
  --version         print the version

Exit codes: 0 fits, 1 problems found, 2 usage or read error.
`

type opts struct {
	targets map[string]bool
	json    bool
	strict  bool
	quiet   bool
	color   bool
	files   []string
	fix     bool
	noColor bool
}

func parseArgs(args []string) (*opts, error) {
	o := &opts{targets: map[string]bool{OpenAI: true, Anthropic: true}, color: true}
	if len(args) > 0 && (args[0] == "fix" || args[0] == "check") {
		o.fix = args[0] == "fix"
		args = args[1:]
	}
	explicit := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		val := ""
		if k, v, ok := strings.Cut(a, "="); ok && strings.HasPrefix(a, "--") {
			a, val = k, v
		}
		switch a {
		case "--json":
			o.json = true
		case "--strict":
			o.strict = true
		case "--quiet", "-q":
			o.quiet = true
		case "--no-color":
			o.color = false
			o.noColor = true
		case "--target", "-t":
			if val == "" {
				if i+1 >= len(args) {
					return nil, errors.New("--target needs a value")
				}
				i++
				val = args[i]
			}
			o.targets = map[string]bool{}
			explicit = true
			for _, p := range strings.Split(val, ",") {
				switch strings.ToLower(strings.TrimSpace(p)) {
				case "openai", "oai", "gpt":
					o.targets[OpenAI] = true
				case "anthropic", "claude":
					o.targets[Anthropic] = true
				case "all", "both":
					o.targets[OpenAI], o.targets[Anthropic] = true, true
				default:
					return nil, fmt.Errorf("unknown target %q (use openai, anthropic)", p)
				}
			}
		case "-h", "--help":
			fmt.Print(usage)
			os.Exit(0)
		case "--version", "-v":
			fmt.Println("schemafit " + version)
			os.Exit(0)
		default:
			if strings.HasPrefix(a, "-") && a != "-" {
				return nil, fmt.Errorf("unknown option %s", a)
			}
			o.files = append(o.files, args[i])
		}
	}
	if len(o.files) == 0 {
		return nil, errors.New("no input files")
	}
	if o.fix && (!explicit || len(o.targets) != 1) {
		return nil, errors.New("fix needs exactly one --target (openai or anthropic)")
	}
	if o.fix && len(o.files) != 1 {
		return nil, errors.New("fix takes one file")
	}
	return o, nil
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func readInput(name string, stdin io.Reader) ([]byte, error) {
	if name == "-" {
		return io.ReadAll(stdin)
	}
	return os.ReadFile(name)
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	o, err := parseArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "schemafit: %v\n\n%s", err, usage)
		return 2
	}
	if o.color {
		f, ok := stdout.(*os.File)
		o.color = ok && isTerminal(f) && os.Getenv("NO_COLOR") == ""
		if forceColor() {
			o.color = true
		}
	}
	if o.fix {
		return runFix(o, stdin, stdout, stderr)
	}
	p := &printer{w: stdout, color: o.color, quiet: o.quiet, want: o.targets}
	var reports []fileReport
	code := 0
	for _, name := range o.files {
		r := checkFile(name, stdin, o.targets)
		reports = append(reports, r)
		if r.readErr != "" {
			code = 2
		}
	}
	for _, r := range reports {
		if r.readErr == "" && (r.errors > 0 || (o.strict && r.warnings > 0)) && code == 0 {
			code = 1
		}
	}
	if o.json {
		writeJSON(stdout, reports, o.targets)
	} else {
		for _, r := range reports {
			p.file(r)
		}
		p.summary(reports)
	}
	return code
}

type fileReport struct {
	name     string
	readErr  string
	targets  []Target
	findings []Finding
	errors   int
	warnings int
	notes    int
}

func checkFile(name string, stdin io.Reader, want map[string]bool) fileReport {
	r := fileReport{name: name}
	if name == "-" {
		r.name = "<stdin>"
	}
	data, err := readInput(name, stdin)
	if err != nil {
		r.readErr = err.Error()
		return r
	}
	root, dups, err := Parse(data)
	if err != nil {
		var se *SyntaxError
		if errors.As(err, &se) {
			l, c := NewLocator(data).Pos(se.Off)
			r.findings = []Finding{{Line: l, Col: c, Sev: Error, Rule: "invalid-json", Msg: se.Msg}}
			r.errors = 1
			return r
		}
		r.readErr = err.Error()
		return r
	}
	r.targets = Extract(root)
	if len(r.targets) == 0 {
		r.findings = []Finding{{Line: 1, Col: 1, Sev: Error, Rule: "no-schema", Msg: "no JSON Schema found (expected a schema, a response_format, a tools array or a tools/list result)"}}
		r.errors = 1
		return r
	}
	r.findings = Check(data, root, dups, r.targets, want)
	for _, f := range r.findings {
		switch f.Sev {
		case Error:
			r.errors++
		case Warning:
			r.warnings++
		default:
			r.notes++
		}
	}
	return r
}

func runFix(o *opts, stdin io.Reader, stdout, stderr io.Writer) int {
	provider := OpenAI
	if o.targets[Anthropic] {
		provider = Anthropic
	}
	data, err := readInput(o.files[0], stdin)
	if err != nil {
		fmt.Fprintf(stderr, "schemafit: %v\n", err)
		return 2
	}
	root, _, err := Parse(data)
	if err != nil {
		fmt.Fprintf(stderr, "schemafit: %s: invalid JSON: %v\n", o.files[0], err)
		return 2
	}
	targets := Extract(root)
	if len(targets) == 0 {
		fmt.Fprintf(stderr, "schemafit: %s: no JSON Schema found\n", o.files[0])
		return 2
	}
	ec := func(code, s string) string { return s }
	f, ok := stderr.(*os.File)
	if !o.noColor && ((ok && isTerminal(f) && os.Getenv("NO_COLOR") == "") || forceColor()) {
		ec = func(code, s string) string { return "\x1b[" + code + "m" + s + "\x1b[0m" }
	}
	log := Fix(targets, provider)
	out := Encode(root)
	fmt.Fprint(stdout, out)
	for _, l := range log {
		fmt.Fprintf(stderr, "%s  %s\n", ec("32", "fixed"), l)
	}
	// re-check the result so what is left is visible
	root2, dups2, _ := Parse([]byte(out))
	t2 := Extract(root2)
	left := 0
	for _, f := range Check([]byte(out), root2, dups2, t2, map[string]bool{provider: true}) {
		if f.Sev == Error {
			left++
			fmt.Fprintf(stderr, "%s   %s: %s\n", ec("31", "left"), strings.TrimSpace(f.Label+" "+f.Where), f.Msg)
		}
	}
	if left > 0 {
		fmt.Fprintf(stderr, "%s\n", ec("31;1", fmt.Sprintf("%d error(s) need a manual change", left)))
		return 1
	}
	fmt.Fprintf(stderr, "%s\n", ec("32;1", fmt.Sprintf("fits %s strict mode (%d change(s))", provider, len(log))))
	return 0
}

type jsonFinding struct {
	File     string `json:"file"`
	Schema   string `json:"schema,omitempty"`
	Line     int    `json:"line"`
	Column   int    `json:"column"`
	Severity string `json:"severity"`
	Provider string `json:"provider"`
	Rule     string `json:"rule"`
	Message  string `json:"message"`
	Pointer  string `json:"pointer,omitempty"`
}

func writeJSON(w io.Writer, reports []fileReport, want map[string]bool) {
	type fileOut struct {
		File     string            `json:"file"`
		Error    string            `json:"error,omitempty"`
		Schemas  []map[string]any  `json:"schemas"`
		Verdict  map[string]string `json:"verdict"`
		Findings []jsonFinding     `json:"findings"`
	}
	var out []fileOut
	for _, r := range reports {
		fo := fileOut{File: r.name, Error: r.readErr, Schemas: []map[string]any{}, Findings: []jsonFinding{}, Verdict: map[string]string{}}
		for _, t := range r.targets {
			fo.Schemas = append(fo.Schemas, map[string]any{"label": t.Label, "pointer": t.Ptr, "strict": t.Strict})
		}
		for _, f := range r.findings {
			prov := f.Target
			if prov == "" {
				prov = "all"
			}
			fo.Findings = append(fo.Findings, jsonFinding{r.name, f.Label, f.Line, f.Col, f.Sev.String(), prov, f.Rule, f.Msg, f.Where})
		}
		for _, p := range []string{OpenAI, Anthropic} {
			if !want[p] {
				continue
			}
			fo.Verdict[p] = "fits"
			if r.readErr != "" || countFor(r.findings, p, Error) > 0 {
				fo.Verdict[p] = "rejected"
			}
		}
		out = append(out, fo)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(map[string]any{"version": version, "files": out})
}

func countFor(fs []Finding, provider string, sev Severity) int {
	n := 0
	for _, f := range fs {
		if f.Sev == sev && (f.Target == "" || f.Target == provider) {
			n++
		}
	}
	return n
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func forceColor() bool {
	v := os.Getenv("FORCE_COLOR")
	return v != "" && v != "0" && v != "false"
}
