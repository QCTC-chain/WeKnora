# 智能体模型网关(Agent-as-Model)M1:OpenAI 兼容端点

状态:M1(G1–G5)已实现,自动化测试全绿;测试计划与执行指引见 `agent-model-gateway-m1-test.md`。　关联:`docs/design/agent-hitl-suspend-resume.md`(M3 依赖)、`docs/design/agent-permission-model.md`、`internal/router/routes_mcp_endpoint.go`(公开集成面先例)

## 0. 摘要

新增 OpenAI 兼容端点 `GET /v1/models` 与 `POST /v1/chat/completions`,把**已发布的智能体**作为一个 model 暴露。外部 agent 客户端(pi / zcode / deepseek / 任何 OpenAI SDK)配置 `base_url + api_key + model` 三元组后,即可把 WeKnora 智能体当作 LLM 使用。

M1 采用三项明确的降级策略,把实现难度压到最低:

1. **无状态临时会话**:每次 completion 请求内部创建临时 session,客户端发来的完整 `messages` 数组渲染成单轮 query 灌入,不做跨请求会话续接。
2. **忽略客户端 tools**:请求中的 `tools` / `function` 定义不透传(响应永远不出现 `finish_reason: "tool_calls"`),M3 再做双向桥。
3. **中间过程折叠**:agent loop 的思考、工具调用、检索过程按配置折叠为正文文本或 `reasoning_content` 增量,只把最终答案作为正式内容输出。

## 1. 背景与目标

WeKnora 的智能体目前只能通过 WeKnora 自己的 Web / API / IM / MCP-server 入口使用。用户希望把内置或自建智能体(含其知识库绑定、工具、技能、系统提示词)直接挂进 pi、zcode、deepseek 等外部 agent 客户端的模型列表里,像切换一个普通模型那样切换到 WeKnora 智能体。

**目标**

- OpenAI Chat Completions 协议(流式 + 非流式)+ `/v1/models` 列表,主流客户端零适配可用。
- `Authorization: Bearer <WeKnora API Key>` 鉴权,与现有 API key 能力位模型兼容。
- 智能体发布开关:未发布的智能体不出现在 `/v1/models`,调用返回 404。
- 中间过程可配置折叠,保证长 agent loop 期间客户端不超时。
- 全链路审计与按 key 并发限流。

**非目标(M1 明确不做)**

- 客户端工具(function calling)透传 → M3。
- 跨请求会话续接(`previous_id` 语义)→ M2。
- Anthropic `/v1/messages` 协议 → M2。
- 按调用计费/配额报表 → M2(仅做并发与频控)。
- Prompt 缓存、semantic caching。

## 2. 现状要点(设计依据)

| 事实 | 位置 | 对设计的影响 |
|---|---|---|
| 服务层问答入口与 HTTP 解耦,已支持非 Web 直调:IM 与 MCP-server 直接调用 QA service,`TurnLeaseHeld=false` 时服务自行取会话租约 | `internal/types/interfaces/session.go:61,77`、`internal/types/qa_request.go:206-231` | 网关作为**第四个直调方**复用 `SessionService.AgentQA`,不复制 handler 流水线 |
| `QARequest.Mode`:空即 unattended,API-key/IM/MCP 调用方不能应答 prompt;`Suspendable=false` 同步等待 | `internal/types/qa_request.go:222-231`、`internal/types/run_mode.go:6-17` | 网关置 `Mode=""(unattended)`、`Suspendable=false`:ask_user / 审批由引擎按无人值守语义自动处理 |
| Agent 事件类型:`thought` / `tool_call` / `tool_result` / `final_answer` / `error` / `references` / `agent_prompt_required` 等 | `internal/event/event.go:53-68` | 事件→OpenAI chunk 翻译表的输入清单 |
| 流式消费端按事件类型累积引用、拼装最终答案 | `internal/handler/session/agent_stream_handler.go:557,665,860` | 网关翻译器照抄其累积语义(引用去重、fallback/truncated 标记) |
| 智能体配置含 AgentMode / SystemPrompt / AllowedTools / KB / MCP / Skills / 沙箱绑定 | `internal/types/custom_agent.go:157-260` | 发布语义只需要加最小字段,能力面已完整 |
| API key 能力位模型:`chat` / `read_agents` / `manage_agents` …;`apiKeyChat()` 已保护 agent-chat 路由 | `internal/types/tenant_api_key.go:78-164`、`internal/router/routes_chat.go:141-152` | `/v1/*` 直接复用 `chat` 能力位,M1 不新增 scope |
| 全局鉴权为三通道:Bearer JWT → `X-API-Key`;Bearer 位置**不接受** API key | `internal/middleware/auth.go:119-122` | pi 等客户端发的是 `Authorization: Bearer <api-key>`,今天会 401 → 网关需自带鉴权中间件 |
| 公开集成面先例:`/mcp/:endpoint_id` 注册在全局 Auth 之前,自带 bearer 鉴权 | `internal/router/routes_mcp_endpoint.go:32-42` | `/v1/*` 照搬该挂载模式,不动全局中间件 |
| Session 有 `Kind` 列(`''` / assistant),建会话时定死 | `internal/types/session.go:98-100` | 临时会话用 `Kind="gateway"`,零迁移,便于按类清理 |
| 引擎上下文窗口 200k 假设,压缩机制已内建 | `internal/types/agent.go:15`、`internal/agent/compaction/` | 客户端超长历史渲染进 query 后仍受压缩保护 |
| HITL 挂起-恢复基础设施(prompt kind + 快照 + Resume) | `docs/design/agent-hitl-suspend-resume.md`、`internal/agent/hitl/` | M1 只依赖"无人值守语义";M3 把外部工具调用注册为新 prompt kind |

