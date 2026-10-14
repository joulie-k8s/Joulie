package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// readJournal decodes every line of the events journal in dir.
func readJournal(t *testing.T, dir string) []simEvent {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, eventJournalFile))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []simEvent
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e simEvent
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("journal line %d %q: %v", len(out)+1, sc.Text(), err)
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// The events journal stays open for the simulator's life and takes each
// tick's events with one write: after a clean stop it holds every event,
// tick batches and single events alike, complete and in the order they
// were recorded; an event recorded after the stop is not written; and a
// restart on the same directory appends to it.
func TestEventJournalKeepsEveryEventInOrderAcrossAStop(t *testing.T) {
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	dir := t.TempDir()
	s := newSimulatorWithRegisterer(defaultBaseModelFromEnv(), nil, nil, 10, prometheus.NewRegistry())
	s.configurePersistence(dir)
	var want []string
	for b := 0; b < 50; b++ {
		evs := make([]simEvent, 20)
		for i := range evs {
			node := fmt.Sprintf("tick-%02d-%02d", b, i)
			evs[i] = newSimEvent("hw", node, map[string]any{"i": i})
			want = append(want, node)
		}
		s.recordEvents(evs)
		node := fmt.Sprintf("control-%02d", b)
		s.recordEvent("control", node, nil)
		want = append(want, node)
	}
	s.closePersistence()
	s.recordEvent("control", "after-stop", nil)

	got := readJournal(t, dir)
	if len(got) != len(want) {
		t.Fatalf("journal holds %d events, want %d", len(got), len(want))
	}
	for i, e := range got {
		if e.Node != want[i] {
			t.Fatalf("journal event %d is %s, want %s: events out of order", i, e.Node, want[i])
		}
	}
	if len(s.events) != 10 || s.events[9].Node != "after-stop" {
		t.Errorf("/debug/events keeps %d events ending %v, want the last 10 ending with after-stop", len(s.events), s.events[len(s.events)-1])
	}

	r := newSimulatorWithRegisterer(defaultBaseModelFromEnv(), nil, nil, 10, prometheus.NewRegistry())
	r.configurePersistence(dir)
	r.recordEvent("control", "restarted", nil)
	r.closePersistence()
	if got := readJournal(t, dir); len(got) != len(want)+1 || got[len(got)-1].Node != "restarted" {
		t.Errorf("after a restart the journal holds %d events, want %d ending with restarted", len(got), len(want)+1)
	}
}

// One tick of 100 events into the journal: with the file kept open and one
// write per tick, and, for comparison, opened, written and closed once per
// event. The events are made beforehand, so neither counts their clock
// reads.
func BenchmarkEventJournalTick(b *testing.B) {
	log.SetOutput(io.Discard)
	b.Cleanup(func() { log.SetOutput(os.Stderr) })
	s := newSimulatorWithRegisterer(defaultBaseModelFromEnv(), nil, nil, 300, prometheus.NewRegistry())
	s.configurePersistence(b.TempDir())
	b.Cleanup(s.closePersistence)
	evs := benchJournalEvents()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.persistEventJournal(evs)
	}
}

func BenchmarkEventJournalTickOpenPerEvent(b *testing.B) {
	dir := b.TempDir()
	evs := benchJournalEvents()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, e := range evs {
			line, err := json.Marshal(e)
			if err != nil {
				b.Fatal(err)
			}
			f, err := os.OpenFile(filepath.Join(dir, eventJournalFile), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				b.Fatal(err)
			}
			_, _ = f.Write(append(line, '\n'))
			_ = f.Close()
		}
	}
}

func benchJournalEvents() []simEvent {
	payload := map[string]any{"hw.id": "cpu0", "hw.type": "cpu", "joulie.hw.limit": 250.0}
	evs := make([]simEvent, 100)
	for k := range evs {
		evs[k] = newSimEvent("hw.limit.changed", "node-1", payload)
	}
	return evs
}
