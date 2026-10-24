package claudecode

import (
	"testing"

	"github.com/kjhnns/agentd/internal/eventbus"
)

// TestDeduperKeepsTheAuthoritativeAnswer (R2 review round 4): the display
// text of a result that repeats the last output is blanked, but the full
// answer stays on the event, so a dropped output event cannot cost the
// turn its answer.
func TestDeduperKeepsTheAuthoritativeAnswer(t *testing.T) {
	d := &resultDeduper{}
	evs := d.Apply([]eventbus.Event{
		{Kind: eventbus.KindOutput, Text: "the answer"},
		{Kind: eventbus.KindResult, Text: "the answer"},
	})
	if evs[1].Text != "" || evs[1].Answer != "the answer" {
		t.Fatalf("result event = %+v", evs[1])
	}
}
