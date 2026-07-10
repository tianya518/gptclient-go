package sentinel

import "testing"

func TestResolveChatModel_dallE3(t *testing.T) {
	r := ResolveChatModel("dall-e-3")
	if !r.ForcePictureV2 || r.ChatModel != "dall-e-3" || r.APIModel != "dall-e-3" {
		t.Fatalf("%+v", r)
	}
}

func TestResolveChatModel_gptImage2Alias(t *testing.T) {
	r := ResolveChatModel("gpt-image-2")
	if !r.ForcePictureV2 || r.ChatModel != ModelDALLE3 {
		t.Fatalf("%+v", r)
	}
	if r.APIModel != "gpt-image-2" {
		t.Fatalf("APIModel=%q", r.APIModel)
	}
}

func TestResolveChatModel_gptImage2ThinkingAlias(t *testing.T) {
	r := ResolveChatModel("gpt-image-2-thinking")
	if !r.ForcePictureV2 || r.ChatModel != ModelDALLE3 {
		t.Fatalf("%+v", r)
	}
}

func TestResolveChatModel_plainChat(t *testing.T) {
	r := ResolveChatModel("gpt-5-5-thinking")
	if r.ForcePictureV2 || r.ChatModel != "gpt-5-5-thinking" {
		t.Fatalf("%+v", r)
	}
}
