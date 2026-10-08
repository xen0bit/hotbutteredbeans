package report

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/xen0bit/hotbutteredbeans/internal/scan"
)

// --- text ---

const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
	ansiYellow = "\x1b[33m"
	ansiCyan   = "\x1b[36m"
)

// Text is for a terminal: windows in rank order of their best finding, each with its
// findings (p, the rise from the base with --compare, CWE, title), the question of the
// top one, and the window's code once.
func Text(w io.Writer, r *Report) error {
	c := func(code, s string) string {
		if !r.Color {
			return s
		}
		return code + s + ansiReset
	}
	if len(r.Findings) == 0 {
		_, err := fmt.Fprintf(w, "No findings at p >= %.2f in %d windows of %d files.\n", r.MinP, r.Windows, r.Files)
		return err
	}
	for _, win := range byWindow(r.Findings) {
		f := win.first
		head := c(ansiCyan+ansiBold, f.Location)
		if len(f.Changed) > 0 {
			head += c(ansiDim, "  changed "+ranges(f.Changed))
		}
		fmt.Fprintln(w, head)
		for _, x := range win.findings {
			pc := ansiDim
			switch {
			case x.P >= 0.8:
				pc = ansiRed + ansiBold
			case x.P >= 0.5:
				pc = ansiYellow
			}
			d := ""
			if x.Delta != nil {
				d = fmt.Sprintf("%+.2f ", *x.Delta)
				if x.BaseP == nil {
					d = "  new "
				}
				d = c(ansiDim, d)
			}
			fmt.Fprintf(w, "  %s %s%-8s %s\n", c(pc, fmt.Sprintf("%.3f", x.P)), d, x.CWE, x.Title)
		}
		fmt.Fprintf(w, "  %s\n", c(ansiDim, f.CWE+": "+f.Question))
		if r.ContentLines != 0 {
			body, more := numbered(f, r.ContentLines, true)
			for _, l := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
				if strings.HasPrefix(l, ">") {
					fmt.Fprintf(w, "    %s\n", c(ansiBold, l))
				} else {
					fmt.Fprintf(w, "    %s\n", l)
				}
			}
			if more > 0 {
				fmt.Fprintf(w, "    %s\n", c(ansiDim, fmt.Sprintf("  ... %d more lines in the window", more)))
			}
		}
		fmt.Fprintln(w)
	}
	return nil
}

// --- json, jsonl ---

// JSON is the whole report as one document.
func JSON(w io.Writer, r *Report) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if r.Findings == nil {
		r.Findings = []scan.Finding{} // [] rather than null
	}
	return enc.Encode(r)
}

// JSONL is one finding per line.
func JSONL(w io.Writer, r *Report) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	for _, f := range r.Findings {
		if err := enc.Encode(f); err != nil {
			return err
		}
	}
	return nil
}

// --- github ---

// GitHub writes workflow commands, which GitHub Actions shows as annotations on the
// changed lines of a pull request.
func GitHub(w io.Writer, r *Report) error {
	esc := strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A")
	prop := strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A", ":", "%3A", ",", "%2C")
	for _, f := range r.Findings {
		level := "notice"
		if f.P >= 0.5 {
			level = "warning"
		}
		from, to := span(f)
		title := fmt.Sprintf("%s %s (p=%.2f", f.CWE, f.Title, f.P)
		if d := delta(f); d != "" {
			title += ", " + d
		}
		title += ")"
		fmt.Fprintf(w, "::%s file=%s,line=%d,endLine=%d,title=%s::%s\n", level, prop.Replace(f.Path), from, to,
			prop.Replace(title), esc.Replace(f.Question+"\nWindow "+f.Location+". hbb ranks code to read; it does not prove a flaw."))
	}
	return nil
}

// --- markdown ---

