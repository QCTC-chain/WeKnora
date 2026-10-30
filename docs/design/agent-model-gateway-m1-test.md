# 模型网关 M1 测试文档

关联:`docs/design/agent-model-gateway-m1.md`(设计)、`internal/handler/openai_compat/`(实现)、`website-docs/04-api/03-gateway-openai-compat.md`(用户文档)。

状态:M1(G1–G5)已实现,自动化测试全绿;本文档描述**如何验证**——自动化部分可直接重跑,部署冒烟与兼容矩阵部分需一个真实部署环境。

## 1. 测试范围与分层

| 层 | 内容 | 方式 | 状态 |
|---|---|---|---|
| L1 单元/集成 | 协议类型、鉴权、渲染、翻译器、错误映射、并发、审计、回收、配置、路由挂载 | `go test`(仓库内 9 个包、80+ 用例) | ✅ 全绿 |
| L2 部署冒烟 | 真实镜像 + 真实 LLM/库:开关、迁移、发布、建 key、curl 全链路 | 人工,见 §3 | ⬜ 待部署环境 |
| L3 兼容矩阵 | 第三方客户端(openai-python/pi/dsh 等)消费 SSE/错误协议 | 人工,见 §4 | ⬜ 待部署环境 |
| L4 回归 | 全局鉴权三通道、Web/IM/MCP 既有链路 | 自动化已覆盖路由/鉴权;链路级随主套件 | ✅(§5) |

## 2. L1 自动化测试

### 2.1 测试矩阵(按里程碑)

**G1 协议与鉴权**

| 用例 | 位置 | 验证点 |
|---|---|---|
| `TestGatewayAuth*`(11 个) | `internal/middleware/gateway_auth_test.go` | 缺 key/坏 key 401;Bearer 与 `X-API-Key` 双通道;`chat` capability 缺失 403 `insufficient_capability`;full_access 放行;平台 key 必须带 `X-Tenant-ID`;空间 key 不能跨空间;停用空间 403;允许矩阵 |
| `TestListModels*`、`TestResolveAgent` | `internal/handler/openai_compat/handler_test.go` | 仅列 published 智能体;空列表是 `[]` 不是 `null`;UUID 优先、名称别名兜底、未发布/跨空间 404 `model_not_found` |
| `TestSetAgentPublished*`(7 个) | `internal/application/service/custom_agent_publish_test.go` | 发布/取消/幂等;已发布名称唯一冲突 409;同名不算冲突;内置智能体拒发;不存在 404;空名 400 |
| `TestGateway*Route*`(6 个) | `internal/router/routes_openai_compat_test.go` | `/v1/models` 挂载与 API key 鉴权;方法错误 404;**只挂 `/v1` 不污染 `/api/v1`**;`chat/completions` 已挂载;**默认关闭时路由不注册** |

**G2 渲染与非流式**

| 用例 | 位置 | 验证点 |
|---|---|---|
| `TestRenderMessages*`(11 个)+`TestRenderAgentImageAbilityGate` | `render_test.go` | messages 折叠为单轮 query;无 user 消息 400;最后一条 user 覆盖历史;system 每条 8000 字符截断并标注;历史窗口(40 条/32000 字符)占位;tool 消息文本化;图片抽取(≤4 张/≤10MB/仅 https/data:);无图片能力智能体降级并在 warnings 提示;多字节截断不切半个 rune |
| `TestChatCompletionsHappyPath` 等(8 个) | `completions_test.go` | 非流式 envelope(id/object/model/choices/usage);AgentQA 请求语义(`TurnLeaseHeld=false` 等);错误 envelope;引用注记;引用关闭;图片路由到具备能力的智能体;客户端 tools 回显进 `meta.client_tools_ignored` |

**G3 流式与翻译器**