## 3. 总体架构

```mermaid
flowchart LR
    subgraph 外部客户端
        PI[pi / zcode / deepseek CLI] -- "POST /v1/chat/completions\nAuthorization: Bearer wk-xxx" --> GW
        PI2 -- "GET /v1/models" --> GW
    end

    subgraph WeKnora
        GW[GatewayAuth 中间件\nAPI key + chat 能力位]
        H[openai_compat.Handler\n协议解析 / 错误封装]
        T[EventTranslator\nevent → OpenAI chunk]
        E[临时会话构造器\nmessages → query 渲染]
        S[SessionService.AgentQA\n(与 Web/IM/MCP 同一入口)]
        AG[Agent 引擎\nReAct / 工具 / KB 检索]
    end

    GW --> H --> E --> S --> AG
    S -- event.EventBus --> T --> PI
```

新增包与职责:

| 组件 | 位置(建议) | 职责 |
|---|---|---|
| 协议类型 | `internal/handler/openai_compat/types.go` | ChatCompletionRequest/Response/Chunk、Error envelope、Model list,字段与 OpenAI 规范对齐 |
| HTTP handler | `internal/handler/openai_compat/handler.go` | `/v1/models`、`/v1/chat/completions`;参数校验、404/401/429 封装 |
| 消息渲染器 | `internal/handler/openai_compat/render.go` | `messages[]` → 单轮 query 文本(+图片链路),见 §7 |
| 事件翻译器 | `internal/handler/openai_compat/translate.go` | 订阅 `event.EventBus`,把内部事件翻译为 SSE chunk,见 §8 |
| 网关鉴权 | `internal/handler/openai_compat/auth.go` | 只认 API key(Bearer 或 `X-API-Key`),要求 `chat` 能力位 |
| 路由注册 | `internal/router/routes_openai_compat.go` | 注册在引擎根 `/v1`,全局 Auth 之前(同 MCP endpoint 模式) |
| 会话回收 | `internal/background` 新任务 | 定期删除过期的 `Kind="gateway"` 会话及其消息/附件 |

## 4. 对外协议契约

### 4.1 GET /v1/models

```json
{
  "object": "list",
  "data": [
    {
      "id": "9b2f…(agent UUID)",
      "object": "model",
      "created": 1730000000,
      "owned_by": "weknora",
      "meta": { "agent_name": "文档问答助手", "agent_mode": "smart-reasoning" }
    }
  ]
}
```

- 仅返回**调用方租户内已发布**的智能体(见 §5)。
- `meta` 为非标准扩展字段,客户端会忽略;`id` 与 `agent_name` 都可作为 completions 的 `model` 值。

### 4.2 POST /v1/chat/completions

请求(OpenAI 标准字段的子集 + 声明的忽略项):

