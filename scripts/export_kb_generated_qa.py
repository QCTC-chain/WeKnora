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
    python3 scripts/export_kb_generated_qa.py --kb-id <id> --output out.jsonl --limit 20

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
import time
import urllib.error
import urllib.parse
import urllib.request
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
    """遍历知识库下所有文档的分块,产出 (knowledge, chunk, question_dict)。"""
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
            yield from iter_chunk_questions(client, kb_item)
        total = payload.get("total") or 0
        if page * PAGE_SIZE >= total:
            break
        page += 1


def iter_chunk_questions(client, knowledge):
    """遍历单个文档的分块,产出 (chunk, question_dict)。"""
    knowledge_id = knowledge.get("ID") or knowledge.get("id")
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
            for q in extract_questions(chunk):
                yield knowledge, chunk, q
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


def load_done_keys(output_path):
    """读取已有输出文件,返回已完成 (chunk_id, question) 集合,用于断点续跑。"""
    done = set()
    if not output_path.exists():
        return done
    with output_path.open("r", encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            try:
                rec = json.loads(line)
                done.add((rec.get("chunk_id"), rec.get("question")))
            except json.JSONDecodeError:
                continue
    return done


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
    parser.add_argument("--limit", type=int, default=None, help="最多处理的问题数量")
    parser.add_argument("--max-docs", type=int, default=None, help="最多遍历的文档数量")
    parser.add_argument("--doc-id", action="append", default=[], help="仅处理指定文档 ID(可重复)")
    parser.add_argument("--dedupe", action="store_true", help="跨分块去重相同的问题文本")
    parser.add_argument("--no-resume", action="store_true", help="忽略已有输出,重新开始")
    parser.add_argument("--insecure", action="store_true", help="跳过 HTTPS 证书校验")
    parser.add_argument("--timeout", type=int, default=60, help="普通 HTTP 请求超时(秒)")
    parser.add_argument("--dry-run", action="store_true", help="仅列出问题,不生成答案")
    args = parser.parse_args()

    if not args.api_key and not args.token:
        print("错误: 请通过 --api-key/--token 或环境变量 WEKNORA_API_KEY/WEKNORA_JWT 提供凭据",
              file=sys.stderr)
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

    # 3. 断点续跑:跳过已完成的 (chunk_id, question)
    done = set() if args.no_resume else load_done_keys(output_path)
    pending = [t for t in tasks if (t[1].get("id"), t[2]["question"]) not in done]
    if len(pending) < len(tasks):
        print(f"断点续跑: 已完成 {len(tasks) - len(pending)} 个,待处理 {len(pending)} 个")

    # 4. 逐个生成答案并写入 JSONL(每个问题独立会话,保证答案互不干扰)
    ok_count, fail_count = 0, 0
    with output_path.open("a", encoding="utf-8") as out:
        for i, (knowledge, chunk, q) in enumerate(pending, 1):
            query = q["question"]
            print(f"[{i}/{len(pending)}] {query[:60]}{'...' if len(query) > 60 else ''}", end=" ")
            start = time.time()
            try:
                result, session_id = ask_question(client, query, args.kb_id,
                                                  knowledge.get("id") or knowledge.get("ID"),
                                                  agent_id=args.agent_id)
            except APIError as e:
                result = {"answer": "", "thinking": "", "references": [],
                          "assistant_message_id": "", "usage": None, "error": str(e)}
                session_id = None
            duration_ms = int((time.time() - start) * 1000)

            record = build_record(kb, knowledge, chunk, q, result, session_id, duration_ms)
            out.write(json.dumps(record, ensure_ascii=False) + "\n")
            out.flush()

            if result.get("error") or not result.get("answer"):
                fail_count += 1
                print(f"失败({duration_ms}ms): {result.get('error') or '答案为空'}")
            elif EmptyAnswerBudgetMarker in result["answer"]:
                fail_count += 1
                print(f"失败({duration_ms}ms): 输出预算耗尽。"
                      "请用 --agent-id 指定配置了大 max_completion_tokens 的智能体。")
            else:
                ok_count += 1
                print(f"完成({duration_ms}ms, 引用{record['reference_count']}条)")

    print(f"\n完成: 成功 {ok_count}, 失败 {fail_count}")
    print(f"输出文件: {output_path}")


if __name__ == "__main__":
    main()
