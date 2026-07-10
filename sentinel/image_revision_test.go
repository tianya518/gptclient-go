package sentinel

import "testing"

func TestMultiImageSlots(t *testing.T) {
	result := &ChatResult{
		ExpectGeneratedImages: true,
		pictureV2Path:           true,
	}
	result.ensureImageSlots()
	c := &Client{}
	opts := ChatOptions{ForcePictureV2: true}

	ingest := func(genID, fileID string) {
		c.noteGeneratedImageRevision(result, opts, ParsedGeneratedImage{
			GenID: genID, FileID: fileID, MessageID: "msg-" + genID,
		}, "test")
	}
	ingest("gen-a", "file_aaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	ingest("gen-b", "file_bbbbbbbbbbbbbbbbbbbbbbbbbbbb")

	if len(result.imageSlots) != 2 {
		t.Fatalf("expected 2 slots, got %d", len(result.imageSlots))
	}
	result.RebuildImageFileIDsFromSlots()
	if len(result.ImageFileIDs) != 2 {
		t.Fatalf("ImageFileIDs=%v", result.ImageFileIDs)
	}
	if !result.CanSkipImageWSAfterSSE() {
		t.Fatal("2 populated slots should allow skip WS after SSE")
	}
	result.imageAsyncTaskPending = 1
	if result.CanSkipImageWSAfterSSE() {
		t.Fatal("pending async should not skip WS")
	}
}

func TestPictureV2ImageWithoutGenID(t *testing.T) {
	result := &ChatResult{
		ExpectGeneratedImages: true,
		pictureV2Path:           true,
		userReferenceFileIDs:    map[string]bool{"file_user_ref": true},
	}
	if !result.allowPictureV2ImageWithoutGenID("file_generated") {
		t.Fatal("picture_v2 should accept generated file without gen_id")
	}
	if result.allowPictureV2ImageWithoutGenID("file_user_ref") {
		t.Fatal("user reference file must be excluded")
	}

	c := &Client{}
	opts := ChatOptions{ForcePictureV2: true}
	msg := map[string]interface{}{
		"id": "msg-asst-1",
		"author": map[string]interface{}{
			"role": "assistant",
		},
		"content": map[string]interface{}{
			"content_type": "multimodal_text",
			"parts": []interface{}{
				map[string]interface{}{
					"content_type":  "image_asset_pointer",
					"asset_pointer": "sediment://file_aabb11223344",
					"width":         float64(1024),
					"height":        float64(768),
				},
			},
		},
	}
	c.tryNoteGeneratedImagesFromMessage(msg, result, opts, "test")
	if !result.HasDalleGeneratedOutput() {
		t.Fatalf("expected generated output, slots=%+v", result.imageSlots)
	}
	if len(result.ImageFileIDs) == 0 {
		result.RebuildImageFileIDsFromSlots()
	}
	if len(result.ImageFileIDs) != 1 || result.ImageFileIDs[0] != "file_aabb11223344" {
		t.Fatalf("ImageFileIDs=%v", result.ImageFileIDs)
	}
}
