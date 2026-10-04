package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// section is one table of a table output, and a verb such as policy show prints more than one.
type section struct {
	columns []string
	rows    [][]string
}

// cell keeps a value on its row: a tab or a newline inside it would split the table.
var cell = strings.NewReplacer("\t", " ", "\n", " ")

// writeSections prints the sections with a blank line between them, each aligned on its own.
func writeSections(w io.Writer, sections ...section) error {
	for i, s := range sections {
		if i > 0 {
			if _, err := fmt.Fprintln(w); err != nil {
				return fmt.Errorf("write the output: %w", err)
			}
		}

		tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
		fmt.Fprintln(tw, strings.Join(s.columns, "\t"))
		for _, row := range s.rows {
			values := make([]string, len(row))
			for j, value := range row {
				values[j] = cell.Replace(value)
			}
			fmt.Fprintln(tw, strings.Join(values, "\t"))
		}

		if err := tw.Flush(); err != nil {
			return fmt.Errorf("write the output: %w", err)
		}
	}

	return nil
}

// writeJSON encodes the whole value before it writes, so a failure leaves stdout empty.
func writeJSON(w io.Writer, v any) error {
	blob, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the output: %w", err)
	}

	if _, err := fmt.Fprintln(w, string(blob)); err != nil {
		return fmt.Errorf("write the output: %w", err)
	}

	return nil
}

// fieldSection is the FIELD VALUE table of a record, one row per JSON leaf, in the order the record declares them.
func fieldSection(v any) (section, error) {
	blob, err := json.Marshal(v)
	if err != nil {
		return section{}, fmt.Errorf("encode the output: %w", err)
	}

	dec := json.NewDecoder(bytes.NewReader(blob))
	dec.UseNumber()

	s := section{columns: []string{"FIELD", "VALUE"}}
	if err := walkFields(dec, "", &s.rows); err != nil {
		return section{}, fmt.Errorf("read the encoded output: %w", err)
	}

	return s, nil
}

// walkFields reads tokens rather than a map, because a map would lose the record's field order.
func walkFields(dec *json.Decoder, path string, rows *[][]string) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}

	delim, isDelim := tok.(json.Delim)
	if !isDelim {
		*rows = append(*rows, []string{path, leafText(tok)})
		return nil
	}

	count := 0
	for ; dec.More(); count++ {
		child := fmt.Sprintf("%s[%d]", path, count)
		if delim == '{' {
			key, err := dec.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok {
				return errors.New("an object key is not a string")
			}
			child = joinField(path, name)
		}

		if err := walkFields(dec, child, rows); err != nil {
			return err
		}
	}

	if count == 0 && path != "" {
		*rows = append(*rows, []string{path, "-"})
	}

	// The closing delimiter.
	_, err = dec.Token()
	return err
}

func joinField(path, name string) string {
	if path == "" {
		return name
	}

	return path + "." + name
}

// leafText spells a JSON leaf for a table, with a dash for null as the other tables print an empty value.
func leafText(tok json.Token) string {
	if tok == nil {
		return "-"
	}
	if s, ok := tok.(string); ok {
		return orDash(s)
	}

	return fmt.Sprint(tok)
}
