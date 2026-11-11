#!/usr/bin/env python3
"""导出知识库预生成问题及其答案为 JSONL。

功能:
1. 读取指定知识库(通过知识库 ID)
2. 获取该知识库内分块(chunk)上 AI 预生成的问题
3. 通过 WeKnora 会话/知识问答接口为每个问题生成答案
4. 将问题、答案、引用文档、引用块和思考过程等数据记录为 JSONL 文件

仅依赖 Python 标准库。

用法示例:
    export WEKNORA_BASE_URL=http://localhost:8080
    export WEKNORA_API_KEY=sk-xxxx          # 或 export WEKNORA_JWT=<登录token>
    export WEKNORA_AGENT_ID=ag-xxxx
    python3 scripts/export_kb_generated_qa.py --kb-id <知识库ID>
    python3 scripts/export_kb_generated_qa.py --kb-id <id> --output out.jsonl --limit 20 -j 4 --retry-failed
    python3 scripts/export_kb_generated_qa.py --kb-id <id> --shard-size 20000 -j 8 --progress-every 100
                                            # 几十万级问题:分片输出(每片 2 万条),多线程,
                                            # 降频打印进度;断点续跑自动读取所有分片

对应后端接口(见 internal/router/routes_knowledge.go / routes_chat.go):
    GET  /api/v1/knowledge-bases/{kb_id}            知识库详情
    GET  /api/v1/knowledge-bases/{kb_id}/knowledge  知识库下文档列表(分页)
    GET  /api/v1/chunks/{knowledge_id}              文档分块列表(分页),
                                                   预生成问题位于 chunk.metadata.generated_questions
    POST /api/v1/sessions                           创建会话
    POST /api/v1/knowledge-chat/{session_id}        知识问答(SSE 流式)
"""

import argparse
import json
import os
import ssl
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from concurrent.futures import ThreadPoolExecutor, as_completed
from pathlib import Path

DEFAULT_BASE_URL = "http://localhost:8080"
PAGE_SIZE = 100
SSE_POLL_TIMEOUT = 300  # 单个问题 SSE 流的最长等待时间(秒)
SSE_IDLE_TIMEOUT = 120  # SSE 流无新数据的最长等待时间(秒)
# 服务端输出预算耗尽且无正文的回退文案(chat_completion_stream.go: EmptyTruncatedAnswerFallback)
EmptyAnswerBudgetMarker = "per-response output limit"


class APIError(Exception):
    """WeKnora API 调用失败。"""


