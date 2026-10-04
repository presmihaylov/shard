package cli

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// The rows follow the record's declared order and spell every leaf, so a table is the JSON read down a page.
func TestFieldSectionIsOneRowPerLeafInDeclaredOrder(t *testing.T) {
	type inner struct {
		MemoryMiB int64 `json:"memory_mib"`
		Note      string
	}
	record := struct {
		ID      string   `json:"id"`
		Command []string `json:"command"`
		Ports   []int    `json:"ports"`
		Inner   inner    `json:"resources"`
		Exit    *int     `json:"exit"`
		Ready   bool     `json:"ready"`
	}{ID: "s-1", Command: []string{"sh", "-c"}, Ports: []int{}, Inner: inner{MemoryMiB: 512, Note: "a\tb\nc"}}

	s, err := fieldSection(record)
	if err != nil {
		t.Fatalf("fieldSection: %v", err)
	}

	want := [][]string{
		{"id", "s-1"},
		{"command[0]", "sh"},
		{"command[1]", "-c"},
		{"ports", "-"},
		{"resources.memory_mib", "512"},
		{"resources.Note", "a\tb\nc"},
		{"exit", "-"},
		{"ready", "false"},
	}
	if len(s.rows) != len(want) {
		t.Fatalf("got rows %q, want %q", s.rows, want)
	}
	for i := range want {
		if strings.Join(s.rows[i], "=") != strings.Join(want[i], "=") {
			t.Errorf("row %d is %q, want %q", i, s.rows[i], want[i])
		}
	}
}

// A value with a tab or a newline stays on its own row, under its own column.
func TestWriteSectionsKeepsAValueOnItsRow(t *testing.T) {
	var out bytes.Buffer
	err := writeSections(&out,
		section{columns: []string{"FIELD", "VALUE"}, rows: [][]string{{"reason", "a\tb\nc"}}},
		section{columns: []string{"RULE"}, rows: [][]string{{"allow example.com"}}},
	)
	if err != nil {
		t.Fatalf("writeSections: %v", err)
	}

	want := "FIELD    VALUE\nreason   a b c\n\nRULE\nallow example.com\n"
	if out.String() != want {
		t.Errorf("got\n%q\nwant\n%q", out.String(), want)
	}
}

func TestWriteJSONIsOneIndentedValue(t *testing.T) {
	var out bytes.Buffer
	if err := writeJSON(&out, []map[string]int{{"rule_count": 2}}); err != nil {
		t.Fatalf("writeJSON: %v", err)
	}

	if out.String() != "[\n  {\n    \"rule_count\": 2\n  }\n]\n" {
		t.Errorf("got %q", out.String())
	}
	var back []map[string]int
	if err := json.Unmarshal(out.Bytes(), &back); err != nil {
		t.Errorf("the output is not one JSON value: %v", err)
	}
}

// An encode failure is found before the first byte, so stdout stays empty.
func TestWriteJSONWritesNothingWhenTheEncodeFails(t *testing.T) {
	var out bytes.Buffer
	if err := writeJSON(&out, map[string]float64{"bad": math.Inf(1)}); err == nil {
		t.Fatal("writeJSON encoded an infinity")
	}

	if out.Len() != 0 {
		t.Errorf("a failed encode wrote %q", out.String())
	}
}