| 字段 | 处理 |
|---|---|
| `model` | 必填。按 UUID 精确匹配 → 按 agent 名称精确匹配(租户内);未命中或未发布 → `404 model_not_found` |
| `messages` | 渲染为单轮 query,见 §7 |
| `stream` | `true` 走 SSE;`false` 聚合后一次性返回 |
| `temperature` / `top_p` / `max_tokens` / `reasoning_effort` | M1 **忽略**(智能体用自己的模型配置);响应 `meta` 中回显"ignored_params"便于排障。文档明示 |
| `tools` / `tool_choice` / `functions` | M1 忽略;收到时在响应 `meta` 回显 `client_tools_ignored: N` |
| `user` | 透传到审计日志,不参与鉴权 |
| `stop` / `n` / `logprobs` 等 | 忽略 |

非流式响应:

```json
{
  "id": "chatcmpl-<uuid>", "object": "chat.completion", "created": 1730000000,
  "model": "<请求中的 model 原样回显>",
  "choices": [{ "index": 0, "message": {"role": "assistant", "content": "最终答案"}, "finish_reason": "stop" }],
  "usage": { "prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0 }
}
```

流式响应(SSE,`Content-Type: text/event-stream`):

```
: keep-alive                                  ← 首个内容产生前每 15s 一次
data: {"id":"chatcmpl-…","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant"}}]}

data: {"…","choices":[{"delta":{"reasoning_content":"正在检索知识库…"}}]}
data: {"…","choices":[{"delta":{"content":"根据文档…"}}]}
data: {"…","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{…}}
data: [DONE]
```

- 首 chunk 先发 `delta.role`;`final_answer` 事件映射为 `delta.content` 增量;`thought` 默认映射为 `delta.reasoning_content`(DeepSeek 风格扩展,pi/zcode/deepseek 客户端可渲染;可配置关闭,见 §10)。
- `usage` 只在最后一个 chunk 携带(与 OpenAI `stream_options.include_usage` 行为对齐;为兼容不支持的客户端,始终携带)。

### 4.3 错误封装

统一 OpenAI error envelope,HTTP 状态码映射:

| 场景 | HTTP | `error.type` | `error.code` |
|---|---|---|---|
| 缺失/无效 API key | 401 | `authentication_error` | `invalid_api_key` |
| key 无 `chat` 能力位 | 403 | `permission_error` | `insufficient_capability` |
| model 未发布/不存在/租户外 | 404 | `invalid_request_error` | `model_not_found` |
| 按 key 并发超限 | 429 | `rate_limit_error` | `concurrency_limit` |
| 频控超限 | 429 | `rate_limit_error` | `rate_limit_exceeded` |
| 请求体不合法(无 messages、超长等) | 400 | `invalid_request_error` | — |
| 内部错误 / agent 运行失败 | 500 | `server_error` | — |

流已开始(`200` 已发出)后的失败:发 `data: {"error":{…}}` 事件,随后 `data: [DONE]` 关流——不发 HTTP 错误码(协议限制)。

### 4.4 自定义响应头

| 头 | 用途 |
|---|---|
| `X-WeKnora-Agent-Id` | 本次命中的智能体 UUID,排障用 |
| `X-WeKnora-Session-Id` | 本次内部临时会话 ID;用户可在 Web 端按此定位留痕会话(M2 会话续接的挂点) |
| `X-WeKnora-Gateway-Version` | 网关协议版本,`1` |

## 5. 智能体发布(M1 最小模型)

- `CustomAgent` 新增两个字段:`Published bool`(默认 false)、`PublishedAt *time.Time`。迁移放 `migrations/`,存量智能体一律视为未发布(包括内置智能体——管理员显式发布后才出现在 `/v1/models`)。
- 发布入口复用现有 `PATCH /agents/:id`(`internal/router/routes_agent.go` 的 `agentsWrite`,Owner/Admin 权限已内置),不新增端点。
- **命名唯一性**:允许 `model` 用可读名称的前提是租户内已发布智能体名称不冲突。发布时校验:已发布集合内名称(TrimSpace 后)重复 → 409,提示改名。未发布态不约束。
- 下架 = `Published=false`:立即从 `/v1/models` 消失;进行中的请求不受影响(已过鉴权与解析),新请求 404。
- 内置智能体发布后同样受调用方租户隔离:跨租户永远 404(不泄露存在性)。

## 6. 鉴权与租户边界

