# 文生图真实流程抓包记录(MCP 抓官网 chatgpt.com)

抓取时间:2026-07-08。工具:MCP user-js-reverse(注入 fetch 拦截器,记录 SSE 流 + 请求体 + 所有 GET /conversation 轮询)。
模型:`gpt-5-5-thinking`。提示词:"生成6张不同风格的猫的图片,写实、油画、水彩、像素、卡通、赛博朋克各一张"。

## 1. 请求体(POST /backend-api/f/conversation)

关键字段(网页版真实):

```json
{
  "action": "next",
  "messages": [{
    "author": {"role": "user"},
    "content": {"content_type": "text", "parts": ["生成6张..."]},
    "metadata": {"selected_sources": [], "serialization_metadata": {"custom_symbol_offsets": []}}
  }],
  "parent_message_id": "client-created-root",
  "model": "gpt-5-5-thinking",
  "client_prepare_state": "none",
  "conversation_mode": {"kind": "primary_assistant"},
  "system_hints": [],
  "supports_buffering": true,
  "supported_encodings": ["v1"],
  "paragen_cot_summary_display_override": "allow",
  "force_parallel_switch": "auto",
  "thinking_effort": "extended"
}
```

- **多图/多风格的关键是 `model=gpt-5-5-thinking` + `thinking_effort=extended`**。
  非 thinking 模型(gpt-5-5 / gpt-4o)不会把"6张"拆成 batch_requests,直接出 1 张(常合成拼图)。
- 是否真正拆成 N 张由模型自主决定,**有随机性**:同一 prompt 有时拆 6 条 batch_requests(出6张),有时只 1 条(出1张)。

## 2. SSE 流(极短,约 0.76 秒就 [DONE])

主 `POST /f/conversation` 的 SSE 只有 3 条有效数据后立即结束:

```
event: delta_encoding
data: "v1"
data: {"type": "resume_conversation_token", "kind": "topic", "token": "...", "conversation_id": "..."}
data: {"type": "stream_handoff", "conversation_id": "...", "turn_exchange_id": "...",
       "options": [
         {"type": "resume_sse_endpoint", "topic_id": "conversation-turn-..."},
         {"type": "subscribe_ws_topic",  "topic_id": "conversation-turn-..."}
       ]}
data: [DONE]
```

**要点:主 SSE 里没有任何生图关键词(ghostrider/dalle/image_gen)。** 这些信号只在后续 GET /conversation 的响应里出现。`stream_handoff` 提供了 SSE 续流与 WS 两个选项,但网页实际选择了**轮询**。

## 3. 图片下发:轮询 GET /conversation/{id}(不走 WS)

网页在 SSE [DONE] 后,每约 10.5s 轮询一次 `GET /backend-api/conversation/{id}`:

| 相对时间 | async_status | image_asset_pointer 数 |
|---|---|---|
| 15.9s | 3(生成中) | 0 |
| 26.3s | 3 | 0 |
| 36.9s | 3 | 0 |
| 47.4s | 3 | 0 |
| 58.1s | **4(完成)** | 1 |

- `async_status`: **3=生成中,4=完成**(另见过 **5**,出现在 thinking 刚拆 batch_requests、生图排队阶段,亦应视为"进行中,需继续轮询")。
- 拿到 async_status=4 后网页**停止轮询**。图片节点在 mapping 中:
  - `code`(language=json,含 `batch_requests` 数组,recipient=`t2uay3k.sj1i4kz`)= imagegen 工具的批量请求(N 条 = N 张)。
  - `tool`(name=`t2uay3k.sj1i4kz`)的 `multimodal_text` 节点承载 `image_asset_pointer`(最终图)。

## 4. 服务端(sentinel-go)当前 bug 与修复方向

**bug**:服务端在主 SSE `[DONE]` 后,因这段 SSE 不含生图关键词 →
`probeImageRouteFromSSE` 未命中 → `sawImageGenTool=false` → 判为普通对话,3s 返回空,**从不轮询**。

**修复方向**:
1. 收到 `stream_handoff`(或 thinking 模型 + [DONE] 但正文为空)后,**不要直接判普通对话结束**。
2. SSE 结束后**主动轮询一次 GET /conversation**,读 `async_status`:
   - 有 async_status 且 ∈{3,5}(或 <4)→ 进入持续轮询直到 4;
   - =4 → 收齐 mapping 中所有 image_asset_pointer;
   - 无 async_status 字段 → 同步完成(旧路径),按现有逻辑收图。
