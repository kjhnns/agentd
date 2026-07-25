package session

import (
	"strings"
	"testing"

	"github.com/kjhnns/agentd/internal/channel"
	"github.com/kjhnns/agentd/internal/media"
)

func TestRenderInboundTextOnly(t *testing.T) {
	got := RenderInbound(channel.InboundMsg{Text: "  hello  "})
	if got != "hello" {
		t.Errorf("got %q", got)
	}
}

func TestRenderInboundImageMarkerExactlyOnce(t *testing.T) {
	in := channel.InboundMsg{Media: []media.Artifact{{
		Kind: media.KindImage, Path: "/tmp/media/telegram/x-photo.jpg",
		Mime: "image/jpeg", Name: "photo.jpg", Size: 123,
	}}}
	got := RenderInbound(in)
	want := "[The user sent an image saved at /tmp/media/telegram/x-photo.jpg. Use the Read tool to view it, then respond.]"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
	if strings.Count(got, "[The user sent an image") != 1 {
		t.Errorf("marker not exactly once: %q", got)
	}
}

func TestRenderInboundMixedTextAndImage(t *testing.T) {
	in := channel.InboundMsg{
		Text: "what is this?",
		Media: []media.Artifact{{
			Kind: media.KindImage, Path: "/p/img.jpg", Mime: "image/jpeg", Name: "img.jpg",
		}},
	}
	got := RenderInbound(in)
	if !strings.HasPrefix(got, "what is this?\n\n") {
		t.Errorf("caption should lead: %q", got)
	}
	if !strings.Contains(got, "saved at /p/img.jpg") {
		t.Errorf("missing path: %q", got)
	}
	if strings.Count(got, "[The user sent an image") != 1 {
		t.Errorf("marker count wrong: %q", got)
	}
}

func TestRenderInboundAudioTranscript(t *testing.T) {
	in := channel.InboundMsg{Media: []media.Artifact{{
		Kind: media.KindAudio, DurationS: 7, Transcript: "book the dentist for tuesday",
	}}}
	got := RenderInbound(in)
	want := "[voice message, 7s, transcribed]: book the dentist for tuesday"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestRenderInboundDocumentIncludesNameMimeSize(t *testing.T) {
	in := channel.InboundMsg{Media: []media.Artifact{{
		Kind: media.KindDocument, Path: "/p/report.pdf", Name: "report.pdf",
		Mime: "application/pdf", Size: 4096,
	}}}
	got := RenderInbound(in)
	for _, frag := range []string{"/p/report.pdf", "name: report.pdf", "type: application/pdf", "size: 4096 bytes", "Read tool"} {
		if !strings.Contains(got, frag) {
			t.Errorf("missing %q in %q", frag, got)
		}
	}
}

func TestRenderInboundMultipleArtifacts(t *testing.T) {
	in := channel.InboundMsg{
		Text: "two things",
		Media: []media.Artifact{
			{Kind: media.KindImage, Path: "/p/a.jpg"},
			{Kind: media.KindAudio, DurationS: 3, Transcript: "and a voice note"},
		},
	}
	got := RenderInbound(in)
	if strings.Count(got, "[The user sent an image") != 1 || strings.Count(got, "[voice message,") != 1 {
		t.Errorf("markers wrong: %q", got)
	}
	if len(strings.Split(got, "\n\n")) != 3 {
		t.Errorf("expected 3 blocks: %q", got)
	}
}
