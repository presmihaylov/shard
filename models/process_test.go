package models

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func TestTheFullestProcessTableFitsTheSealedChannel(t *testing.T) {
	table := ProcessTable{Kind: ProcessTableKind}
	for range MaxProcesses {
		table.Processes = append(table.Processes, ProcessReport{
			Name: strings.Repeat("a", MaxProcessName),
			ProcessStatus: ProcessStatus{
				State:     ProcessRestarting,
				Restarts:  math.MaxInt,
				Exit:      &ExitStatus{Code: math.MaxInt32, Signal: math.MaxInt32},
				StartedAt: time.Date(2026, 10, 7, 17, 51, 0, 123456789, time.FixedZone("", -12*3600)),
			},
			Seq: math.MaxUint64,
		})
	}
	encoded, err := json.Marshal(table)
	if err != nil {
		t.Fatal(err)
	}
	// shard-init frames the record with a newline on each side.
	if size := len(encoded) + 2; size > ExitChannelSize {
		t.Fatalf("the fullest table is %d bytes, over the %d the sealed channel holds", size, ExitChannelSize)
	}
}

func TestValidProcessNameTakesAFileSafeName(t *testing.T) {
	for _, name := range []string{"api", "db", "pg_ctl.16-main", strings.Repeat("a", MaxProcessName)} {
		if !ValidProcessName(name) {
			t.Errorf("ValidProcessName(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"", "Api", ".hidden", "-x", "a/b", "..", strings.Repeat("a", MaxProcessName+1)} {
		if ValidProcessName(name) {
			t.Errorf("ValidProcessName(%q) = true, want false", name)
		}
	}
}