- **GatewayAuth 中间件**(新):只接受两种形态——`Authorization: Bearer <api-key>`(新增支持,外部客户端的标准形态)与 `X-API-Key: <api-key>`(WeKnora 既有形态)。**不尝试 JWT**:该中间件只挂在 `/v1`,不影响全局三通道行为(`internal/middleware/auth.go:119-122`)。
- 复用现有 API key 校验与主体解析(authenticateAPIKeyRequest 的核心逻辑抽出共用),解析出 `(tenant_id, user principal, capabilities)` 写入与现有中间件相同的 context key,下游服务零感知。
- 能力位要求:`GET /v1/models` 与 `POST /v1/chat/completions` 均要求 `chat`(`internal/types/tenant_api_key.go:82`)。M1 不做 per-key agent 白名单(M2)。
- **租户边界**:会话、消息、KB 检索、MCP 调用全部发生在 key 所属租户内;共享智能体(跨租户 share)M1 不暴露,`/v1/models` 只列本租户资源。
- **审计**:每次调用写审计日志(复用 `internal/handler/audit_log.go` 链路):key 指纹、agent、model 原文、prompt/completion token、时长、结束原因。请求正文默认**不入库**(可能含外部代码上下文,量大且敏感);可配置 `gateway.audit_payload=true` 开启采样留存。

## 7. 无状态会话映射

### 7.1 临时会话

- 每次 completion 请求创建 `Session{Kind: "gateway", Title: "gateway:<model>", UserID: <key principal>}`,跑完即留在库中供审计与排障。
- 回收:`internal/background` 增加周期任务,删除 `Kind='gateway'` 且 `updated_at < now-retention` 的会话(级联消息/附件),默认 retention 72h(可配)。
- 同一会话串行:临时会话天然一请求一会话,不触碰 live-run 互斥;`TurnLeaseHeld=false` 由服务侧自行取租约(与 IM/MCP 调用方一致,`internal/types/qa_request.go:206-214`)。

### 7.2 messages → query 渲染

客户端发来的是完整对话历史,WeKnora 临时会话没有这些历史,因此全部折叠进本轮 query:

```
[调用方指令]
<system 消息内容(多条拼接)>

[对话历史]
user: …
assistant: …
(最多保留最近 N=40 条,超出部分以 …(更早 12 条已省略)… 占位;
 总预算 32k 字符,超出按条截断)

[本次问题]
<最后一条 user 消息>
```

规则:

| 消息角色 | 处理 |
|---|---|
| `system` / `developer` | 全部拼入「调用方指令」块;单条上限 8k runes,超出截断并在块尾标注 `(截断)`。这是外部客户端 system prompt 与 WeKnora 智能体系统提示词的**叠加点**:智能体自身 SystemPrompt 仍由引擎注入,调用方指令渲染在 user 侧,不与智能体指令直接拼接、不改变其优先级 |
| `user`(非最后一条) | 进入「对话历史」块,原样文本 |
| `assistant` | 进入「对话历史」块;`reasoning_content` 不回填 |
| `tool` / 带 `tool_calls` 的 `assistant` | 文本化为 `[客户端工具 name] 参数摘要 → 结果摘要(截断 2k 字符)`;M1 不执行任何客户端工具 |
| 最后一条 `user` | 作为 `QARequest.Query`;为空则 400 |
| 多模态(`content` 数组含 `image_url`) | data URL 与 http(s) URL 走既有图片上传链路转 `QARequest.ImageURLs`;智能体 `ImageUploadEnabled=false` 时丢弃图片并在 query 尾部注明 `[图片已忽略:智能体未开启图片能力]`;单请求上限 4 张、单张 10MB |

渲染器为纯函数,输入 messages + 限额,输出 (query, imageURLs, warnings),warnings 并入响应 `meta` 便于客户端自查。

## 8. 事件翻译

翻译器持有一次运行的 `event.EventBus`(与 Web handler 同一订阅方式,`internal/handler/session/agent_stream_handler.go:126`),按表翻译:

| 内部事件(`internal/event/event.go:53-68`) | OpenAI 线上格式 | 配置开关 |
|---|---|---|
| `thought`(增量) | `delta.reasoning_content` 增量 | `thought_mode`: `reasoning`(默认)/ `collapse`(并入 content,加 `<think>` 包裹)/ `drop` |
| `tool_call` / `tool_result` / `progress` / `command_output` | `delta.content` 单行状态文本,如 `[工具] knowledge_search → 3 条结果`;同名工具连续事件合并为一行更新(只发一次) | `progress_mode`: `text`(默认)/ `drop` |
| `references`(累积) | 不即时下发;complete 时按智能体 `citation_enabled` 在正文尾部追加「来源」清单(去重逻辑照抄 `agent_stream_handler.go:557`) | 随智能体配置 |
| `final_answer`(增量) | `delta.content` 增量 | — |
| `agent_prompt_required`(ask_user / 审批 / OAuth) | **不应出现**:网关以 unattended 模式运行(`Mode=""`),引擎按无人值守语义自动处理(审批拒绝并继续、ask_user 由模型自行假设)。若仍收到(防御),折叠为一行说明文本并继续等待终态 | — |
| `error` | `data: {"error":{…}}` + `finish_reason: "stop"` + `[DONE]` | — |
| `complete` | 末 chunk:`finish_reason="stop"` + `usage`;随后 `data: [DONE]` | — |
| `context_compacted` / `memory_recalled` / `session_title` / artifact 类 | 忽略 | — |