class Client:
    """极简 WeKnora API 客户端(标准库实现)。"""

    def __init__(self, base_url, api_key=None, token=None, timeout=60, verify=True):
        self.base = base_url.rstrip("/")
        self.timeout = timeout
        self.verify = verify
        self.headers = {"Content-Type": "application/json", "Accept": "application/json"}
        if api_key:
            self.headers["X-API-Key"] = api_key
        elif token:
            self.headers["Authorization"] = f"Bearer {token}"

    def _request(self, method, path, body=None, stream=False):
        url = f"{self.base}{path}"
        data = json.dumps(body, ensure_ascii=False).encode("utf-8") if body is not None else None
        req = urllib.request.Request(url, data=data, method=method, headers=self.headers)
        ctx = None
        if url.startswith("https"):
            ctx = ssl.create_default_context()
            if not self.verify:
                ctx.check_hostname = False
                ctx.verify_mode = ssl.CERT_NONE
        try:
            resp = urllib.request.urlopen(req, timeout=self.timeout, context=ctx)
        except urllib.error.HTTPError as e:
            detail = ""
            try:
                detail = e.read().decode("utf-8", "replace")[:500]
            except Exception:
                pass
            raise APIError(f"{method} {path} -> HTTP {e.code}: {detail}") from e
        except urllib.error.URLError as e:
            raise APIError(f"{method} {path} 连接失败: {e.reason}") from e
        if stream:
            return resp
        payload = resp.read().decode("utf-8", "replace")
        if not payload:
            return {}
        try:
            return json.loads(payload)
        except json.JSONDecodeError:
            return {"raw": payload}

    # ---- 业务接口 ----

    def get_knowledge_base(self, kb_id):
        return self._request("GET", f"/api/v1/knowledge-bases/{kb_id}")

    def list_knowledges(self, kb_id, page=1, page_size=PAGE_SIZE):
        qs = urllib.parse.urlencode({"page": page, "page_size": page_size})
        return self._request("GET", f"/api/v1/knowledge-bases/{kb_id}/knowledge?{qs}")

    def list_chunks(self, knowledge_id, page=1, page_size=PAGE_SIZE):
        qs = urllib.parse.urlencode({"page": page, "page_size": page_size})
        return self._request("GET", f"/api/v1/chunks/{knowledge_id}?{qs}")

    def create_session(self, title="kb-qa-export"):
        return self._request(
            "POST", "/api/v1/sessions",
            body={"title": title[:100], "description": "Generated QA export"},
        )

    def knowledge_chat_stream(self, session_id, query, kb_id, knowledge_id=None, agent_id=None):
        """发起知识问答并返回原始 SSE 响应对象。"""
        body = {
            "query": query,
            "knowledge_base_ids": [kb_id],
            "channel": "api",
            "disable_title": True,
        }
        # 指定智能体:其 max_completion_tokens / thinking 等配置会覆盖
        # config.yaml 里 conversation.summary.max_completion_tokens(默认 1024)
        if agent_id:
            body["agent_id"] = agent_id
        # 携带问题来源,便于后端将检索对齐到来源文档(与前端点击预生成问题行为一致)
        if knowledge_id:
            body["question_origin"] = {
                "knowledge_base_id": kb_id,
                "knowledge_id": knowledge_id,
            }
        path = f"/api/v1/knowledge-chat/{urllib.parse.quote(session_id, safe='')}"
        url = f"{self.base}{path}"
        data = json.dumps(body, ensure_ascii=False).encode("utf-8")
        req = urllib.request.Request(url, data=data, method="POST", headers=self.headers)
        ctx = None
        if url.startswith("https") and not self.verify:
            ctx = ssl._create_unverified_context()
        try:
            resp = urllib.request.urlopen(req, timeout=self.timeout, context=ctx)
        except urllib.error.HTTPError as e:
            detail = ""
            try:
                detail = e.read().decode("utf-8", "replace")[:500]
            except Exception:
                pass
            raise APIError(f"POST {path} -> HTTP {e.code}: {detail}") from e
        if resp.headers.get_content_type() != "text/event-stream":
            payload = resp.read().decode("utf-8", "replace")
            raise APIError(f"POST {path} 未返回 SSE 流: {payload[:300]}")
        return resp


def parse_sse_events(resp):
    """逐行解析 SSE,产出 (event_name, data_dict) 元组。"""
    event_name = "message"
    data_lines = []
    for raw in resp:
        line = raw.decode("utf-8", "replace").rstrip("\r\n")
        if line == "":
            if data_lines:
                data = "\n".join(data_lines)
                try:
                    payload = json.loads(data)
                except json.JSONDecodeError:
                    payload = {"content": data}
                yield event_name, payload
            event_name, data_lines = "message", []
            continue
        if line.startswith("event:"):
            event_name = line[len("event:"):].strip()
        elif line.startswith("data:"):
            data_lines.append(line[len("data:"):].strip())


