package main

import (
	"fmt"
	"io"
	"strings"
)

type printer struct {
	w     io.Writer
	color bool
	quiet bool
	want  map[string]bool
}

func (p *printer) c(code, s string) string {
	if !p.color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func plural(n int, s string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, s)
	}
	return fmt.Sprintf("%d %ss", n, s)
}

func (p *printer) file(r fileReport) {
	if r.readErr != "" {
		fmt.Fprintf(p.w, "%s %s: %s\n\n", p.c("31;1", "error"), r.name, r.readErr)
		return
	}
	fmt.Fprintf(p.w, "%s\n", p.c("1", r.name))
	shown := r.findings
	if p.quiet {
		shown = nil
		for _, f := range r.findings {
			if f.Sev == Error {
				shown = append(shown, f)
			}
		}
	}
	label := "\x00"
	for _, f := range shown {
		if f.Label != label {
			label = f.Label
			if label != "" {
				fmt.Fprintf(p.w, "  %s\n", p.c("36", label))
			}
		}
		pos := fmt.Sprintf("%d:%d", f.Line, f.Col)
		sev := f.Sev.String()
		switch f.Sev {
		case Error:
			sev = p.c("31", fmt.Sprintf("%-7s", sev))
		case Warning:
			sev = p.c("33", fmt.Sprintf("%-7s", sev))
		default:
			sev = p.c("34", fmt.Sprintf("%-7s", sev))
		}
		prov := f.Target
		if prov == "" {
			prov = "all"
		}
		provc := map[string]string{OpenAI: "32", Anthropic: "35", "all": "37"}[prov]
		fmt.Fprintf(p.w, "  %8s  %s  %s  %s  %s\n", pos, sev, p.c(provc, fmt.Sprintf("%-9s", prov)), f.Msg, p.c("2", f.Rule))
	}
	// verdict per provider
	var parts []string
	for _, prov := range []string{OpenAI, Anthropic} {
		if !p.want[prov] {
			continue
		}
		e, w := countFor(r.findings, prov, Error), countFor(r.findings, prov, Warning)
		name := map[string]string{OpenAI: "OpenAI strict", Anthropic: "Claude strict"}[prov]
		if e > 0 {
			parts = append(parts, p.c("31;1", "✗ "+name)+p.c("31", fmt.Sprintf(": rejected, %s", plural(e, "error"))))
		} else if w > 0 {
			parts = append(parts, p.c("33;1", "✓ "+name)+p.c("33", fmt.Sprintf(": fits, %s", plural(w, "warning"))))
		} else {
			parts = append(parts, p.c("32;1", "✓ "+name)+p.c("32", ": fits"))
		}
	}
	n := len(r.targets)
	what := plural(n, "schema")
	fmt.Fprintf(p.w, "  %s   %s\n\n", p.c("2", what+":"), strings.Join(parts, "   "))
}

func (p *printer) summary(rs []fileReport) {
	e, w, n, s := 0, 0, 0, 0
	for _, r := range rs {
		e += r.errors
		w += r.warnings
		n += r.notes
		s += len(r.targets)
	}
	line := fmt.Sprintf("%s, %s, %s in %s", plural(e, "error"), plural(w, "warning"), plural(n, "note"), plural(s, "schema"))
	if e > 0 {
		line = p.c("31;1", line)
	} else {
		line = p.c("32;1", line)
	}
	fmt.Fprintln(p.w, line)
}
