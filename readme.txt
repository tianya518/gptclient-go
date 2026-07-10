sentinel-go —— ChatGPT 逆向的 OpenAI 兼容 API 服务

【启动服务端】
  go run ./cmd/server
  或双击 build 出的 sentinel-server.exe
  默认监听 http://localhost:5005

【接入方式】
  用任意 OpenAI 兼容客户端（推荐 Open WebUI），把 API 地址填为：
      http://localhost:5005/v1
  API Key 任意填写。

【常用端点】
  GET  http://localhost:5005/         服务信息
  GET  http://localhost:5005/health   健康检查
  POST http://localhost:5005/v1/chat/completions   对话
  GET  http://localhost:5005/v1/models             模型列表