**usage 口径**(文档与响应 meta 中声明):`prompt_tokens` / `completion_tokens` 为内部 agent loop 各轮 LLM 调用的**累计值**(含检索、工具轮),通常显著大于客户端按输入长度估算的预期——这是 Agent-as-Model 的固有放大比,M2 在计量报表中显式呈现。

**客户端断连**:`request.Context()` 取消即向上传递(网关不同于 Web UI 的"后台继续跑"),内部运行取消、留痕会话保存已产生内容。`gateway.cancel_on_disconnect=true` 为默认;关掉则跑完(耗时但不浪费已消耗 token)。

## 9. 超时、心跳与并发

| 机制 | 默认 | 说明 |
|---|---|---|
| 心跳 | 15s | 首个 `delta` 前每 15s 输出 `: keep-alive` 注释行,防客户端/中间层空闲超时;产生内容后停止 |
| 请求总时长上限 | 600s(`gateway.request_timeout`) | 到点发一行说明 + `finish_reason="stop"` + `[DONE]`;非流式则 500 前先尽力返回部分文本(实现为聚合器持有已收增量) |
| 每 key 并发运行数 | 3(`gateway.max_concurrency_per_key`) | 信号量实现,超限立即 429(不等排队);复用 `internal/ratelimit` |
| 每 key 请求频率 | 沿用租户级频控 | — |
| LLM 单轮超时 | 沿用智能体配置 | 不在网关层重复设防 |

## 10. 配置项

`internal/config` 新增 `gateway` 段(环境变量 `GATEWAY_*` 映射,沿用现有模式):

| 配置 | 环境变量 | 默认 |
|---|---|---|
| `enabled` | `GATEWAY_ENABLED` | `false`(显式开启) |
| `path_prefix` | `GATEWAY_PATH_PREFIX` | `/v1` |
| `heartbeat_interval` | `GATEWAY_HEARTBEAT_INTERVAL` | `15s` |
| `request_timeout` | `GATEWAY_REQUEST_TIMEOUT` | `600s` |
| `max_concurrency_per_key` | `GATEWAY_MAX_CONCURRENCY_PER_KEY` | `3` |
| `session_retention` | `GATEWAY_SESSION_RETENTION` | `72h` |
| `thought_mode` | `GATEWAY_THOUGHT_MODE` | `reasoning` |
| `progress_mode` | `GATEWAY_PROGRESS_MODE` | `text` |
| `max_history_messages` / `max_history_chars` | — | `40` / `32000` |
| `audit_payload` | `GATEWAY_AUDIT_PAYLOAD` | `false` |
| `cancel_on_disconnect` | `GATEWAY_CANCEL_ON_DISCONNECT` | `true` |

## 11. 改动清单(文件级)

| 动作 | 文件 | 内容 |
|---|---|---|
| 新增 | `internal/handler/openai_compat/` | `types.go` / `handler.go` / `render.go` / `translate.go` / `auth.go` + 对应 `*_test.go` |
| 新增 | `internal/router/routes_openai_compat.go` | `/v1` 注册(引擎根、全局 Auth 之前,同 `routes_mcp_endpoint.go:32-42` 模式)+ 路由表自测 |
| 修改 | `internal/types/custom_agent.go` | `Published` / `PublishedAt` 字段 + 发布校验(名称冲突) |
| 新增 | `migrations/` | published 两列迁移 |
| 修改 | `internal/container/container.go`、`internal/router/router.go` | 装配与注册 |
| 修改 | `internal/config` | gateway 配置段 |
| 新增 | `internal/background` 回收任务 | gateway 会话清理 |
| 修改 | `internal/middleware` | 从 authenticateAPIKeyRequest 抽出可复用的 key 校验函数(不改行为) |
| 修改 | `docs/swagger` | 端点注解(注意 `docs/swagger_contract_test.go` 契约测试) |