| 用例 | 位置 | 验证点 |
|---|---|---|
| `TestChatCompletionsStreamHappyPath` | `stream_test.go` | SSE 帧序(`chat.completion.chunk`→`[DONE]`),正文含引用注记,末 chunk 带 usage+meta |
| `TestChatCompletionsStreamThoughtModes` | `stream_test.go` | thought_mode=reasoning(`delta.reasoning_content`)/collapse(`<think>` 进正文)/drop 三种模式 |
| `TestChatCompletionsStreamToolProgressAndMerge` | `stream_test.go` | tool_call/tool_result 合并为单行 `[工具] name → 结果摘要`(设计 §8);Progress 事件文本行 |
| `TestChatCompletionsStreamProgressDrop` | `stream_test.go` | progress_mode=drop 时无进度行 |
| `TestChatCompletionsStreamAgentError` | `stream_test.go` | 中途失败发 `data: {"error":…}` 帧后仍以 `[DONE]` 收尾,HTTP 200 |
| `TestChatCompletionsStreamDisconnectPersistsPartial` | `stream_test.go` | 客户端断连(取消 ctx)后已生成部分内容照常落库,结束原因 `disconnect` |
| `TestChatCompletionsStreamNoCancelOnDisconnect` | `stream_test.go` | cancel_on_disconnect=false 时断连不取消内部运行(WithoutCancel) |
| `TestChatCompletionsStreamTimeoutNote` | `stream_test.go` | 超时输出部分内容 + `[网关] 响应超时` 注记 + meta.warnings |
| `TestSSEHeartbeatBeforeFirstContent` | `stream_test.go` | 首个内容前按 heartbeat_interval 输出 `: keep-alive` 注释行 |

**G4 并发/审计/回收/配置**

| 用例 | 位置 | 验证点 |
|---|---|---|
| `TestPerKeyConcurrencyLimit`、`TestPerKeyConcurrencyIndependentKeysAndUnlimited` | `stream_test.go` | 超 `max_concurrency_per_key` 立即 429 `concurrency_limit`(任何行创建之前);不同 key 互不影响;0=不限制 |
| `TestGatewayAuditPerTurn`、`TestGatewayAuditPayloadOptInAndFailureTolerated` | `stream_test.go` | 每回合一行 `gateway.completion` 审计(key 指纹/agent/stream/end_reason/用量);query 仅 `audit_payload=true` 留存(截断 2000);审计失败不 500 |
| `TestGatewaySessionReaper*`(4 个) | `internal/application/service/gateway_session_reaper_test.go` | 按 retention 删过期 gateway 会话及其消息;单条消息删除失败跳过该会话不中断;retention 非正值回退 72h;空轮次 noop |
| `TestApplyGatewayEnvOverrides*`、`TestGatewayRequestTimeoutOverride` | `internal/config/gateway_env_test.go` | 全部 `GATEWAY_*` 环境变量覆盖与默认值;无 Gateway 段时自动创建;duration 与纯秒数两种格式 |

### 2.2 运行方式

标准环境(有 Go 工具链/CI):

```bash
go build ./...
go vet ./internal/...
go test -count=1 ./internal/... ./docs/...
# 网关重点包(秒级):
go test -count=1 ./internal/handler/openai_compat/ ./internal/router/ ./internal/middleware/ ./internal/config/
# 竞态检测(流式/并发涉及 goroutine):
go test -race -count=1 ./internal/handler/openai_compat/
```

本仓库宿主机无 Go 工具链,通过 `gobuild` 容器等价执行(docker):
`docker exec -w /app gobuild go test ...`,参数同上。

**当前全绿基线**(提交时):`openai_compat`、`container`(含 `TestRouterWiring` 双模式)、`router`、`middleware`、`config`、`database`(27.7s,含 sqlite 版本化 schema 对照)、`types`、`event`、`handler`、`handler/session`、`application/service`(166s)、`docs`(swagger 契约)全部 `ok`;`-race` 通过;`go.mod/go.sum` 零改动。

## 3. L2 部署冒烟(人工,一次性)

前置:一个能登录的管理员账号;`docker compose` 可用。

### 3.1 部署

