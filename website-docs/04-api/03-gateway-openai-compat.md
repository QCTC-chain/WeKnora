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

### POST /v1/messages(Anthropic 兼容,M2-5)

Claude Code、pi 等原生讲 Anthropic Messages 协议的客户端可直接调用,管线(鉴权/授权/配额/幂等/会话续接/审计/计量)与 `/v1/chat/completions` 完全一致,仅协议壳不同:

```bash
curl -X POST http://localhost:8080/v1/messages \
  -H "x-api-key: wk-xxxx" \
  -H "Content-Type: application/json" \
  -d '{"model":"文档问答","max_tokens":1024,"system":"你是文档助手","messages":[{"role":"user","content":"保修期多久?"}]}'
```

- **鉴权**:`x-api-key` 头(或 `Authorization: Bearer`),与 OpenAI 端点同一把 API key。
- **请求映射**:顶层 `system`(字符串或块数组)→ 系统消息;`text`/`image` content 块同 OpenAI(`image` base64 → data URL,受同样的图片能力/数量/大小限制);`tool_use`/`tool_result` 块按 M1 语义文本化记录,**不执行客户端工具**;`tools` 计数后忽略;`metadata.user_id` 参与会话续接派生身份。
- **流式**(`"stream":true`):SSE 事件 `message_start` → `content_block_delta`(`text_delta`/`thinking_delta`)→ `message_delta`(`stop_reason:"end_turn"` + output usage)→ `message_stop`;内容产生前以 `: keep-alive` 注释帧保活。thinking 类事件映射为 `thinking_delta`。
- **非流式**:返回 Anthropic `message` 对象(单 `text` 块,`stop_reason:"end_turn"`,`usage.input_tokens`/`output_tokens` 为内部累计值)。
- **续接/幂等**:`X-WeKnora-Session-ID` 与 `Idempotency-Key` 头语义与 OpenAI 端点相同。
- **错误壳**:`{"type":"error","error":{"type":"not_found_error","message":...}}`,类型按状态映射(400→`invalid_request_error`、401→`authentication_error`、403→`permission_error`、404→`not_found_error`、429→`rate_limit_error`、其余→`api_error`)。

### GET /v1/metrics(可观测性,M2-6)

Prometheus 文本格式 exposition(与网关共用 Bearer / `X-API-Key` 鉴权),供 Prometheus/Grafana 抓取:

| 指标 | 标签 | 说明 |
| --- | --- | --- |
| `gateway_requests_total` | protocol, outcome | 按协议(openai/anthropic)与结果分类(ok/client_error/not_found/rate_limited/server_error)计数的请求 |
| `gateway_model_not_found_total` | agent | 404 `model_not_found`(不存在/未发布/未授予)按智能体计数 |
| `gateway_budget_exceeded_total` | limit | 429 配额拒绝,`limit` ∈ key_requests / key_tokens / tenant_tokens |
| `gateway_concurrency_limited_total` | — | 429 `concurrency_limit` 拒绝总数 |
| `gateway_translation_dropped_total` | kind | 事件翻译丢弃/合并:thought_dropped / progress_dropped / tool_events_merged |
| `gateway_tokens_total` | direction | prompt / completion token 累计 |
| `gateway_ttfb_seconds` | protocol, agent | 流式首帧延迟直方图 |
| `gateway_turn_duration_seconds` | protocol | 端到端轮耗时直方图 |
| `gateway_internal_rounds` | protocol | 每请求内部 agent-loop 轮数直方图 |
| `gateway_token_amplification_ratio` | protocol | 内部总 token ÷ 可见答案 token(≈4 rune/token)直方图 |

排障:开启 `GATEWAY_AUDIT_PAYLOAD=true` 后,审计明细额外保留渲染后的 query 与采样事件流水(`event_log`,≤40 条,含相对毫秒与丢弃计数),用于复盘单请求的翻译过程。

## 错误码

| HTTP | code | 场景 |
| --- | --- | --- |
| 400 | (空)/参数级 | 请求体、`model` 缺失、`messages` 为空或渲染失败 |
| 401 | `invalid_api_key` | 缺少或无效 API key |
| 403 | `insufficient_capability` | key 无 `chat` 能力位 |
| 404 | `model_not_found` | model 不存在、未发布、跨空间或**未授予该 key**(`agent_grants` 白名单) |
| 409 | `session_busy` / `request_in_progress` / `idempotency_key_mismatch` | 会话已有进行中的轮 / 同 key 幂等键正在执行 / 幂等键复用到不同请求体 |
| 429 | `concurrency_limit` | 该 key 并发数超过 `GATEWAY_MAX_CONCURRENCY_PER_KEY` |
| 429 | `budget_exceeded` | 命中 key 或工作空间的每日 token/请求配额(消息内含 UTC 重置时间提示) |
| 500 | `internal_error` | 会话/消息创建或智能体运行失败 |

