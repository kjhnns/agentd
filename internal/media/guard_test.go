package media

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestUnreliable(t *testing.T) {
	cases := []struct {
		name, text, lang string
		dur              int
		bad              bool
	}{
		{"OPS-81 note", "GERRARD STREET GERRARD STREET COVENTRY STREET COVENTRY STREET", "nynorsk", 120, true},
		{"same text no language", "GERRARD STREET GERRARD STREET COVENTRY STREET COVENTRY STREET", "", 120, true},
		{"repetitive slow clip", "street street street coventry street street", "", 30, true},
		{"repetitive fast command", "Call Mom, call Dad, call Mom, call Dad", "english", 4, false},
		{"talks about captions", "Remove the thanks for watching line from the end of the video please", "english", 5, false},
		{"fluent french", "Bonjour, je voudrais réserver une table pour deux personnes ce soir", "french", 5, false},
		{"exotic and slow", "hei hei hei", "nynorsk", 30, true},
		{"caption credit", "Thanks for watching!", "english", 3, true},
		{"empty long", "", "", 30, true},
		{"loop", strings.Repeat("and then we went to the shop ", 6), "english", 15, true},
		{"normal english", "Remind me tomorrow at nine to call the dentist about the cleaning", "english", 5, false},
		{"normal german", "Kannst du bitte schauen ob das Paket schon angekommen ist", "german", 4, false},
		{"normal portuguese", "Quero saber se a farmácia está aberta hoje à noite", "portuguese", 4, false},
		{"short yes", "yes", "english", 1, false},
		{"long real note", "I was thinking about the trip to Rio next month and whether we should take the train or the bus because the flight is expensive and Cami wants to see the coast on the way so maybe we stop in Paraty for two nights and then continue", "english", 25, false},
		{"unknown duration short", "ok send it", "", 0, false},
	}
	for _, c := range cases {
		got := Unreliable(c.text, c.lang, c.dur)
		if (got != "") != c.bad {
			t.Errorf("%s: Unreliable = %q, want bad=%v", c.name, got, c.bad)
		}
	}
}

type fixedTranscriber struct{ tr Transcript }

func (f fixedTranscriber) Transcribe(context.Context, string) (Transcript, error) { return f.tr, nil }

func TestIngestKeepsAudio(t *testing.T) {
	cases := []struct {
		name     string
		tr       Transcript
		keep     bool
		wantKept bool
		wantBad  bool
	}{
		{"good, not kept", Transcript{Text: "call the dentist tomorrow", Language: "english", DurationS: 2}, false, false, false},
		{"good, KeepAudio", Transcript{Text: "call the dentist tomorrow", Language: "english", DurationS: 2}, true, true, false},
		{"unreliable always kept", Transcript{Text: "GERRARD STREET GERRARD STREET", Language: "nynorsk", DurationS: 120}, false, true, true},
	}
	for _, c := range cases {
		s := &Service{Dir: t.TempDir(), Transcriber: fixedTranscriber{c.tr}}
		art, err := s.Ingest(context.Background(), Request{
			Reader: strings.NewReader("AUDIO"), Filename: "note.m4a", Mime: "audio/mp4",
			Source: "watch", KeepAudio: c.keep,
		})
		if err != nil {
			t.Fatalf("%s: Ingest: %v", c.name, err)
		}
		if (art.Path != "") != c.wantKept {
			t.Errorf("%s: Path = %q, want kept=%v", c.name, art.Path, c.wantKept)
		}
		if art.Path != "" {
			if _, err := os.Stat(art.Path); err != nil {
				t.Errorf("%s: kept file missing: %v", c.name, err)
			}
		}
		if (art.Unreliable != "") != c.wantBad {
			t.Errorf("%s: Unreliable = %q, want bad=%v", c.name, art.Unreliable, c.wantBad)
		}
	}
}