```bash
cd <仓库根目录>
# 开启网关(compose env_file 会把 .env 透传进 app 容器)
grep -q '^GATEWAY_ENABLED=' .env && sed -i 's/^GATEWAY_ENABLED=.*/GATEWAY_ENABLED=true/' .env || echo 'GATEWAY_ENABLED=true' >> .env
docker compose build app && docker compose up -d app
docker compose ps app   # 等待 healthy;迁移 000135(postgres)/000052(sqlite)启动时自动执行
```

### 3.2 数据准备

```bash
BASE=http://localhost:8080
TOKEN=$(curl -s $BASE/api/v1/auth/login -H 'Content-Type: application/json' \
  -d '{"email":"you@example.com","password":"..."}' | python3 -c 'import sys,json;print(json.load(sys.stdin)["data"]["token"])')

# 发布智能体(名称需在已发布集合内唯一,冲突 409)
curl -s -X PATCH $BASE/api/v1/agents/<AGENT_ID> -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"published":true}'

# 建 API key(必须含 chat 能力;token 只出现一次)
curl -s -X POST $BASE/api/v1/tenants/<TENANT_ID>/api-keys -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"name":"gateway-test","capabilities":["chat"]}'
```

### 3.3 冒烟清单

| # | 操作 | 预期 | 覆盖 |
|---|---|---|---|
| S1 | `curl -s $BASE/v1/models`(不带 key) | 401 `invalid_api_key`(若 404 → 开关未生效/镜像未更新,见 §7 排查) | G1 开关+鉴权 |
| S2 | `GET /v1/models -H "Authorization: Bearer $KEY"` | `data[]` 每项含 id/meta.agent_name/meta.agent_mode,`owned_by:"weknora"` | G1 |
| S3 | 同上但用无 `chat` 能力的 key | 403 `insufficient_capability` | G1 |
| S4 | 非流式 completions(`model` 用智能体中文名) | `choices[0].message.content` 带回答(知识库场景含引用注记);`usage.total_tokens`>0;`meta.ignored_params` 含 temperature 等 | G2 |
| S5 | 非流式,`model` 用 UUID | 同 S4(别名与 UUID 双通道) | G1/G2 |
| S6 | 流式 `stream:true` | `text/event-stream`;内容前有 `: keep-alive` 行(慢回答可观察);chunk 帧语法正确;`data: [DONE]` 收尾;末 chunk 含 `usage`+`meta` | G3 |
| S7 | 流式中 Ctrl-C 断开 | `messages` 表对应 assistant 行已落库部分内容;`audit_log` 有 `gateway.completion` 行,end_reason=`disconnect` | G3/G4 |
| S8 | 思考型模型 + `GATEWAY_THOUGHT_MODE=collapse` 重启 | 正文出现 `<think>…</think>`;`reasoning` 模式改出 `delta.reasoning_content` | G3 |
| S9 | 同 key 并发 4 个请求 | 前 3 个 200/流式中,第 4 个立即 429 `concurrency_limit`(响应体 JSON error envelope) | G4 |
| S10 | 不存在的 model | 404 `model_not_found` | G1/G2 |
| S11 | 请求体畸形(无 messages) | 400 | G2 |
| S12 | `GATEWAY_AUDIT_PAYLOAD=true` 重启后跑一轮 | `audit_log.details` 含截断 query;默认(未开)时不含 | G4 |
| S13 | 等 `GATEWAY_SESSION_RETENTION`(测试可调成 `1m`)过 2 个回收周期 | `sessions` 表 `kind='gateway'` 过期行及 messages 被清理,普通会话不受影响 | G4 |
| S14 | 响应头检查(流式响应) | `X-WeKnora-Agent-Id`/`X-WeKnora-Session-Id`/`X-WeKnora-Gateway-Version` | G3 |

### 3.4 数据库核对 SQL(postgres)

```sql
-- 网关临时会话(应随回收消失)
SELECT id, kind, updated_at FROM sessions WHERE kind='gateway' ORDER BY updated_at DESC LIMIT 5;
-- 网关审计
SELECT id, outcome, details, created_at FROM audit_logs WHERE action='gateway.completion' ORDER BY created_at DESC LIMIT 5;
```