// Markdown is for a pull request comment or a job summary.
func Markdown(w io.Writer, r *Report) error {
	fmt.Fprintf(w, "## hbb: %d finding%s\n\n", len(r.Findings), plural(len(r.Findings)))
	scope := fmt.Sprintf("%d windows of %d files", r.Windows, r.Files)
	if r.Mode == "diff" {
		scope = fmt.Sprintf("the %d windows a change touched, in %d files (%s → %s)", r.Windows, r.Files, r.Base, r.Head)
	}
	fmt.Fprintf(w, "Scanned %s with `%s` on %s. Findings at p ≥ %.2f.\n\n", scope, r.Model, r.Device, r.MinP)
	if len(r.Findings) == 0 {
		return nil
	}
	if r.Compare {
		fmt.Fprintln(w, "| p | change | CWE | location | question |\n|---:|---:|---|---|---|")
	} else {
		fmt.Fprintln(w, "| p | CWE | location | question |\n|---:|---|---|---|")
	}
	cell := strings.NewReplacer("|", "\\|", "\n", " ")
	for _, f := range r.Findings {
		loc := "`" + f.Location + "`"
		if r.Compare {
			fmt.Fprintf(w, "| %.2f | %s | %s %s | %s | %s |\n", f.P, delta(f), f.CWE, cell.Replace(f.Title), loc, cell.Replace(f.Question))
		} else {
			fmt.Fprintf(w, "| %.2f | %s %s | %s | %s |\n", f.P, f.CWE, cell.Replace(f.Title), loc, cell.Replace(f.Question))
		}
	}
	fmt.Fprintln(w)
	for _, win := range byWindow(r.Findings) {
		f := win.first
		var cwes []string
		for _, x := range win.findings {
			cwes = append(cwes, fmt.Sprintf("%s %.2f", x.CWE, x.P))
		}
		if r.ContentLines == 0 {
			fmt.Fprintf(w, "- `%s`: %s\n", f.Location, strings.Join(cwes, ", "))
			continue
		}
		body, more := numbered(f, r.ContentLines, true)
		fmt.Fprintf(w, "<details><summary><code>%s</code>: %s</summary>\n\n```%s\n%s```\n", f.Location, strings.Join(cwes, ", "), fence(f.Lang), body)
		if more > 0 {
			fmt.Fprintf(w, "\n%d more lines in the window.\n", more)
		}
		fmt.Fprint(w, "\n</details>\n\n")
	}
	fmt.Fprintf(w, "<sub>hbb ranks code to read first; a score is not a verdict. %s</sub>\n", r.Version)
	return nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func fence(lang string) string {
	switch lang {
	case "c_sharp":
		return "csharp"
	case "tsx":
		return "tsx"
	}
	return lang
}

// --- llm ---

// LLM is context for a language model reviewing the change: what the numbers mean,
// then each flagged window once, with its questions and scores, its code numbered and
// its changed lines marked. Windows are added in rank order until MaxChars of code.
func LLM(w io.Writer, r *Report) error {
	attr := func(k, v string) string {
		return fmt.Sprintf(` %s="%s"`, k, strings.NewReplacer(`"`, "&quot;", "<", "&lt;", ">", "&gt;", "&", "&amp;").Replace(v))
	}
	fmt.Fprintf(w, "<hbb_report%s%s%s", attr("mode", r.Mode), attr("model", r.Model), attr("windows_scanned", fmt.Sprint(r.Windows)))
	if r.Mode == "diff" {
		fmt.Fprintf(w, "%s%s", attr("base", r.Base), attr("head", r.Head))
	}
	fmt.Fprintf(w, "%s>\n<about>\n%s\nLines marked \">\" changed. Read each window and judge for yourself whether the\nflagged weakness is real, citing lines; treat the scores only as where to look first.\n</about>\n",
		attr("min_p", fmt.Sprintf("%.2f", r.MinP)), About)
	budget := r.MaxChars
	omitted := 0
	for i, win := range byWindow(r.Findings) {
		f := win.first
		body, _ := numbered(f, 0, len(f.Changed) > 0)
		if budget > 0 && i > 0 && len(body) > budget {
			omitted++
			continue
		}
		budget -= len(body)
		fmt.Fprintf(w, "<window%s%s%s", attr("path", f.Path), attr("lines", fmt.Sprintf("%d-%d", f.From, f.To)), attr("lang", f.Lang))
		if len(f.Changed) > 0 {
			fmt.Fprint(w, attr("changed", ranges(f.Changed)))
		}
		fmt.Fprintln(w, ">")
		for _, x := range win.findings {
			fmt.Fprintf(w, "<finding%s%s%s", attr("cwe", x.CWE), attr("title", x.Title), attr("p", fmt.Sprintf("%.2f", x.P)))
			if x.Delta != nil {
				if x.BaseP != nil {
					fmt.Fprint(w, attr("p_before_change", fmt.Sprintf("%.2f", *x.BaseP)))
				} else {
					fmt.Fprint(w, attr("p_before_change", "new file"))
				}
			}
			fmt.Fprintf(w, ">%s</finding>\n", x.Question)
		}
		fmt.Fprintf(w, "<code>\n%s</code>\n</window>\n", body)
	}
	if omitted > 0 {
		fmt.Fprintf(w, "<omitted windows=\"%d\">lower-ranked windows left out to fit the budget</omitted>\n", omitted)
	}
	_, err := fmt.Fprintln(w, "</hbb_report>")
	return err
}