3. 轮询判定放宽:不再强依赖 SSE 阶段的 `sawImageGenTool`,而是以 **stream_handoff + async_status** 为准。

## 5. 确定性 6 张独立图抓包(2026-07-08 复现成功)

会话 `6a4e2636-...`。提示词:"请分别生成6张不同风格(写实、卡通、水彩、像素、赛博朋克、国风)的猫咪图片,每种风格一张独立的图"。
完成后(async_status=null,已收尾)mapping 结构:

```
system/text ×2
user/text                      ← 用户提示
tool/t2uay3k.sj1i4kz/multimodal_text   ← 占位/首节点
assistant/model_editable_context
system/text
assistant/thoughts             ← thinking 思考
assistant/code                 ← 一个 code 节点,含 batch_requests:[6 条]
tool/t2uay3k.sj1i4kz/multimodal_text ×6 ← 6 个独立图像节点,每张一个
tool/t2uay3k.sj1i4kz/text
assistant/reasoning_recap
assistant/text                 ← 最终文字总结
```

实测统计:
- `batch_requests` = **6 条**,每条 prompt 形如 `"Create a single standalone image of a cat..."`(模型把"6种风格"自动拆成 6 个独立子请求)。
- **6 个唯一 file_id**,`image_asset_pointer` 共出现 **12 次**(每张图在 mapping 里被引用 2 次:批量输出节点 + 独立展示节点)。
- **重要变更:asset_pointer 协议头已从 `file-service://` 改为 `sediment://`**,例如
  `sediment://file_00000000b9c471fd...`。

### 对 sentinel-go 收图/下载的要求

1. **收图去重**:遍历整个 mapping,收集所有 `tool`(name=`t2uay3k.sj1i4kz`)`multimodal_text` 节点里的 `image_asset_pointer`,
   **按 file_id 去重**(因每图出现 2 次),得到 N=6 张,而非只取第一张。
2. **兼容 `sediment://` 前缀**:下载解析 asset_pointer 时,除 `file-service://` 外,必须识别 `sediment://`,
   提取 `file_...` 作为 file_id 走 `files/download` / `interpreter/download` 换取 download_url。
3. **张数由 batch_requests 决定**:可用 `code` 节点里的 `batch_requests.length` 作为期望张数,
   轮询到收齐该数量的唯一 file_id 再结束(或 async_status=4/null 且不再增长)。
4. **随机性提醒**:同一 prompt,模型有时只出 1 张、有时走 code+text 不出图。服务端应以实际 mapping 为准,
   不能假设固定张数;拿 batch_requests.length 作上界、async_status 作完成信号即可。

---

## 图生图(上传参考图 + 改图)真实协议 — 2026-07-08 MCP 抓包

用 MCP(CDP)在官网同源取回一张已生成图 → 构造 File 塞进 composer 文件输入 → 真实触发上传,
输入"给小猫戴红色圣诞帽",完整捕获上传链 + `/f/conversation` 请求体 + 出图结构。

### 1) 上传链(4 步,PUT 走 XHR 未被 fetch hook 捕获,但可确定)

```
POST /backend-api/files                         → 返回签名 upload_url(oaiusercontent.com)
PUT  <upload_url>  (原始字节)                    → 201(x-ms-blob-type: BlockBlob)
POST /backend-api/files/process_upload_stream    → NDJSON 流:file.processing.started → file_ready
GET  /backend-api/files/{id}/simple              → 元数据(含 library_file_id)
GET  /backend-api/files/download/{id}            → 预览 url
```

**关键结论:旧的 `POST /backend-api/files/{id}/uploaded` 确认端点仍然有效**
(浏览器实测手动跑旧三步:PUT=201、`/uploaded`=200 success)。
故 `sentinel-go` 现有三步上传(create → PUT → `/uploaded`)无需改动,可继续使用。