## 12. 测试计划

1. **单元**:渲染器 fixtures(system 叠加、历史截断、tool 文本化、多模态降级);翻译器(每类事件 → 期望 chunk 序列);错误映射表。
2. **集成**:`httptest` 全链路——建 key → 发布 agent → `/v1/models` → 流式/非流式 completions → 404/429/断连路径;fake agent 走可控的 mock LLM(参考 `packages/dsh-weknora/test/e2e/fake-model.mjs` 的假模型思路反向使用)。
3. **兼容性矩阵**(人工冒烟,CI 跑 openai-python):openai-python SDK、pi、zcode、deepseek 客户端、LangChain `ChatOpenAI` 各跑「单轮问答 / 多轮带历史 / 长任务(验证心跳)/ 工具定义被忽略」四条用例。
4. **回归**:全局鉴权三通道行为不变(重点:Bearer JWT 仍优先、`X-API-Key` 兼容);Web/IM/MCP 问答链路不感知。

## 13. 里程碑拆解

| 步骤 | 内容 | 预估 | 状态 |
|---|---|---|---|
| G1 | 协议类型 + 鉴权中间件 + `/v1/models`(含 published 字段与迁移) | 2d | ✅ |
| G2 | 渲染器 + 临时会话构造 + `SessionService.AgentQA` 接通(非流式先通) | 3d | ✅ |
| G3 | 翻译器 + SSE 流式 + 心跳/超时/断连 | 3d | ✅ |
| G4 | 并发/频控、审计、会话回收任务、配置段 | 2d | ✅ |
| G5 | 测试(单测/集成/兼容矩阵)+ swagger + 文档 | 3d | ✅(人工兼容矩阵冒烟待部署环境执行,见 §12.3) |

### 13.1 实现落点(G3–G5)

| 交付物 | 位置 |
|---|---|
| SSE 翻译器(事件→chunk、思考/进度折叠、同名工具合并一行、断连/超时语义) | `internal/handler/openai_compat/stream.go` |
| 聚合路径与非流式共享的消息构造/AgentQA 启动器 | `internal/handler/openai_compat/completions.go`(`createTurnMessages`/`launchAgentQA`) |
| 每 key 并发闸(进程内信号量,超限即 429 `concurrency_limit`) | `internal/handler/openai_compat/concurrency.go` |
| 每回合审计(`gateway.completion`,凭据指纹不入原文,`audit_payload` 选择性留存 query) | `internal/handler/openai_compat/audit.go`、`internal/types/audit_log.go` |
| 网关临时会话回收任务(默认 72h,小时级 ticker) | `internal/application/service/gateway_session_reaper.go`、`internal/container/container.go`(`startGatewaySessionReaper`) |
| 配置段(`heartbeat_interval`/`max_concurrency_per_key`/`session_retention`/`thought_mode`/`progress_mode`/`audit_payload`/`cancel_on_disconnect`/`path_prefix` + `GATEWAY_*` 环境变量) | `internal/config/config.go` |
| 流式/审计/并发/回收测试(共 30+ 用例) | `internal/handler/openai_compat/stream_test.go`、`internal/application/service/gateway_session_reaper_test.go` |

与设计的两点实现取舍:

1. **同名工具合并(§8)**:SSE 无法改写已发出的帧,实现为 `tool_call` 先挂起、`tool_result` 落地时合成单行 `[工具] name → 结果摘要`(失败则为 `失败: 原因`);若新工具的 call 插队,先补发上一个的 `调用中` 行。长任务期间客户端靠 keep-alive 注释行确认存活。
2. **断连后残留事件**:客户端断开(cancel_on_disconnect=true)后翻译立即停止,已翻译内容照常落库;取消后仍在途的少量事件不保证捕获(与取消传播固有竞态),审计以落库内容为准。

## 14. M1 已知限制(需写入用户文档)

- 客户端 `tools` 不生效:外部 agent 的本地工具(读文件、执行命令)不会被该"模型"调用,表现为纯文本模型;`finish_reason` 恒为 `stop`。
- 无跨请求记忆:每次调用独立临时会话,WeKnora 侧长期记忆(memory)按其自身会话逻辑不跨请求生效。
- `temperature` 等采样参数以智能体配置为准,请求级覆盖被忽略。
- usage 为内部累计口径,数值大于客户端预估。
- 长任务(agent loop 数分钟)受 `gateway.request_timeout` 上限约束。
