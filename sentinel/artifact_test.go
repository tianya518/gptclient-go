package sentinel

import "testing"

func TestBuildArtifactPlan(t *testing.T) {
	optsUpload := ChatOptions{
		Text:   "总结文档",
		Images: []UploadedFile{{FileName: "a.pdf", MimeType: "application/pdf"}},
	}
	if p := BuildArtifactPlan(nil, optsUpload, ""); p.PollSandboxFiles {
		t.Fatal("should not poll sandbox; artifacts come from SSE only")
	}

	withToolMeta := []ArtifactSignal{
		{Type: SignalPythonTool, Value: "python"},
		{Type: SignalExecutionOutput, Value: "execution_output"},
		{Type: SignalToolInvokedMeta, Value: "PythonCaasUserVisibleTool"},
	}
	if p := BuildArtifactPlan(withToolMeta, ChatOptions{Text: "run"}, ""); p.PollSandboxFiles {
		t.Fatal("tool signals without path should not trigger conversation poll")
	}

	withPath := []ArtifactSignal{{Type: SignalSandboxPath, Value: "/mnt/data/out.txt"}}
	if p := BuildArtifactPlan(withPath, ChatOptions{}, ""); p.PollSandboxFiles {
		t.Fatal("sandbox path in SSE should not poll")
	}
	if arts := SandboxArtifactsFromSignals(withPath, "msg-1"); len(arts) != 1 || arts[0].FileName != "out.txt" {
		t.Fatalf("artifacts from signals: %+v", arts)
	}

	if p := BuildArtifactPlan([]ArtifactSignal{{Type: SignalGhostrider}}, ChatOptions{}, "x"); !p.PollImage {
		t.Fatal("ghostrider should mark poll_image in plan (analysis only)")
	}
}

func TestIsGeneratedImageTurn(t *testing.T) {
	uploadOpts := ChatOptions{
		Text:   "识别这三个文件",
		Images: []UploadedFile{{FileName: "a.png"}, {FileName: "b.png"}, {FileName: "c.png"}},
	}
	fileSearchSigs := []ArtifactSignal{
		{Type: SignalFileSearch, Value: "file_search"},
		{Type: SignalTurnUseCase, Value: "multimodal"},
		{Type: SignalImageAsset, Value: "file_page_1"},
	}
	if IsGeneratedImageTurn(fileSearchSigs, uploadOpts) {
		t.Fatal("file_search + multimodal + user uploads should not be image gen")
	}

	genSigs := []ArtifactSignal{
		{Type: SignalImageGenTaskID, Value: "task-1"},
		{Type: SignalImageAsset, Value: "file_gen_1"},
	}
	if !IsGeneratedImageTurn(genSigs, ChatOptions{Text: "画一只猫"}) {
		t.Fatal("image_gen_task_id should be image gen")
	}

	if !IsGeneratedImageTurn(nil, ChatOptions{ForcePictureV2: true}) {
		t.Fatal("ForcePictureV2 should be image gen")
	}
}

func TestApplyArtifactsFromSignals(t *testing.T) {
	sandboxOnly := &ChatResult{
		ArtifactSignals: []ArtifactSignal{
			{Type: SignalSandboxPath, Value: "/mnt/data/a.pdf"},
			{Type: SignalImageAsset, Value: "file-abc"},
		},
		LastAssistantMsgID: "msg-1",
	}
	ApplyArtifactsFromSignals(sandboxOnly, ChatOptions{Text: "run"})
	if len(sandboxOnly.SandboxArtifacts) != 1 || sandboxOnly.SandboxArtifacts[0].FileName != "a.pdf" {
		t.Fatalf("sandbox: %+v", sandboxOnly.SandboxArtifacts)
	}
	if len(sandboxOnly.ImageFileIDs) != 0 {
		t.Fatalf("image asset without gen turn should be ignored: %+v", sandboxOnly.ImageFileIDs)
	}

	genResult := &ChatResult{
		ArtifactSignals: []ArtifactSignal{
			{Type: SignalGhostrider},
			{Type: SignalImageAsset, Value: "sediment://file_00000000a8a471fd8615d81d1b2c3671"},
		},
	}
	ApplyArtifactsFromSignals(genResult, ChatOptions{})
	if len(genResult.ImageFileIDs) != 0 {
		t.Fatalf("image gen turn should not fill ImageFileIDs from SSE sediment alone: %+v", genResult.ImageFileIDs)
	}
	if !genResult.ExpectGeneratedImages {
		t.Fatal("ExpectGeneratedImages should be true for ghostrider")
	}
}

func TestExtractSignalsFromJSON(t *testing.T) {
	evt := map[string]interface{}{
		"v": map[string]interface{}{
			"message": map[string]interface{}{
				"author":  map[string]interface{}{"role": "tool", "name": "python"},
				"content": map[string]interface{}{"content_type": "execution_output"},
			},
		},
	}
	sigs := ExtractSignalsFromJSON(evt)
	if !containsSignal(sigs, SignalPythonTool) || !containsSignal(sigs, SignalExecutionOutput) {
		t.Fatalf("signals: %+v", sigs)
	}

	fileSearchEvt := map[string]interface{}{
		"type": "server_ste_metadata",
		"metadata": map[string]interface{}{
			"turn_use_case": "multimodal",
		},
	}
	sigs2 := ExtractSignalsFromJSON(fileSearchEvt)
	if !containsSignal(sigs2, SignalTurnUseCase) {
		t.Fatalf("turn_use_case signal: %+v", sigs2)
	}
}

func containsSignal(sigs []ArtifactSignal, typ ArtifactSignalType) bool {
	for _, s := range sigs {
		if s.Type == typ {
			return true
		}
	}
	return false
}