`POST /backend-api/files` 请求体(官网):
```json
{"file_name":"cat_ref.png","file_size":2305094,"use_case":"multimodal",
 "mime_type":"image/png","timezone_offset_min":-480,"reset_rate_limits":false,
 "supports_direct_azure_multipart":true,"entry_surface":"chat_composer",
 "selection_method":"file_picker","client_resolved_mime_type":"image/png"}
```
最小可用集为 `file_name/file_size/use_case/mime_type(+width/height)`,其余为埋点字段可省略。

### 2) `/f/conversation` 请求体:参考图如何被引用(核心)

```json
{
  "model": "gpt-5-5-thinking",
  "messages": [{
    "author": {"role": "user"},
    "content": {
      "content_type": "multimodal_text",
      "parts": [
        {"content_type":"image_asset_pointer",
         "asset_pointer":"sediment://file_00000000e64c...",
         "size_bytes":1564800,"width":1254,"height":1254},
        "请在这张小猫照片的基础上，给它戴一顶红色的圣诞帽，其余保持不变"
      ]
    },
    "metadata": {"attachments": [
      {"id":"file_00000000e64c...","size":1564800,"name":"cat_ref.png",
       "mime_type":"image/png","width":1254,"height":1254,
       "source":"local","library_file_id":"libfile_..."}]}
  }],
  "thinking_effort":"...", "paragen_cot_summary_display_override":"...",
  "force_parallel_switch":"..."
}
```

与旧版差异(已在 `sentinel/files.go` 对齐):
- `asset_pointer` 前缀:`file-service://` → **`sediment://`**;
- `metadata.attachments[*].mimeType`(驼峰)→ **`mime_type`**(蛇形),并新增 **`source:"local"`**;
- 注:**无 `picture_v2` 字段** —— 模型见到图 + 改图指令即自动进入编辑,走与文生图相同的 thinking 轮次。

### 3) 出图结构 = 与文生图一致

编辑后的图作为 `role=tool`(recipient=`t2uay3k.sj1i4kz`)的 `multimodal_text` 节点里的
`image_asset_pointer`(`sediment://file_...`,**带 gen_id 元数据**),经 `assistant/code` 触发。
故收图/轮询逻辑直接复用文生图路径,无需新增。

### 4) 端到端验证(sentinel-go 服务端)

`POST /v1/chat/completions`(model=gpt-5-5-thinking + image_url data URL + 改图 prompt):
上传参考图 → 探测判定生图轮次 → 轮询 async_status 3→4 → 收到 1 张编辑图
`file_000000009d70...`(gen_id 齐全)→ 图片代理下载为有效 PNG(1.85MB),
橙猫成功戴上红色圣诞帽、其余保持不变。**图生图流程打通。**

---

## 文件上传(给 AI 分析总结)真实协议 — 2026-07-08 MCP 抓包

MCP 构造一个 .txt 文档 File 塞进 composer 触发上传,发送"总结这个文档"。

### 上传链 = 与图片同(仅 use_case 不同)

- `POST /files` 请求体:`use_case:"my_files"`(图片是 `multimodal`)、`mime_type:"text/plain"`;
- `process_upload_stream` 请求体:`use_case:"my_files"`、**`index_for_retrieval:true`**(文档要建检索/RAG 索引;图片为 false);
- 旧确认端点 `/uploaded` 同样有效,`sentinel-go` 对非图片已用 `my_files`,**无需改动**。

### `/f/conversation` 请求体:文档与图片的关键区别

```json
{
  "content": {"content_type": "text", "parts": ["请总结这个文档的核心内容"]},
  "metadata": {"attachments": [
    {"id":"file_...","size":683,"name":"sentinel_report.txt","mime_type":"text/plain",
     "source":"local","library_file_id":"libfile_...","is_big_paste":false}]}
}
```

- **文档不进 parts**(无 `image_asset_pointer`),`content_type` 保持 `text`,只挂在 `metadata.attachments`;
- 图片才用 `multimodal_text` + parts 里的 `image_asset_pointer`。
- `sentinel/chat.go` 现有逻辑:仅 `use_case==multimodal` 的图片加进 parts,文档只进 attachments —— **主结构本就匹配**。

### 端到端验证 + 修复一个探测 bug

