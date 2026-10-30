# 模型网关(OpenAI 兼容端点)

WeKnora 可以把**已发布的智能体**直接暴露为 OpenAI 兼容模型:外部客户端(pi、zcode、openai-python、LangChain、Cherry Studio 等)像添加一个普通 OpenAI 模型那样添加 WeKnora 智能体,`chat/completions` 请求由网关转成一次智能体单轮问答。实现见 `internal/handler/openai_compat/`,设计文档见 `docs/design/agent-model-gateway-m1.md`。

## 开启方式

网关默认关闭。设置环境变量后重启后端:

```
GATEWAY_ENABLED=true
```

全部配置项(均有默认值,见 `internal/config/config.go` 的 `GatewayConfig`):

| 环境变量 | 默认 | 说明 |
| --- | --- | --- |
| `GATEWAY_ENABLED` | `false` | 总开关,关闭时 `/v1` 路由不注册 |
| `GATEWAY_REQUEST_TIMEOUT` | `600s` | 单次补全内部预算(Go duration 或纯秒数) |
| `GATEWAY_HEARTBEAT_INTERVAL` | `15s` | 首个内容输出前 keep-alive 注释行间隔 |
| `GATEWAY_MAX_CONCURRENCY_PER_KEY` | `3` | 每个 API key 同时进行的补全数,超限立即 429;`0` 表示不限制(按副本计) |
| `GATEWAY_SESSION_RETENTION` | `72h` | 网关临时会话(审计用)保留时长,超期由后台任务回收 |
| `GATEWAY_THOUGHT_MODE` | `reasoning` | 思考过程:`reasoning`(进 `delta.reasoning_content`)/ `collapse`(包 `<think>` 进正文)/ `drop` |
| `GATEWAY_PROGRESS_MODE` | `text` | 工具/进度行:`text` / `drop` |
| `GATEWAY_AUDIT_PAYLOAD` | `false` | 审计明细是否留存用户 query(截断 2000 字符);默认只记元数据 |
| `GATEWAY_CANCEL_ON_DISCONNECT` | `true` | 客户端断开是否取消内部运行;`false` 时跑完并照常落库 |

## 认证

使用空间级或平台级 API key(`Authorization: Bearer <key>` 或 `X-API-Key: <key>`),key 需要 **`chat` capability**(或 `full_access`)。网关挂在 `/v1` 前缀下,不走 `/api/v1` 的 JWT 认证;密钥格式与创建方式同 [API 总览](01-api-overview.md#_2-api-key-机器主体)。

```bash
BASE=http://localhost:8080
KEY=sk-xxxx   # POST /api/v1/tenants/:id/api-keys 创建,勾选 chat 能力
```

## 端点

### GET /v1/models

列出当前空间已发布的智能体,每个智能体一个 model:

```bash
curl -s $BASE/v1/models -H "Authorization: Bearer $KEY"
```

```json
{
  "object": "list",
  "data": [
    {
      "id": "<agent-uuid>",
      "object": "model",
      "created": 1735689600,
      "owned_by": "weknora",
      "meta": {"agent_name": "文档问答", "description": "...", "agent_mode": "quick_agent", "is_builtin": false}
    }
  ]
}
```

`id`(UUID)与 `meta.agent_name` 都可以作为 `chat/completions` 的 `model` 参数。

### POST /v1/chat/completions

**messages 折叠规则**:整个 `messages` 数组折叠为智能体的单轮输入——system 消息叠进调用方指令(每条截断 8000 字符),其余按对话历史注入(最近 40 条、32000 字符),最后一条 user 消息为本次问题。**没有跨请求会话续接**:每次调用创建独立的临时会话(审计保留 72h)。

**非流式**:

```bash
curl -s $BASE/v1/chat/completions -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"文档问答","messages":[{"role":"user","content":"这份手册的保修期多久?"}]}'
```

```json
{
  "id": "chatcmpl-...",
  "object": "chat.completion",
  "model": "文档问答",
  "choices": [{"index": 0, "message": {"role": "assistant", "content": "整机保修 2 年…\n---\n参考来源:\n1. 产品手册"}, "finish_reason": "stop"}],
  "usage": {"prompt_tokens": 120, "completion_tokens": 30, "total_tokens": 150},
  "meta": {"ignored_params": ["temperature", "top_p", "max_tokens", "reasoning_effort", "stop", "n", "logprobs"], "agent_id": "…", "session_id": "…"}
}
```

**流式**(SSE,`stream=true`):响应为 `text/event-stream`,帧格式 `data: {chat.completion.chunk}`,首个内容前每 `GATEWAY_HEARTBEAT_INTERVAL` 输出一行 `: keep-alive` 注释,以 `data: [DONE]` 结束;最后一个 chunk 带 `usage` 与 `meta`。思考过程按 `GATEWAY_THOUGHT_MODE` 进入 `delta.reasoning_content` 或正文;工具/进度按 `GATEWAY_PROGRESS_MODE` 输出单行状态文本(如 `[工具] knowledge_search → 3 条结果`)或丢弃。中途失败输出一个 `data: {"error":{…}}` 帧后仍以 `[DONE]` 收尾(HTTP 状态保持 200)。

```bash
curl -N $BASE/v1/chat/completions -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"文档问答","stream":true,"messages":[{"role":"user","content":"保修期多久?"}]}'
```

多模态:`content` 数组中的 `image_url` 仅在智能体开启图片能力时生效(每次最多 4 张、单张 ≤10MB),否则忽略并在 `meta.warnings` 提示。

## 错误码

| HTTP | code | 场景 |
| --- | --- | --- |
| 400 | (空)/参数级 | 请求体、`model` 缺失、`messages` 为空或渲染失败 |
| 401 | `invalid_api_key` | 缺少或无效 API key |
| 403 | `insufficient_capability` | key 无 `chat` 能力位 |
| 404 | `model_not_found` | model 不存在、未发布或跨空间 |
| 429 | `concurrency_limit` | 该 key 并发数超过 `GATEWAY_MAX_CONCURRENCY_PER_KEY` |
| 500 | `internal_error` | 会话/消息创建或智能体运行失败 |

## 审计与限流

- 每次成功进入智能体执行的补全写一条 `gateway.completion` 审计日志(`POST /v1/chat/completions`),明细含 key 指纹(id+名称,**不含密钥原文**)、解析到的智能体、调用方原始 model 字符串、stream 标志、token 用量、耗时与结束原因(`completed`/`error`/`timeout`/`disconnect`/`disconnect_run_completed`)。`GATEWAY_AUDIT_PAYLOAD=true` 时额外留存截断后的 query。
- 并发限制按 key 计数,**每个后端副本独立计数**;多副本部署时全局上限为副本数 × 每副本上限。
- 网关临时会话(`kind=gateway`)由后台任务按 `GATEWAY_SESSION_RETENTION` 回收(每小时一轮,单轮最多 200 条)。

## M1 已知限制

- 客户端 `tools` / `function` 定义**不透传**(M1 不执行客户端工具),响应 `finish_reason` 恒为 `stop`;发送了 tools 时在 `meta.client_tools_ignored` 回显数量。双向工具桥在 M3(HITL)实现。
- 无跨请求记忆;`temperature` 等采样参数以智能体配置为准(在 `meta.ignored_params` 回显)。
- `usage` 为内部 agent loop 各轮 LLM 调用的**累计值**(含检索、工具轮),通常显著大于客户端按输入长度估算的预期。
- 长任务受 `GATEWAY_REQUEST_TIMEOUT` 约束,超时时流式路径返回已产生的部分内容并在 `meta.warnings` 标注;非流式路径同样返回部分内容。
