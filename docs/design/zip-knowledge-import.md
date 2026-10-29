# ZIP 知识包导入（Markdown + images）

## 概述

支持将导出型 zip 包（Markdown 文档 + 同目录 `images/` 图片）导入知识库：上传时展开、按文档建知识、图片入库并把引用重写为存储地址（rewrite-at-upload）；下载导出时反向恢复 `images/` 目录布局。

## 五个确认点的落地

1. **缺失图片**：zip 中找不到的图片引用**保留原相对路径**，并在导入报告 `warnings` 中说明（`图片 xxx 未在压缩包中找到，已保留原路径`）。
2. **重写后的 Markdown 即知识源文件**：入库的 md 内容就是重写后的版本；解析/分块/图库链路零改动，重复解析幂等（已是存储地址的引用不会被再次改写）。
3. **导出恢复目录布局**：单文档/批量下载 zip 时，`resource://` 引用改写回 `images/<name>` 相对路径，图片字节写入 `<folder>/images/`，重新导入可完整复原。无法解析的引用原样保留 `resource://`。
4. **不支持的文件直接跳过**：zip 内不支持的扩展名不建知识，进入报告 `skipped` 列表（原因：不支持的文件类型，已忽略）。
5. **同目录多篇 Markdown**：每篇 md 独立成知识；图片按内容 sha256 去重共享。

## 分阶段范围

- **P0 上传导入**：`POST knowledge` 收到 zip → `archive.Open` 安全展开 → `BuildManifest` → 每篇 md `importArchiveDocument`（存图片、重写引用、入原有解析队列）；响应 `{type:"archive", items, skipped}` 报告。zip 白名单仅在上传分支生效（`utils.IsZipArchiveExtension`），不影响解析引擎类型表。
- **P1 下载导出**：`GetKnowledgeFileForExport` 解析 `resource://` 引用与字节（限额内），下载处理器重写回相对路径并写入图片。
- **P2 混合与 URL 导入**：
  - zip 内 pdf/xlsx/txt 等受支持类型走纯文件直导管道（不处理图片，无引用改写）。
  - 引用式图片 `![alt][label]` + `[label]: images/x.png`：仅改写定义行（一次改写覆盖全部用法），CommonMark 标签匹配（大小写不敏感、空白折叠、首个定义生效），代码块内不处理。
  - 文件 URL 指向 zip（`CreateKnowledgeFromURLWithReport`）：同步 SSRF 安全下载 → 展开导入 → 返回归档报告；重新导入/替换流程不支持 zip（明确 400 提示）。

## 限额（默认值，均可用环境变量调整或代码常量收紧）

| 项 | 限制 | 来源 |
| --- | --- | --- |
| zip 体积 | 200 MB | `ARCHIVE_MAX_SIZE_MB`（nginx `client_max_body_size` 需同步） |
| 常规文件数 | 5000（仅计入实际加载的文件；`__MACOSX`、链接、重复等被跳过的条目不占额度） | `ARCHIVE_MAX_ENTRIES` |
| 展开总量 | 512 MiB | `archive.Limits` |
| 条目数 / 单条目 | 5000 / 100 MiB | `archive.Limits` |
| md 数量 / 单篇图片数 | 200 / 1000（超出保留原路径+警告） | manifest / `maxArchiveDocumentImages` |
| 导出 | md 64 MiB、图片 100 MiB、1000 张，超限原样流式 | `knowledge_export.go` |
| 下载包合计 | 512 MiB | 下载处理器 |

路径安全：拒绝路径穿越、符号链接条目、GBK/CP437 文件名恢复、zip-slip 防护。

## 部署注意（nginx）

前端直传 zip 经 nginx 反代，`client_max_body_size` 必须 ≥ `ARCHIVE_MAX_SIZE_MB`（默认 200 MB），否则大包会被 nginx 以 413 拒绝，后端限额形同虚设。示例：

```nginx
client_max_body_size 210m;   # ≥ ARCHIVE_MAX_SIZE_MB，留少量余量给 multipart 开销
```

## 测试证据

- `internal/infrastructure/archive`：安全展开、名称恢复、manifest 组装（含跳过/附件）。
- `internal/infrastructure/docparser`：相对/HTML/引用式图片的收集与幂等重写。
- `internal/application/service`：rewrite-at-upload、缺失图片警告、附件直导、zip URL 展开、**真实语料集成**（`archive_corpus_integration_test.go`：41 打包图片、29 处引用全部改写、0 丢失、0 警告、1 个解析任务）。
- `internal/handler`：下载 zip 恢复 `images/` 布局、多文件夹同名图片隔离、未解析引用保留。
- 前端：上传白名单放行 zip、报告归类（`classifyUploadResult`）、行内汇总、locale 六语言键审计。
