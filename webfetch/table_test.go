package webfetch

import (
	"strings"
	"testing"
)

// Tables need the explicit table plugin: htmltomarkdown.ConvertReader on its
// own flattens <table> children into a stream of paragraphs, which loses the
// column alignment that makes benchmark tables readable.
func TestTablesBecomeGFM(t *testing.T) {
	const page = `<html><body><p>before</p>
<table>
  <tr><th>Bench</th><th>Model A</th><th>Model B</th></tr>
  <tr><td>Terminal-Bench</td><td>57.9%</td><td>37.3%</td></tr>
  <tr><td>ExploitBench</td><td>100.0%</td><td>78.5%</td></tr>
</table>
<p>after</p></body></html>`

	out, err := convertHTMLToMarkdown([]byte(page), EmailDecode)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"| Bench | Model A | Model B |",
		"|---|---|---|",
		"| Terminal-Bench | 57.9% | 37.3% |",
		"| ExploitBench | 100.0% | 78.5% |",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in output:\n%s", want, out)
		}
	}
}

// Header-only tables (every cell a <th>, no <p> wrappers) used to collapse into
// a single glued line such as "CodingGPT-6 AstraGPT-5.6 SolTerminal-Bench 4.0".
func TestHeaderOnlyTableStaysTabular(t *testing.T) {
	const page = `<html><body><table>
  <tr><th>Coding</th><th>GPT-6 Astra</th><th>GPT-5.6 Sol</th></tr>
  <tr><th>DeepSWE v1.1</th><th>74.1%</th><th>72.7%</th></tr>
</table></body></html>`

	out, err := convertHTMLToMarkdown([]byte(page), EmailDecode)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "| Coding | GPT-6 Astra | GPT-5.6 Sol |") {
		t.Fatalf("header row not preserved:\n%s", out)
	}
	if !strings.Contains(out, "| DeepSWE v1.1 | 74.1% | 72.7% |") {
		t.Fatalf("body row not preserved:\n%s", out)
	}
	if strings.Contains(out, "CodingGPT") {
		t.Fatalf("cells are still being glued together:\n%s", out)
	}
}