def ask_question(client, query, kb_id, knowledge_id=None, agent_id=None):
    """为单个问题生成答案(每个问题使用独立会话,避免上下文串扰)。

    返回 (结果dict, 会话ID)。结果包含 answer / thinking / references 等字段。
    """
    session = client.create_session(title=query[:50])
    session_id = ((session.get("data") or {}).get("id")
                  or (session.get("data") or {}).get("ID"))
    if not session_id:
        raise APIError(f"创建会话失败: {json.dumps(session, ensure_ascii=False)[:300]}")

    answer_parts, thinking_parts, references = [], [], []
    assistant_message_id, usage, error_msg = "", None, None

    resp = client.knowledge_chat_stream(session_id, query, kb_id, knowledge_id, agent_id)
    start = time.time()
    last_data = start
    try:
        for event_name, payload in parse_sse_events(resp):
            now = time.time()
            if now - start > SSE_POLL_TIMEOUT:
                error_msg = "SSE 流超时"
                break
            if event_name != "message":
                continue
            resp_type = payload.get("response_type")
            if resp_type == "heartbeat":
                last_data = now
                continue
            if now - last_data > SSE_IDLE_TIMEOUT:
                error_msg = "SSE 流空闲超时"
                break
            last_data = now

            if resp_type == "answer":
                answer_parts.append(payload.get("content") or "")
                if payload.get("assistant_message_id"):
                    assistant_message_id = payload["assistant_message_id"]
            elif resp_type == "thinking":
                thinking_parts.append(payload.get("content") or "")
            elif resp_type == "references":
                refs = payload.get("knowledge_references") or []
                references.extend(refs)
            elif resp_type == "session_title":
                continue
            elif resp_type == "error":
                error_msg = payload.get("content") or "服务端返回错误"
            elif resp_type == "complete":
                if payload.get("usage"):
                    usage = payload["usage"]
                break
            else:
                # progress / tool_call 等事件仅记录 message_id
                if payload.get("assistant_message_id"):
                    assistant_message_id = payload["assistant_message_id"]
    except (urllib.error.URLError, OSError) as e:
        # 流中断时保留已接收的部分结果
        error_msg = f"SSE 流中断: {e}"

    try:
        resp.close()
    except Exception:
        pass

    return {
        "answer": "".join(answer_parts).strip(),
        "thinking": "".join(thinking_parts).strip(),
        "references": references,
        "assistant_message_id": assistant_message_id,
        "usage": usage,
        "error": error_msg,
    }, session_id


def iter_generated_questions(client, kb_id, max_docs=None, doc_filter=None):
    """遍历知识库下所有文档的分块,产出 (knowledge, chunk, question_dict)。

    knowledge/chunk 均为瘦身后的最小字段字典(几十万级问题时避免内存膨胀):
    knowledge = {id, title};chunk = {id, chunk_index, content(截断至200字符)}
    """
    page, seen_docs = 1, 0
    while True:
        payload = client.list_knowledges(kb_id, page=page, page_size=PAGE_SIZE)
        if not payload.get("success"):
            raise APIError(f"获取文档列表失败: {json.dumps(payload, ensure_ascii=False)[:300]}")
        knowledges = payload.get("data") or []
        if not knowledges:
            break
        for kb_item in knowledges:
            kid = kb_item.get("id") or kb_item.get("ID")
            if doc_filter and kid not in doc_filter:
                continue
            seen_docs += 1
            if max_docs and seen_docs > max_docs:
                return
            slim_knowledge = {
                "id": kid,
                "title": kb_item.get("title") or kb_item.get("Title"),
            }
            yield from iter_chunk_questions(client, kid, slim_knowledge)
        total = payload.get("total") or 0
        if page * PAGE_SIZE >= total:
            break
        page += 1


def iter_chunk_questions(client, knowledge_id, knowledge):
    """遍历单个文档的分块,产出 (knowledge, chunk, question_dict)。chunk 瘦身。"""
    page = 1
    while True:
        payload = client.list_chunks(knowledge_id, page=page, page_size=PAGE_SIZE)
        if not payload.get("success"):
            print(f"  ! 获取分块失败 knowledge_id={knowledge_id}: "
                  f"{json.dumps(payload, ensure_ascii=False)[:200]}", file=sys.stderr)
            return
        chunks = payload.get("data") or []
        if not chunks:
            return
        for chunk in chunks:
            questions = extract_questions(chunk)
            if not questions:
                continue
            slim_chunk = {
                "id": chunk.get("id"),
                "chunk_index": chunk.get("chunk_index"),
                "content": (chunk.get("content") or "")[:200],
            }
            for q in questions:
                yield knowledge, slim_chunk, q
        total = payload.get("total") or 0
        if page * PAGE_SIZE >= total:
            return
        page += 1


def extract_questions(chunk):
    """从分块 metadata 中提取预生成问题,兼容旧格式(字符串数组)与新格式(对象数组)。"""
    metadata = chunk.get("metadata") or {}
    if isinstance(metadata, str):
        try:
            metadata = json.loads(metadata)
        except json.JSONDecodeError:
            return []
    questions = metadata.get("generated_questions") or []
    out = []
    for i, q in enumerate(questions):
        if isinstance(q, str):
            out.append({"id": f"legacy-{chunk.get('id', i)}-{i}", "question": q})
        elif isinstance(q, dict) and q.get("question"):
            out.append({"id": q.get("id") or f"q-{chunk.get('id', i)}-{i}", "question": q["question"]})
    return out


