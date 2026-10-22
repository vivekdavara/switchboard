package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vivekdavara/switchboard/internal/load"
)

// scripts/load.sh keeps loadgen's -json line for every run, and scripts/load_table.py turns them into README's load
// table, so each field must be the summary's own: the day-5 review swapped p99.9 for p99, the max for p99.9, and the
// throughput for requests over elapsed (counting transport errors), and no test noticed. Every value here differs.
var summary = load.Summary{Requests: 7, PerSecond: 1234.4, Statuses: map[int]int{200: 6, 502: 1},
	Errors: map[string]int{"EOF": 1}, P50: time.Millisecond, P90: 2 * time.Millisecond, P99: 3 * time.Millisecond,
	P999: 4 * time.Millisecond, Max: 5 * time.Millisecond, FirstByte: 500 * time.Microsecond, Successful: 6}

func TestTheJSONSummaryIsTheSummaryFieldForField(t *testing.T) {
	b, err := json.Marshal(jsonSummary(summary, 2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"requests": 7.0, "elapsed_s": 2.0, "per_second": 1234.4,
		"statuses": map[string]any{"200": 6.0, "502": 1.0}, "errors": map[string]any{"EOF": 1.0},
		"p50_us": 1000.0, "p90_us": 2000.0, "p99_us": 3000.0, "p999_us": 4000.0, "max_us": 5000.0,
		"first_byte_p50_us": 500.0}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %v\nwant %v", got, want)
	}
}

// load_table.py reads the line as load.sh writes it (with the case and the run added) into the README's columns.
func TestTheLoadTableReadsTheJSONLineIntoItsColumns(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3")
	}
	line := jsonSummary(summary, 2*time.Second)
	line["case"], line["run"] = "gateway-c1", 1
	b, err := json.Marshal(line)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "results.jsonl")
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(python, "../../scripts/load_table.py", path).CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	row := "| gateway-c1 | 1 | 1,234 (1,234 to 1,234) | 1.000 ms | 2.000 ms | 3.000 ms | 4.000 ms | 5.000 ms | 1 |"
	if !strings.Contains(string(out), row+"\n") {
		t.Errorf("no row\n%s\nin\n%s", row, out)
	}
}
