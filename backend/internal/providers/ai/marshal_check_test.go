package ai

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestChatMessageMarshalShapes(t *testing.T) {
	tm := ChatMessage{Role: "user", Content: "hello"}
	b, _ := json.Marshal(tm)
	got := string(b)
	want := `{"role":"user","content":"hello"}`
	if got != want {
		t.Errorf("text marshal: got %s want %s", got, want)
	}

	im := NewImageMessage("user", "describe this", []ImageData{
		{Bytes: []byte("PNGDATA"), ContentType: "image/png"},
	})
	b2, _ := json.Marshal(im)
	s2 := string(b2)
	if !strings.Contains(s2, `"type":"text"`) || !strings.Contains(s2, `"type":"image_url"`) || !strings.Contains(s2, `"data:image/png;base64,UE5HREFUQQ=="`) {
		t.Errorf("image marshal missing expected parts: %s", s2)
	}

	var dec ChatMessage
	if err := json.Unmarshal([]byte(`{"role":"assistant","content":"got it"}`), &dec); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dec.Role != "assistant" || dec.Content != "got it" || len(dec.Parts) != 0 {
		t.Errorf("decoded wrong: %+v", dec)
	}

	om := ollamaMessageFromChat(im)
	if om.Role != "user" || om.Content != "describe this" || len(om.Images) != 1 || om.Images[0] != "UE5HREFUQQ==" {
		t.Errorf("ollama message wrong: %+v", om)
	}

	// text-only message through ollama path: no images field
	omText := ollamaMessageFromChat(ChatMessage{Role: "system", Content: "sys"})
	if omText.Role != "system" || omText.Content != "sys" || len(omText.Images) != 0 {
		t.Errorf("ollama text message wrong: %+v", omText)
	}
}