def record_is_failed(rec):
    """判断一条历史记录是否失败:有错误、答案为空或为输出预算耗尽的回退文案。"""
    answer = rec.get("answer") or ""
    return bool(rec.get("error")) or not answer.strip() or EmptyAnswerBudgetMarker in answer


def count_lines(path):
    """统计文件中非空行数(用于分片轮转判断)。"""
    total = 0
    with path.open("r", encoding="utf-8", errors="replace") as f:
        for line in f:
            if line.strip():
                total += 1
    return total


def existing_output_files(output_path, shard_size):
    """列出当前的输出文件:单文件模式返回 base;分片模式返回所有 part 文件(+遗留 base)。"""
    files = []
    if shard_size > 0:
        pattern = f"{output_path.stem}.part*{output_path.suffix}"
        files = sorted(output_path.parent.glob(pattern))
        if output_path.exists():
            files.insert(0, output_path)  # 遗留单文件也算入续跑读取范围
    elif output_path.exists():
        files = [output_path]
    return files


class ShardedJsonlWriter:
    """按记录数分片的 JSONL 追加写入器;shard_size<=0 时写单个文件。

    - 每写一条立即 flush(保证断点续跑完整性;写入本身微秒级,不是瓶颈)
    - 达到 shard_size 后自动关闭当前分片、打开下一个 part 文件
    """

    def __init__(self, base_path, shard_size):
        self.base = base_path
        self.shard_size = max(0, shard_size)
        self._fh = None
        self._count = 0
        if self.shard_size <= 0:
            self._current = base_path
        else:
            self._current = self._next_shard_path()

    def _shard_file(self, n):
        return self.base.with_name(f"{self.base.stem}.part{n:04d}{self.base.suffix}")

    def _next_shard_path(self):
        """确定下一个要写的分片:有未满的旧分片则续写,否则开新分片。"""
        existing = sorted(self.base.parent.glob(
            f"{self.base.stem}.part*{self.base.suffix}"))
        if existing:
            last = existing[-1]
            try:
                n = int(last.stem.rsplit(".part", 1)[1]) + 1
            except ValueError:
                n = len(existing) + 1
            filled = count_lines(last)
            if filled < self.shard_size:
                self._count = filled
                return last
        self._count = 0
        return self._shard_file(n if existing else 1)

    def _ensure_open(self):
        if self._fh is None:
            self._current.parent.mkdir(parents=True, exist_ok=True)
            self._fh = self._current.open("a", encoding="utf-8")

    def write(self, line):
        self._ensure_open()
        self._fh.write(line)
        self._fh.flush()
        self._count += 1
        if self.shard_size > 0 and self._count >= self.shard_size:
            self._fh.close()
            self._fh = None
            n = int(self._current.stem.rsplit(".part", 1)[1]) + 1
            self._current = self._shard_file(n)
            self._count = 0

    def close(self):
        if self._fh is not None:
            self._fh.close()
            self._fh = None


def load_done_keys(files, include_failed=False):
    """读取输出文件(含所有分片),返回 (已完成key集合, 失败key集合, 残缺行数)。

    include_failed=True 时失败记录不计入已完成(供 --retry-failed 重试)。
    """
    done, failed, corrupt = set(), set(), 0
    for path in files:
        with path.open("r", encoding="utf-8", errors="replace") as f:
            for line in f:
                line = line.strip()
                if not line:
                    continue
                try:
                    rec = json.loads(line)
                except json.JSONDecodeError:
                    corrupt += 1  # 中断产生的残缺行
                    continue
                key = (rec.get("chunk_id"), rec.get("question"))
                if record_is_failed(rec):
                    failed.add(key)
                    if include_failed:
                        continue
                done.add(key)
    return done, failed, corrupt


