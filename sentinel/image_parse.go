package sentinel

// image_parse.go —— 从会话/WS 消息里解析出「生图条目」（ParsedGeneratedImage）。
//
// 负责识别 image_asset_pointer、抽取 file_id / gen_id / 父图关系，并按角色与路径规则
// （classic DALL·E 有 gen_id；picture_v2 / imagegen skill 常无 gen_id）决定是否接受为生图产出。

// ParsedGeneratedImage 从 WS message / part 解析出的生图条目。
type ParsedGeneratedImage struct {
	FileID      string
	MessageID   string
	GenID       string
	ParentGenID string
	EditOp      string
	Width       int
	Height      int
}

func parseGeneratedImagesFromMessage(msg map[string]interface{}) []ParsedGeneratedImage {
	msgID, _ := msg["id"].(string)
	content, _ := msg["content"].(map[string]interface{})
	parts, _ := content["parts"].([]interface{})
	var out []ParsedGeneratedImage
	appendPart := func(partMap map[string]interface{}) {
		if partMap["content_type"] != "image_asset_pointer" {
			return
		}
		ap, _ := partMap["asset_pointer"].(string)
		fileID := extractFileID(ap)
		if fileID == "" {
			return
		}
		p := ParsedGeneratedImage{FileID: fileID, MessageID: msgID}
		if w, ok := partMap["width"].(float64); ok {
			p.Width = int(w)
		}
		if h, ok := partMap["height"].(float64); ok {
			p.Height = int(h)
		}
		if meta, ok := partMap["metadata"].(map[string]interface{}); ok {
			if dalle, ok := meta["dalle"].(map[string]interface{}); ok {
				p.GenID, _ = dalle["gen_id"].(string)
				if pg, ok := dalle["parent_gen_id"].(string); ok {
					p.ParentGenID = pg
				}
				p.EditOp, _ = dalle["edit_op"].(string)
			}
		}
		out = append(out, p)
	}
	for _, part := range parts {
		partMap, ok := part.(map[string]interface{})
		if !ok {
			continue
		}
		appendPart(partMap)
	}
	if ct, _ := content["content_type"].(string); ct == "image_asset_pointer" {
		appendPart(content)
	}
	return out
}

func (result *ChatResult) isUserReferenceFile(fileID string) bool {
	return result != nil && result.userReferenceFileIDs != nil && result.userReferenceFileIDs[fileID]
}

// allowPictureV2ImageWithoutGenID picture_v2 / image_gen 工具产出常无 dalle.gen_id，但仍为有效生图。
func (result *ChatResult) allowPictureV2ImageWithoutGenID(fileID string) bool {
	if fileID == "" || result == nil || result.isUserReferenceFile(fileID) {
		return false
	}
	return result.pictureV2Path || result.sawImageGenTool || result.ImageTaskID != ""
}

func (c *Client) tryNoteGeneratedImagesFromMessage(msg map[string]interface{}, result *ChatResult, opts ChatOptions, via string) {
	if c == nil || result == nil || !result.ExpectGeneratedImages {
		return
	}
	role := getNestedString(msg, "author", "role")
	imgs := parseGeneratedImagesFromMessage(msg)
	if len(imgs) == 0 {
		return
	}
	if c != nil {
		c.logf("[image-parse-dbg] via=%s role=%s imgs=%d sawTool=%v allowNoGen=%v",
			via, role, len(imgs), result.sawImageGenTool, result.allowPictureV2ImageWithoutGenID(imgs[0].FileID))
	}
	switch role {
	case "assistant":
		// picture_v2 / image_gen 常把图放在 assistant multimodal_text
	case "tool":
		// classic DALL·E 出图常在 tool 消息（含 dalle.gen_id）
		// imagegen skill 路径（sawImageGenTool=true）图片无 gen_id，也放行
		accepted := false
		for _, img := range imgs {
			if img.GenID != "" || result.allowPictureV2ImageWithoutGenID(img.FileID) || result.sawImageGenTool {
				accepted = true
				break
			}
			if c != nil {
				c.logf("[image-parse] tool img file=%s gen_id=%q allowNoGen=%v sawTool=%v -> rejected",
					img.FileID, img.GenID, result.allowPictureV2ImageWithoutGenID(img.FileID), result.sawImageGenTool)
			}
		}
		if !accepted {
			return
		}
	case "":
		// conversation API 轮询（conv_poll）返回的图片节点有时没有 author.role
		// imagegen skill 路径：只要 sawImageGenTool=true 就放行
		if !result.sawImageGenTool {
			return
		}
	default:
		return
	}
	msgID, _ := msg["id"].(string)
	for _, img := range imgs {
		if img.MessageID == "" {
			img.MessageID = msgID
		}
		if c != nil && img.GenID == "" {
			c.logf("[image-parse] %s image file=%s gen_id=空 via=%s allow_no_gen=%v",
				role, img.FileID, via, result.allowPictureV2ImageWithoutGenID(img.FileID))
		} else if c != nil && img.GenID != "" {
			c.logf("[image-parse] %s image file=%s gen_id=%s via=%s", role, img.FileID, img.GenID, via)
		}
		c.noteGeneratedImageRevision(result, opts, img, via)
	}
}
