package report

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// SARIF writes SARIF 2.1.0, which GitHub code scanning, GitLab and most security
// dashboards read.
func SARIF(w io.Writer, r *Report) error {
	type msg struct {
		Text     string `json:"text"`
		Markdown string `json:"markdown,omitempty"`
	}
	type rule struct {
		ID               string         `json:"id"`
		Name             string         `json:"name"`
		ShortDescription msg            `json:"shortDescription"`
		FullDescription  msg            `json:"fullDescription"`
		HelpURI          string         `json:"helpUri"`
		Help             msg            `json:"help"`
		Properties       map[string]any `json:"properties"`
	}
	type region struct {
		StartLine int `json:"startLine"`
		EndLine   int `json:"endLine"`
	}
	type location struct {
		PhysicalLocation struct {
			ArtifactLocation struct {
				URI       string `json:"uri"`
				URIBaseID string `json:"uriBaseId"`
			} `json:"artifactLocation"`
			Region region `json:"region"`
		} `json:"physicalLocation"`
	}
	type result struct {
		RuleID              string            `json:"ruleId"`
		RuleIndex           int               `json:"ruleIndex"`
		Level               string            `json:"level"`
		Message             msg               `json:"message"`
		Locations           []location        `json:"locations"`
		PartialFingerprints map[string]string `json:"partialFingerprints"`
		Properties          map[string]any    `json:"properties"`
	}

	ruleIdx := map[string]int{}
	var rules []rule
	for i, ru := range r.Rules {
		ruleIdx[ru.Label] = i
		num := strings.TrimPrefix(ru.CWE, "CWE-")
		rules = append(rules, rule{
			ID: ru.CWE, Name: strings.ReplaceAll(ru.Title, " ", ""),
			ShortDescription: msg{Text: ru.Title},
			FullDescription:  msg{Text: ru.Question},
			HelpURI:          "https://cwe.mitre.org/data/definitions/" + num + ".html",
			Help:             msg{Text: ru.Question + "\n\n" + About},
			Properties:       map[string]any{"tags": []string{"security", "external/cwe/cwe-" + num}, "precision": "low"},
		})
	}
	results := []result{}
	for _, f := range r.Findings {
		level := "note"
		switch {
		case f.P >= 0.9:
			level = "error"
		case f.P >= 0.5:
			level = "warning"
		}
		text := fmt.Sprintf("%s (p=%.2f", f.Question, f.P)
		if d := delta(f); d != "" {
			text += ", " + d
		}
		text += fmt.Sprintf(") in window %s.", f.Location)
		var loc location
		loc.PhysicalLocation.ArtifactLocation.URI = f.Path
		loc.PhysicalLocation.ArtifactLocation.URIBaseID = "%SRCROOT%"
		from, to := span(f)
		loc.PhysicalLocation.Region = region{from, to}
		props := map[string]any{"p": round(f.P), "window": f.Location}
		if f.Delta != nil {
			props["delta"] = round(*f.Delta)
		}
		results = append(results, result{RuleID: f.CWE, RuleIndex: ruleIdx[f.Label], Level: level, Message: msg{Text: text},
			Locations: []location{loc}, PartialFingerprints: map[string]string{"hbbWindow/v1": f.Fingerprint}, Properties: props})
	}
	doc := map[string]any{
		"$schema": "https://json.schemastore.org/sarif-2.1.0.json",
		"version": "2.1.0",
		"runs": []any{map[string]any{
			"tool": map[string]any{"driver": map[string]any{
				"name": "hbb", "fullName": "Hot Buttered Beans", "version": r.Version,
				"informationUri": "https://github.com/xen0bit/hotbutteredbeans", "rules": rules,
			}},
			"originalUriBaseIds": map[string]any{"%SRCROOT%": map[string]string{"uri": "file://" + toURI(r.Root) + "/"}},
			"results":            results,
			"properties":         map[string]any{"model": r.Model, "device": r.Device, "windows": r.Windows},
		}},
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

func round(f float64) float64 { return float64(int(f*1000+0.5)) / 1000 }

func toURI(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}