def purge_bad_lines(files, failed_keys=None):
    """原子地清理各输出文件:移除残缺行;failed_keys 非空时一并移除旧失败记录。"""
    dropped_failed = dropped_corrupt = 0
    for path in files:
        tmp = path.with_suffix(path.suffix + ".tmp")
        with path.open("r", encoding="utf-8", errors="replace") as src, \
                tmp.open("w", encoding="utf-8") as dst:
            for line in src:
                stripped = line.strip()
                try:
                    rec = json.loads(stripped)
                except json.JSONDecodeError:
                    dropped_corrupt += 1
                    continue  # 残缺行丢弃,对应问题会重新生成
                if failed_keys and (rec.get("chunk_id"), rec.get("question")) in failed_keys:
                    dropped_failed += 1
                    continue
                dst.write(line)
        os.replace(tmp, path)
    return dropped_failed, dropped_corrupt


def build_record(kb, knowledge, chunk, q, result, session_id, duration_ms):
    """组装单条 JSONL 记录:问题、答案、引用文档、引用块、思考过程等。"""
    references = result.get("references") or []
    # 按 knowledge_id 去重得到引用文档列表
    cited_docs, seen = [], set()
    for ref in references:
        kid = ref.get("knowledge_id")
        if kid and kid not in seen:
            seen.add(kid)
            cited_docs.append({
                "knowledge_id": kid,
                "title": ref.get("knowledge_title"),
                "filename": ref.get("knowledge_filename"),
                "source": ref.get("knowledge_source"),
                "description": ref.get("knowledge_description"),
            })
    return {
        "kb_id": kb.get("id") or kb.get("ID"),
        "kb_name": kb.get("name") or kb.get("Name"),
        "knowledge_id": knowledge.get("id") or knowledge.get("ID"),
        "knowledge_title": knowledge.get("title") or knowledge.get("Title"),
        "chunk_id": chunk.get("id"),
        "chunk_index": chunk.get("chunk_index"),
        "chunk_content_preview": (chunk.get("content") or "")[:200],
        "question_id": q.get("id"),
        "question": q.get("question"),
        "answer": result.get("answer"),
        "thinking": result.get("thinking"),
        "cited_documents": cited_docs,
        "references": references,          # 引用块(含 content / score / chunk_index / match_type 等)
        "reference_count": len(references),
        "session_id": session_id,
        "assistant_message_id": result.get("assistant_message_id"),
        "usage": result.get("usage"),
        "error": result.get("error"),
        "duration_ms": duration_ms,
        "generated_at": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
    }


