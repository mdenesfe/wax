package client

import (
	"testing"

	"github.com/mdenesfe/wax/internal/proto/wae2e"
)

// TestAddPreview: preview details land in the right message fields; a
// waveform only on voice notes; zero values stay unset.
func TestAddPreview(t *testing.T) {
	img := &wae2e.Message{ImageMessage: &wae2e.ImageMessage{}}
	addPreview(img, &SendMediaRequest{Kind: KindImage, Thumbnail: []byte("t"), Width: 640, Height: 480})
	if m := img.ImageMessage; string(m.JPEGThumbnail) != "t" || m.GetWidth() != 640 || m.GetHeight() != 480 {
		t.Fatalf("image %v", m)
	}
	vid := &wae2e.Message{VideoMessage: &wae2e.VideoMessage{}}
	addPreview(vid, &SendMediaRequest{Kind: KindVideo, Seconds: 12})
	if m := vid.VideoMessage; m.GetSeconds() != 12 || m.Width != nil || m.JPEGThumbnail != nil {
		t.Fatalf("video %v", m)
	}
	voice := &wae2e.Message{AudioMessage: &wae2e.AudioMessage{PTT: new(bool)}}
	addPreview(voice, &SendMediaRequest{Kind: KindVoiceNote, Seconds: 3, Waveform: []byte{1, 2}})
	if m := voice.AudioMessage; m.GetSeconds() != 3 || len(m.Waveform) != 2 {
		t.Fatalf("voice %v", m)
	}
	audio := &wae2e.Message{AudioMessage: &wae2e.AudioMessage{}}
	addPreview(audio, &SendMediaRequest{Kind: KindAudio, Waveform: []byte{1}})
	if audio.AudioMessage.Waveform != nil {
		t.Fatal("waveform on a plain audio file")
	}
}
