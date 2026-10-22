package gateway

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// The docs' examples and tables, checked against the code (day 5): v1.0.0 promises its metric names and labels and
// its error codes (CHANGELOG.md), so the pages that list them must say what the code does.

func readDoc(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("../../" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// fencedAfter returns the first fenced block of the given language after the first line containing marker.
func fencedAfter(t *testing.T, doc, marker, lang string) string {
	t.Helper()
	_, rest, ok := strings.Cut(doc, marker)
	if !ok {
		t.Fatalf("no %q in the doc", marker)
	}
	_, rest, ok = strings.Cut(rest, "\n```"+lang+"\n")
	if !ok {
		t.Fatalf("no ```%s block after %q", lang, marker)
	}
	block, _, ok := strings.Cut(rest, "\n```")
	if !ok {
		t.Fatalf("an unclosed block after %q", marker)
	}
	return block
}

var descString = regexp.MustCompile(`^Desc\{fqName: "([a-z_]+)", .*variableLabels: \{([^}]*)\}\}$`)

func TestDesignsMetricsTableIsTheGatewaysMetrics(t *testing.T) {
	documented := map[string][]string{}
	row := regexp.MustCompile("(?m)^\\| `(switchboard_[a-z_]+)` \\|([^|]*)\\|")
	for _, m := range row.FindAllStringSubmatch(readDoc(t, "DESIGN.md"), -1) {
		var labels []string
		for l := range strings.SplitSeq(m[2], ",") {
			if l = strings.TrimSpace(l); l != "" {
				labels = append(labels, l)
			}
		}
		documented[m[1]] = labels
	}

	exported := map[string][]string{}
	descs := make(chan *prometheus.Desc)
	go func() {
		newMetrics(nil).reg.Describe(descs)
		close(descs)
	}()
	for d := range descs {
		m := descString.FindStringSubmatch(d.String())
		if m == nil {
			t.Fatalf("can't read %s", d)
		}
		if !strings.HasPrefix(m[1], "switchboard_") {
			continue // Go's runtime and the process's own (go_*, process_*), which DESIGN.md names together
		}
		var labels []string
		if m[2] != "" {
			labels = strings.Split(m[2], ",")
		}
		exported[m[1]] = labels
	}
	if len(exported) < 18 {
		t.Fatalf("only %d of the gateway's metrics read: %v", len(exported), exported)
	}
	for name, labels := range exported {
		doc, ok := documented[name]
		if !ok {
			t.Errorf("%s (labels %v) isn't in DESIGN.md's metrics table", name, labels)
		} else if !slices.Equal(doc, labels) {
			t.Errorf("%s: DESIGN.md says labels %v, the code has %v", name, doc, labels)
		}
	}
	for name := range documented {
		if _, ok := exported[name]; !ok {
			t.Errorf("DESIGN.md's metrics table has %s, which the gateway doesn't export", name)
		}
	}
}

func TestTheReadmesConfigurationExampleIsConfigsLocalJSON(t *testing.T) {
	block := fencedAfter(t, readDoc(t, "README.md"), "`configs/local.json` routes", "json")
	file, err := os.ReadFile("../../configs/local.json")
	if err != nil {
		t.Fatal(err)
	}
	if block != strings.TrimRight(string(file), "\n") {
		t.Errorf("README's example:\n%s\nconfigs/local.json:\n%s", block, file)
	}
}

func TestTheReadmesErrorExampleIsWhatTheGatewayAnswers(t *testing.T) {
	block := fencedAfter(t, readDoc(t, "README.md"), "Every error from the API's endpoints", "json")
	p := newProvider(t, answering(200, `{}`))
	srv, _, _ := gatewayFor(t, "", []string{at(p, "")}, "fast=p0/up")
	resp, body := send(t, srv, `{"model":"gpt-9","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 404 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	var doc, got any
	if err := json.Unmarshal([]byte(block), &doc); err != nil {
		t.Fatalf("README's example isn't JSON: %v\n%s", err, block)
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(doc, got) {
		var compact bytes.Buffer
		json.Compact(&compact, []byte(block))
		t.Errorf("README says\n%s\nthe gateway answers\n%s", compact.String(), body)
	}
}

// The docs send a reader to tests and benchmarks by name (WALKTHROUGH's reading orders above all); a rename would
// leave them pointing at nothing. A name in a -fuzz, -bench or -run pattern may be a prefix of the function's.
func TestEveryTestTheDocsNameExists(t *testing.T) {
	declared := regexp.MustCompile(`(?m)^func ((?:Test|Fuzz|Benchmark)[A-Za-z0-9_]+)\(`)
	var funcs []string
	for _, dir := range []string{"../../cmd", "../../internal"} {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, "_test.go") {
				return err
			}
			b, err := os.ReadFile(path)
			for _, m := range declared.FindAllStringSubmatch(string(b), -1) {
				funcs = append(funcs, m[1])
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(funcs) < 250 {
		t.Fatalf("only %d test functions found", len(funcs))
	}
	named := regexp.MustCompile(`(-(?:fuzz|bench|run) '?\^?)?\b((?:Test|Fuzz|Benchmark)[A-Z][A-Za-z0-9_]+)`)
	for _, doc := range []string{"README.md", "DESIGN.md", "WALKTHROUGH.md", "CHANGELOG.md"} {
		for _, m := range named.FindAllStringSubmatch(readDoc(t, doc), -1) {
			pattern, name := m[1] != "", m[2]
			if !slices.ContainsFunc(funcs, func(f string) bool { return f == name || pattern && strings.HasPrefix(f, name) }) {
				t.Errorf("%s names %s, which no test file declares", doc, name)
			}
		}
	}
}