def main():
    parser = argparse.ArgumentParser(
        description="导出 WeKnora 知识库预生成问题及答案为 JSONL",
        formatter_class=argparse.ArgumentDefaultsHelpFormatter,
    )
    parser.add_argument("--kb-id", required=True, help="知识库 ID")
    parser.add_argument("--agent-id", default=os.environ.get("WEKNORA_AGENT_ID"),
                        help="自建智能体 ID(推荐)。注意:内置智能体(builtin-* 前缀,如"
                             " builtin-smart-reasoning)不生效 —— 它们的 max_completion_tokens"
                             " 为 0(未设置),不会覆盖服务端默认值 1024;其 24576 预算只在"
                             " agent-chat 的 ReAct 路径生效,知识问答不走该路径。请使用在"
                             " 界面里保存过明确 max_completion_tokens 的自建智能体"
                             "(或环境变量 WEKNORA_AGENT_ID)")
    parser.add_argument("--base-url", default=os.environ.get("WEKNORA_BASE_URL", DEFAULT_BASE_URL),
                        help="WeKnora 服务地址(或环境变量 WEKNORA_BASE_URL)")
    auth = parser.add_mutually_exclusive_group()
    auth.add_argument("--api-key", default=os.environ.get("WEKNORA_API_KEY"),
                      help="API Key(或环境变量 WEKNORA_API_KEY)")
    auth.add_argument("--token", default=os.environ.get("WEKNORA_JWT") or os.environ.get("WEKNORA_TOKEN"),
                      help="登录 JWT(或环境变量 WEKNORA_JWT)")
    parser.add_argument("--output", default=None,
                        help="输出 JSONL 路径,默认 scripts/output/<kb_id>_generated_qa.jsonl")
    parser.add_argument("--shard-size", type=int, default=0,
                        help="每个分片文件的记录数,0=单文件(默认)。十万级以上规模建议设置"
                             "(如 20000),输出为 <output前缀>.part0001.jsonl 等;续跑自动读取"
                             "所有分片。注意:从单文件切换到分片后,旧单文件进度不再续接")
    parser.add_argument("--progress", choices=["bar", "every", "none"], default="bar",
                        help="进度展示方式: bar=单行进度条实时刷新(默认,tty 下);"
                             "every=每 --progress-every 条打印一行(适合重定向到日志);"
                             "none=静默,仅结束汇总。stdout 被重定向时 bar 自动降级为 every")
    parser.add_argument("--progress-every", type=int, default=1,
                        help="every 模式下每完成多少个问题打印一行(失败始终打印)。"
                             "大规模建议 50~200")
    parser.add_argument("--limit", type=int, default=None, help="最多处理的问题数量")
    parser.add_argument("--concurrency", "-j", type=int, default=1,
                        help="并发线程数(多任务执行)。每个问题独立会话互不干扰;"
                             "过大可能触发服务端限流或拖慢模型响应,建议 2~8")
    parser.add_argument("--max-docs", type=int, default=None, help="最多遍历的文档数量")
    parser.add_argument("--doc-id", action="append", default=[], help="仅处理指定文档 ID(可重复)")
    parser.add_argument("--dedupe", action="store_true", help="跨分块去重相同的问题文本")
    parser.add_argument("--no-resume", action="store_true", help="忽略已有输出,重新开始")
    parser.add_argument("--retry-failed", action="store_true",
                        help="重试历史失败记录(报错/空答案/输出预算耗尽),成功后覆盖旧记录;"
                             "适用于换用 --agent-id 后重跑之前预算耗尽的问题")
    parser.add_argument("--insecure", action="store_true", help="跳过 HTTPS 证书校验")
    parser.add_argument("--timeout", type=int, default=60, help="普通 HTTP 请求超时(秒)")
    parser.add_argument("--dry-run", action="store_true", help="仅列出问题,不生成答案")
    args = parser.parse_args()

    if not args.api_key and not args.token:
        print("错误: 请通过 --api-key/--token 或环境变量 WEKNORA_API_KEY/WEKNORA_JWT 提供凭据",
              file=sys.stderr)
        sys.exit(2)

    if args.concurrency < 1 or args.concurrency > 32:
        print("错误: --concurrency 取值范围 1~32", file=sys.stderr)
        sys.exit(2)
    if args.shard_size < 0:
        print("错误: --shard-size 不能为负", file=sys.stderr)
        sys.exit(2)
    if args.progress_every < 1:
        print("错误: --progress-every 至少为 1", file=sys.stderr)
        sys.exit(2)
    output_path = Path(args.output) if args.output else (
        Path(__file__).resolve().parent / "output" / f"{args.kb_id}_generated_qa.jsonl")
    output_path.parent.mkdir(parents=True, exist_ok=True)

    client = Client(args.base_url, api_key=args.api_key, token=args.token,
                    timeout=args.timeout, verify=not args.insecure)

    # 1. 读取知识库
    try:
        kb_payload = client.get_knowledge_base(args.kb_id)
    except APIError as e:
        print(f"错误: 获取知识库失败: {e}", file=sys.stderr)
        sys.exit(1)
    if not kb_payload.get("success"):
        print(f"错误: 知识库不存在或无权访问: {json.dumps(kb_payload, ensure_ascii=False)[:300]}",
              file=sys.stderr)
        sys.exit(1)
    kb = kb_payload.get("data") or {}
    kb_name = kb.get("name") or kb.get("Name") or args.kb_id
    if args.agent_id:
        if args.agent_id.startswith("builtin-") or args.agent_id == "builtin-smart-reasoning":
            print("警告: 内置智能体的 max_completion_tokens 为 0(未设置),不会覆盖服务端"
                  "默认的 1024,很可能仍触发输出预算耗尽;建议改用自建智能体。",
                  file=sys.stderr)
    else:
        print("提示: 未指定 --agent-id,将使用服务端 config.yaml 的 conversation.summary"
              "配置(max_completion_tokens 默认仅 1024,开启思考的模型容易预算耗尽)。",
              file=sys.stderr)
    qg_cfg = kb.get("question_generation_config") or kb.get("QuestionGenerationConfig") or {}
    print(f"知识库: {kb_name} ({args.kb_id})  类型: {kb.get('Type') or kb.get('type')}")
    if qg_cfg and not qg_cfg.get("Enabled", qg_cfg.get("enabled")):
        print("提示: 该知识库的预生成问题开关未启用(question_generation_config.enabled=false),"
              "可能没有预生成问题。", file=sys.stderr)

    # 2. 收集预生成问题
    doc_filter = set(args.doc_id) if args.doc_id else None
    tasks, seen_texts, skipped = [], set(), 0
    for knowledge, chunk, q in iter_generated_questions(client, args.kb_id, args.max_docs, doc_filter):
        if args.limit and len(tasks) >= args.limit:
            break
        if args.dedupe:
            key = q["question"].strip()
            if key in seen_texts:
                skipped += 1
                continue
            seen_texts.add(key)
        tasks.append((knowledge, chunk, q))

    print(f"共收集到 {len(tasks)} 个预生成问题"
          + (f"(去重跳过 {skipped} 个)" if skipped else ""))
    if not tasks:
        print("没有找到预生成问题,退出。")
        return

    if args.dry_run:
        for i, (knowledge, chunk, q) in enumerate(tasks, 1):
            doc_title = knowledge.get("title") or knowledge.get("Title") or knowledge.get("id")
            print(f"{i:4d}. [{doc_title}] {q['question']}")
        return

    # 3. 断点续跑:跳过已完成的 (chunk_id, question);
    #    --retry-failed 时历史失败记录(预算耗尽/报错/空答案)会被清除并重新生成
    out_files = existing_output_files(output_path, args.shard_size)
    done, failed_keys, corrupt = (set(), set(), 0)
    if not args.no_resume and out_files:
        done, failed_keys, corrupt = load_done_keys(out_files, include_failed=args.retry_failed)
    pending = [t for t in tasks if (t[1].get("id"), t[2]["question"]) not in done]
    if corrupt:
        _, dropped_corrupt = purge_bad_lines(out_files)
        print(f"检测到 {corrupt} 条中断残缺行,已清理,对应问题将重新生成")
    if args.retry_failed and failed_keys:
        dropped, _ = purge_bad_lines(out_files, failed_keys)
        print(f"重试历史失败记录: 清除 {dropped} 条旧失败记录,将重新生成")
    if len(pending) < len(tasks):
        print(f"断点续跑: 已完成 {len(tasks) - len(pending)} 个,待处理 {len(pending)} 个")

    # 4. 并发生成答案并写入 JSONL(每个问题独立会话,互不干扰;
    #    并发时结果按完成顺序写入,断点续跑不依赖行序;
    #    shard_size>0 时自动分片输出,避免单文件过大)
    ok_count, fail_count = 0, 0
    total = len(pending)
    io_lock = threading.Lock()  # 保护文件写入与进度打印
    writer = ShardedJsonlWriter(output_path, args.shard_size)
    if args.shard_size > 0:
        print(f"分片输出: 每片 {args.shard_size} 条,前缀 {output_path}")

    # 进度展示: bar=单行进度条实时刷新;every=每 N 条一行;none=静默。
    # stdout 被重定向时 bar 自动降级为 every,避免 \r 控制字符写进日志。
    progress_mode = args.progress
    if progress_mode == "bar" and not sys.stdout.isatty():
        progress_mode = "every"
        print("提示: 检测到 stdout 被重定向,进度条降级为批量打印(--progress-every)。",
              file=sys.stderr)
    bar_width = 28
    last_bar_len = [0]  # 当前进度条行长度,供清行覆盖用

    def fmt_duration(sec):
        sec = max(0, int(sec))
        if sec >= 3600:
            return f"{sec // 3600}:{sec % 3600 // 60:02d}:{sec % 60:02d}"
        return f"{sec // 60:02d}:{sec % 60:02d}"

    def draw_bar(finished, started_at):
        pct = finished / total * 100 if total else 100.0
        filled = int(bar_width * finished / total) if total else bar_width
        bar = "█" * filled + "░" * (bar_width - filled)
        elapsed = time.time() - started_at
        rate = finished / elapsed if elapsed > 0.5 else 0
        remain = (total - finished) / rate if rate > 0 else 0
        line = (f"[{bar}] {finished}/{total} ({pct:5.1f}%) "
                f"成功 {ok_count} 失败 {fail_count} | "
                f"已用 {fmt_duration(elapsed)} 剩余约 {fmt_duration(remain)}")
        sys.stdout.write("\r" + line)
        last_bar_len[0] = len(line)
        sys.stdout.flush()

    def clear_bar():
        if last_bar_len[0]:
            sys.stdout.write("\r" + " " * last_bar_len[0] + "\r")
            last_bar_len[0] = 0
            sys.stdout.flush()

    def run_one(idx, knowledge, chunk, q):
        """处理单个问题,返回 (record, 状态, 附加信息)。线程安全(不共享可变状态)。"""
        query = q["question"]
        start = time.time()
        try:
            result, session_id = ask_question(client, query, args.kb_id,
                                              knowledge.get("id") or knowledge.get("ID"),
                                              agent_id=args.agent_id)
        except APIError as e:
            result = {"answer": "", "thinking": "", "references": [],
                      "assistant_message_id": "", "usage": None, "error": str(e)}
            session_id = None
        except Exception as e:  # 非预期异常不拖垮整个任务
            result = {"answer": "", "thinking": "", "references": [],
                      "assistant_message_id": "", "usage": None,
                      "error": f"{type(e).__name__}: {e}"}
            session_id = None
        duration_ms = int((time.time() - start) * 1000)
        record = build_record(kb, knowledge, chunk, q, result, session_id, duration_ms)
        return record, idx, query

    try:
        if args.concurrency == 1:
            worker_pool = None
            iterator = (run_one(i, k, c, q) for i, (k, c, q) in enumerate(pending, 1))
        else:
            worker_pool = ThreadPoolExecutor(max_workers=args.concurrency)
            futures = [worker_pool.submit(run_one, i, k, c, q)
                       for i, (k, c, q) in enumerate(pending, 1)]
            iterator = (f.result() for f in as_completed(futures))

        print(f"并发线程数: {args.concurrency}, 进度模式: {progress_mode}"
              + (f"(每 {args.progress_every} 条)" if progress_mode == "every" else ""))
        started_at = time.time()
        try:
            for record, idx, query in iterator:
                preview = query[:60] + ('...' if len(query) > 60 else '')
                with io_lock:
                    writer.write(json.dumps(record, ensure_ascii=False) + "\n")
                    answer = record.get("answer") or ""
                    if record.get("error") or not answer:
                        reason = record.get("error") or "答案为空"
                        fail_count += 1
                    elif EmptyAnswerBudgetMarker in answer:
                        reason = ("输出预算耗尽,请用 --agent-id 指定配置了大 "
                                  "max_completion_tokens 的自建智能体")
                        fail_count += 1
                    else:
                        reason = None
                        ok_count += 1
                    finished = ok_count + fail_count
                    fail_line = (f"[{idx}/{total}] 失败({record['duration_ms']}ms): "
                                 f"{reason} | {preview}") if reason else None
                    if progress_mode == "bar":
                        if fail_line:
                            clear_bar()
                            print(fail_line)
                        draw_bar(finished, started_at)
                    elif progress_mode == "every":
                        if fail_line:
                            print(fail_line)
                        elif (args.progress_every <= 1
                                or finished % args.progress_every == 0):
                            print(f"[{finished}/{total}] "
                                  f"完成({record['duration_ms']}ms, "
                                  f"引用{record['reference_count']}条) | {preview}")
                    # none: 静默
        finally:
            if progress_mode == "bar":
                clear_bar()
            if worker_pool is not None:
                worker_pool.shutdown(wait=True, cancel_futures=True)
    finally:
        writer.close()

    print(f"\n完成: 成功 {ok_count}, 失败 {fail_count}, 总计 {ok_count + fail_count}, "
          f"耗时 {fmt_duration(time.time() - started_at)}")
    if args.shard_size > 0:
        parts = sorted(output_path.parent.glob(
            f"{output_path.stem}.part*{output_path.suffix}"))
        print(f"输出文件: {len(parts)} 个分片,前缀 {output_path}"
              + (f"\n  合并查看: cat {output_path.parent}/{output_path.stem}.part*.jsonl > all.jsonl"
                 if parts else ""))
    else:
        print(f"输出文件: {output_path}")


if __name__ == "__main__":
    main()