首次实测服务端返回 **HTTP 200 但正文为空**。根因:`gpt-5-5-thinking` 处理文档时会思考,
思考期间 `async_status=3`;而 `probeImageTurnViaConversation` 旧逻辑把 `async_status∈{3,4,5}`
直接判为"生图轮次",于是切到收图轮询、把真正的文字总结丢弃了。
(文档轮次会话实测:`batch_requests/image_asset_pointer/sediment://` 全为 0,只有 async_status=3。)

**修复**(`sentinel/chat.go`):
- 探测**不再用 async_status 判生图**(它对文本/生图都出现);改用真正的生图标记
  `imageTurnMarkers = {batch_requests, image_asset_pointer, sediment://, t2uay3k, image_gen_task_id}`;
- 探测到 assistant 正文且无生图标记 → 就地取回正文(`extractAssistantFinalText`),置 `asyncTextResolved`,
  跳过 WS catchup(避免空闲 WS 失效);
- 收图轮询兜底:命中生图标记后若 0 图(官网卡住/模型改用文本),回退拉取 assistant 正文,避免整轮回复丢失。

修复后:文档总结 HTTP 200、15.6s、正文正确(模型答出 `imroc/req`、文档暗号、一句话总结,带 filecite 引用)。
回归:`picture_v2` 强制生图仍正常(柯基 62.3s 出 1 图);探测对真实生图轮次经 `t2uay3k` 正确识别、无回归。

---

## 各模型真实 API 参数 — 2026-07-09 MCP 抓包

抓取方法:Chrome CDP + fetch 拦截器,对 `POST /backend-api/f/conversation` 请求体逐一抓取。
消息内容:「说一个字：好」(纯文本,无图片附件)。

### 完整映射表

| 官网 UI 名称 | 后端 `model` 字段 | `thinking_effort` | 说明 |
|---|---|---|---|
| 极速 | `gpt-5-5` | （不携带此字段） | 最快响应，无思考 |
| 均衡 | `gpt-5-5-thinking` | `"standard"` | 标准思考 |
| 高级（默认） | `gpt-5-5-thinking` | `"extended"` | 深度思考，多图并行关键 |
| GPT-5.5（子菜单） | `gpt-5-5-thinking` | `"standard"` | 同"均衡" |
| GPT-5.4 | `gpt-5-4-thinking` | `"standard"` | 上一代 thinking 模型 |
| GPT-5.3（极速 5.3） | `gpt-5-3-instant` | （不携带此字段） | 最轻量 |
| o3 / Medium | `o3` | （不携带此字段） | 推理专用，不携带 thinking_effort |

**通用字段（所有模型一致）**:
```json
{
  "conversation_mode": {"kind": "primary_assistant"},
  "force_parallel_switch": "auto",
  "paragen_cot_summary_display_override": "allow"
}
```

### 关键结论

1. **`thinking_effort` 不是所有模型都发送**：`gpt-5-5`（极速）、`gpt-5-3-instant`、`o3` 不携带此字段；
   只有 `gpt-5-5-thinking` 和 `gpt-5-4-thinking` 才发送 `standard`/`extended`。

2. **官网 UI"极速"的 model 是 `gpt-5-5`**（不含 -thinking 后缀），与"均衡/高级"使用的
   `gpt-5-5-thinking` 是**两个不同的 model 字符串**。

3. **多图并行需要 `gpt-5-5-thinking` + `thinking_effort: "extended"`**（即官网"高级"模式）。

4. **GPT-5.3 的后端 model 是 `gpt-5-3-instant`**（含 -instant 后缀），非 `gpt-5-3`。

### sentinel-go 代码已更新（`sentinel/model_resolve.go`）

用户可用的 model 别名（调用 `/v1/chat/completions` 时传入）：

| 请求 model（用户传入） | 实际后端 model | thinking_effort |
|---|---|---|
| `gpt-5-5-thinking`（默认） | `gpt-5-5-thinking` | `extended` |
| `gpt-5-5` | `gpt-5-5` | （不携带） |
| `gpt-5-4` / `gpt-5-4-thinking` | `gpt-5-4-thinking` | `standard` |
| `gpt-5-3` / `gpt-5-3-instant` | `gpt-5-3-instant` | （不携带） |
| `o3` | `o3` | （不携带） |
| `dall-e-3` / `gpt-image-2` | `dall-e-3` | n/a（picture_v2） |