## 审计与限流

- 每次进入智能体执行的补全(成功或失败、流式或非流式、OpenAI 或 Anthropic 壳)写一条 `gateway.completion` 审计日志,明细含 key 指纹(id+名称,**不含密钥原文**)、会话 ID、解析到的智能体、调用方原始 model 字符串、stream 标志、token 用量、耗时、HTTP 状态码与结束原因(`completed`/`error`/`timeout`/`disconnect`/`disconnect_run_completed`)。`GATEWAY_AUDIT_PAYLOAD=true` 时额外留存截断后的 query 与采样事件流水(见上方 /v1/metrics 一节的排障说明)。
- 并发限制按 key 计数,**每个后端副本独立计数**;多副本部署时全局上限为副本数 × 每副本上限。
- **用量计量(M2-2)**:每轮补全写一行 `gateway_usage`(租户/key/智能体/会话/模型/stream/token/耗时/结束原因),是配额判定的数据源;被 429 拒绝的请求不产生用量行,失败轮同样计量(结束原因 `error`)。计量写失败只告警不阻断已完成的轮。
- 网关临时会话(`kind=gateway`)由后台任务按 `GATEWAY_SESSION_RETENTION` 回收(每小时一轮,单轮最多 200 条);被续接复用的会话每轮滑动保留窗口(`X-WeKnora-Session-ID: new` 或换 user 字段可强制新会话)。

## API key 网关字段(M2-2)

工作空间 API key 管理接口(`agent_grants` / `daily_token_budget` / `daily_request_budget`,均为可选):

- **`agent_grants`**(字符串数组,最多 100 个智能体 ID):空数组 = 不限制(可调工作空间内全部已发布智能体);非空 = 白名单,仅可调用列出的智能体,`/v1/models` 同步过滤,越权调用按 404 `model_not_found` 应答(不泄露白名单)。全量权限 key 同样受 grants 与配额约束(网关以 chat 能力位 + grants + 配额为准)。
- **`daily_token_budget` / `daily_request_budget`**(正整数,可空):按 key 每日(UTC)配额;另有工作空间级 `GATEWAY_TENANT_DAILY_TOKEN_BUDGET` 兜底。命中配额返回 429 `budget_exceeded`,消息内附 UTC 重置时间。配额判定在会话创建之前,拒绝的请求不建会话、不产生用量。
- 配额探测故障时**放行**(fail-open):计量系统不可用不应打断对话。

## 幂等(M2-4)

带 `Idempotency-Key` 头(≤128 字符)的补全请求在 key(无 key 时退化为工作空间)维度去重,窗口 24 小时:

- 首次执行完成后,相同 key + 相同请求体的重放返回**原样响应**并带 `X-WeKnora-Idempotent-Replay: true` 头(状态码、body、SSE 全部一致);
- 相同 key 但**不同请求体** → 409 `idempotency_key_mismatch`;
- 同 key 仍在执行中 → 409 `request_in_progress` + `Retry-After: 2`;
- 执行中断(如 5xx)会释放占位,重试重新执行;不带该头的请求行为完全不变。

## M1/M2 已知限制

- 客户端 `tools` / `function` 定义**不透传**(不执行客户端工具),OpenAI 壳响应 `finish_reason` 恒为 `stop`,Anthropic 壳 `stop_reason` 恒为 `end_turn`;发送了 tools 时在 `meta.client_tools_ignored` 回显数量。双向工具桥在 M3(HITL)实现。
- 无跨请求记忆(除非用 `X-WeKnora-Session-ID` 续接);`temperature` 等采样参数以智能体配置为准(在 `meta.ignored_params` 回显)。
- `usage` 为内部 agent loop 各轮 LLM 调用的**累计值**(含检索、工具轮),通常显著大于客户端按输入长度估算的预期。
- 长任务受 `GATEWAY_REQUEST_TIMEOUT` 约束,超时时流式路径返回已产生的部分内容并在 `meta.warnings` 标注;非流式路径同样返回部分内容。