## 4. L3 兼容性矩阵(人工冒烟)

设计 §12.3 的矩阵。每个客户端跑四条用例:**单轮问答 / 多轮带历史(messages 数组折叠)/ 长任务(观察心跳与部分内容)/ 带工具定义(应被忽略并回显)**。

| 客户端 | 接入参数 | 预期可验证点 | 备注 |
|---|---|---|---|
| openai-python SDK | `base_url="http://host:8080/v1"`, `api_key=sk-...` | chunk 解析、`[DONE]`、usage、`meta` 附加字段;`reasoning_content` | 首选,自动化程度最高 |
| curl / httpie | 见 §3.3 | 全部协议细节 | 基线 |
| Cherry Studio / ChatBox 等 GUI | 自定义 OpenAI provider | 思考折叠、进度行观感 | 纯文本客户端,最适配 M1 |
| dsh(DeepSeek Harness) | OpenAI 兼容 provider 指向 `/v1` | SSE 消费、reasoning_content、工具进度行 | 见 §4.1 限制 |
| pi / zcode | 同上 | 同上 | harness 形态,tools 限制同 dsh |

### 4.1 dsh 专项说明

dsh 是编码 harness,靠 `tool_calls` 驱动文件/命令操作。**M1 网关不透传客户端 tools、`finish_reason` 恒为 `stop`**,因此 dsh 只能把它当纯文本聊天模型:流式输出、思考内容、工具进度行均可正常消费,但 agent loop 无法执行工具,若 dsh 强校验 tools 支持可能报错或退化。完整的双向工具桥在 M3(HITL)实现;另注意 `packages/dsh-weknora` 插件走原生 `/api/v1` 检索接口,与网关无关。

## 5. L4 回归确认

自动化已覆盖:全局鉴权中间件行为不变(`internal/middleware/auth_test.go` 等随主套件)、`/v1` 不污染 `/api/v1`(`TestGatewayRouteMountsExactlyAtV1`)、网关默认关闭时零影响(`TestGatewayConfigDisabledByDefault`)。链路级建议部署后抽查:Web 端一次知识库问答、一次 MCP 端点调用结果正常。

## 6. 配置速查(测试调参用)

| 环境变量 | 默认 | 冒烛建议 |
|---|---|---|
| `GATEWAY_ENABLED` | `false` | 必开 |
| `GATEWAY_REQUEST_TIMEOUT` | `600s` | 超时用例可调成 `10s` |
| `GATEWAY_HEARTBEAT_INTERVAL` | `15s` | 调成 `2s` 观察 S6 |
| `GATEWAY_MAX_CONCURRENCY_PER_KEY` | `3` | S9 用默认即可 |
| `GATEWAY_SESSION_RETENTION` | `72h` | S13 调成 `1m` |
| `GATEWAY_THOUGHT_MODE` | `reasoning` | S8 切换三档 |
| `GATEWAY_PROGRESS_MODE` | `text` | `drop` 对照 |
| `GATEWAY_AUDIT_PAYLOAD` | `false` | S12 打开 |
| `GATEWAY_CANCEL_ON_DISCONNECT` | `true` | false 时断连后回答跑完并落库 |

## 7. 排查速查

| 症状 | 判据 | 处置 |
|---|---|---|
| `/v1/models` 返回 404 | 路由未注册 | 确认 `GATEWAY_ENABLED=true` 已进容器(`docker exec WeKnora-app env \| grep GATEWAY`)且镜像是重建后的(`docker inspect WeKnora-app --format '{{.Image}}'` 与 build 输出对比) |
| 401 但 key 正确 | 鉴权失败 | key 是否带 `chat` 能力;平台 key 需 `X-Tenant-ID` |
| 流式无 keep-alive | 心跳仅首个内容前输出 | 首内容来得快属正常;调小 heartbeat_interval 观察 |
| usage 远大于预期 | 内部 agent loop 各轮累计口径 | 属预期(设计 §14),非缺陷 |
| 429 误伤 | 每 key 每副本独立计数 | 多副本部署时全局上限=副本数×配置值 |
