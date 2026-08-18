# -*- coding: utf-8 -*-
"""mistral-bridge 深度探针(第二轮)。承接 e2e.py 的 34/34 绿,覆盖以下 4 件事:

  T1 usage 精确性(多尺度) + 前缀缓存渐进验证(25K→50K→100K→150K→200K 共享前缀续打,
     观察 TTFT 增长曲线是否呈亚线性)
  T2 多上下文尺度下的 TTFT 与吐字速度(tok/s)
  T3 工具调用「流式+并发」组合;两种内置网络搜索批量验 ;搜索开启后日常对话行为
  T4 非 JSON 模式 reasoning_effort=high 空回探测(高 budget 下 10 连打)

所有 prompt 采用统一「reasoning_effort: high + max_tokens 拉高」以满足新约定。
"""
import json
import os
import sys
import threading
import time
import queue
import urllib.request
import urllib.error

BASE = os.environ.get("BRIDGE", "http://127.0.0.1:8080")
KEY = os.environ.get("MISTRAL_KEY", "")
MODEL = "glm-5-2"

# ---------- HTTP 底层 ----------

def _post(payload, stream=False, timeout=900, tag=""):
    hdrs = {"Authorization": f"Bearer {KEY}", "Content-Type": "application/json"}
    data = json.dumps(payload).encode()
    req = urllib.request.Request(BASE + "/v1/chat/completions", data=data, headers=hdrs, method="POST")
    t0 = time.time()
    try:
        resp = urllib.request.urlopen(req, timeout=timeout)
        ttfb = None
        chunks = []
        for raw in resp:
            if ttfb is None:
                ttfb = time.time() - t0
            chunks.append(raw)
        if not chunks:
            ttfb = time.time() - t0
        return 200, chunks, ttfb if ttfb is not None else 0.0, None
    except urllib.error.HTTPError as e:
        body = e.read()[:2000]
        return e.code, [body], time.time() - t0, f"HTTPError {e.code}"
    except Exception as e:
        return -1, [], time.time() - t0, f"EXC {type(e).__name__}: {e}"


def _sse_text(chunks):
    """聚合流式 chunk 内容;返回(content, reasoning, finish, usage, done)"""
    content, reasoning, finish, usage, done = "", "", None, None, False
    for raw in chunks:
        line = raw.decode("utf-8", errors="replace").rstrip("\r\n") if isinstance(raw, bytes) else raw
        if not line.startswith("data:"):
            continue
        data = line[5:].strip()
        if data == "[DONE]":
            done = True
            continue
        try:
            j = json.loads(data)
        except Exception:
            continue
        for c in j.get("choices", []) or []:
            d = c.get("delta", {})
            if d.get("content"):
                content += d["content"]
            if d.get("reasoning_content"):
                reasoning += d["reasoning_content"]
            if c.get("finish_reason"):
                finish = c["finish_reason"]
        if j.get("usage"):
            usage = j["usage"]
    return content, reasoning, finish, usage, done


def _call(messages, stream=False, max_tokens=32000, high=True, **kw):
    """统一调用约定:high=默认开,max_tokens 拉宽。返回 (content, finish, usage, ttfb, total, err)"""
    payload = {"model": MODEL, "messages": messages, "max_tokens": max_tokens, "stream": stream}
    if high:
        payload["reasoning_effort"] = "high"
    payload.update({k: v for k, v in kw.items() if v is not None})
    t0 = time.time()
    st, chunks, ttfb, err = _post(payload, stream=stream, timeout=1200, tag=kw.get("_tag", ""))
    total = time.time() - t0
    if err or st != 200:
        return None, None, None, ttfb, total, f"st={st} err={err}"
    if stream:
        content, reasoning, finish, usage, done = _sse_text(chunks)
        # 流式把 reasoning 与 content 分别处理
        return content, finish, usage, ttfb, total, None
    else:
        try:
            j = json.loads(b"".join(chunks))
            m = j["choices"][0]["message"]
            return (m.get("content") or ""), j["choices"][0]["finish_reason"], j.get("usage"), ttfb, total, None
        except Exception as e:
            return None, None, None, ttfb, total, f"parse {e}"


def big_text(target_tokens):
    word = "mistral bridge token throughput probe numeric id "
    line_tpl = "seq{n}. " + word + "chunk{n}\n"
    n_lines = max(1, target_tokens // 18)
    return "".join(line_tpl.format(n=i) for i in range(n_lines))


def log(name, marker, detail=""):
    print(f"[{marker}] {name} {detail}", flush=True)


# ---------- T1: usage 精确性 + 缓存渐进 ----------

def t1_usage_and_progressive_cache():
    print("\n== T1 usage 精确性 + 前缀缓存渐进验证 ==", flush=True)
    sizes = [25_000, 50_000, 100_000, 150_000, 200_000]
    results = []

    # 渐式增长:每段 prompt 共享予段前缀(后一次 prompt 是前一次的超集)
    full_text = big_text(200_000)
    prev_ttfb = None
    for sz in sizes:
        text = full_text[: sz * 4]  # 每 token ≈ 4 ascii chars
        msgs = [{"role": "user", "content": f"Long doc:\n{text}\nOnly answer size-ok."}]
        content, finish, usage, ttfb, total, err = _call(msgs, stream=False, max_tokens=32000, high=False)
        if err:
            log(f"T1 size={sz}", "FAIL", err)
            results.append((sz, None, None, None))
            continue
        pt = usage.get("prompt_tokens") if usage else None
        log(f"T1 size={sz}", "DATA",
            f"prompt_tokens={pt} ttfb={ttfb:.1f}s total={total:.1f}s "
            f"growth={(ttfb - prev_ttfb):.1f}s" if prev_ttfb else f"prompt_tokens={pt} ttfb={ttfb:.1f}s")
        results.append((sz, pt, ttfb, total))
        prev_ttfb = ttfb
        time.sleep(1)

    # 判定:usage 精确存在 vs 尺寸相符比例
    ok = all(pt for _, pt, _, _ in results if pt)
    for sz, pt, _, _ in results:
        if pt and abs(pt - sz) / sz > 0.3:
            ok = False
            log(f"T1 size={sz}", "WARN", f"prompt_tokens={pt} 偏离目标 {sz} 超 30%")
    log("T1 usage 精确", "PASS" if ok else "FAIL", str(results))

    # 缓存判定:若命中缓在,第 N 次相比第 N-1 次 ttfb 增量应明显趋平
    ttfbs = [t for _, _, t, _ in results if t]
    flat = len(ttfbs) >= 4 and ttfbs[-1] - ttfbs[1] < ttfbs[1] * 0.3
    log("T1 cache 加速可见", "INFO",
        f"ttfb序列={['%.1f' % t for t in ttfbs]}({('亚线性' if flat else '线性增长~无可见缓存加速')})")
    return ok, flat, results


# ---------- T2: 多上下文 TTFT 与吐字速度 ----------

def t2_ttft_and_throughput():
    print("\n== T2 多上下文 TTFT / tok/s ==", flush=True)
    sizes = [1_000, 50_000, 100_000, 200_000]
    res = []
    for sz in sizes:
        text = big_text(sz)
        msgs = [{"role": "user", "content": f"{text}\nCount 1 to 300, one number per line."}]
        # 流式量 TTFT + 总时间,估算 tok/s
        content, finish, usage, ttfb, total, err = _call(msgs, stream=True, max_tokens=32000, high=False)
        if err:
            log(f"T2 size={sz}", "FAIL", err)
            continue
        ct = usage.get("completion_tokens") if usage else 0
        gen_t = total - ttfb
        tps = ct / max(gen_t, 0.1)
        log(f"T2 size={sz}", "DATA", f"ttfb={ttfb:.1f}s total={total:.1f}s gen={gen_t:.1f}s ct={ct} tps={tps:.1f}")
        res.append((sz, ttfb, total, ct, tps))
        time.sleep(1)
    return res


# ---------- T3: 工具「流式+并发」 & 内置搜索批测 & 搜索日常切换 ----------

def t3_stream_concurrent_tools():
    print("\n== T3 工具调用「流式+并发」 ==", flush=True)
    tools = [{"type": "function", "function": {
        "name": "get_weather", "description": "Get weather",
        "parameters": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]},
    }}]
    cities = ["Beijing", "Shanghai", "Berlin", "London", "Tokyo"]
    results = {}
    lock = threading.Lock()

    def worker(city):
        msgs = [{"role": "user", "content": f"Weather in {city}? call the tool."}]
        tools_ = tools
        payload = {"model": MODEL, "messages": msgs, "tools": tools_, "stream": True,
                   "max_tokens": 32000, "reasoning_effort": "high"}
        st, chunks, ttfb, err = _post(payload, stream=True, timeout=120)
        if err or st != 200:
            with lock:
                results[city] = ("err", f"st={st} err={err}")
            return
        # 聚合 tool_calls
        tool_args = ""
        done = False
        finish = None
        for raw in chunks:
            line = raw.decode("utf-8", errors="replace").rstrip("\r\n")
            if not line.startswith("data:"):
                continue
            d = line[5:].strip()
            if d == "[DONE]":
                done = True
                continue
            try:
                j = json.loads(d)
            except Exception:
                continue
            for c in j.get("choices", []) or []:
                for tc in c.get("delta", {}).get("tool_calls", []) or []:
                    if tc.get("function", {}).get("arguments"):
                        tool_args += tc["function"]["arguments"]
                if c.get("finish_reason"):
                    finish = c["finish_reason"]
        try:
            args = json.loads(tool_args) if tool_args else None
        except Exception as e:
            with lock:
                results[city] = ("parse_fail", f"args={tool_args!r} err={e}")
            return
        with lock:
            results[city] = ("ok", f"args={args} finish={finish} done={done}")

    threads = [threading.Thread(target=worker, args=(c,), daemon=True) for c in cities]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    ok_count = sum(1 for v in results.values() if v[0] == "ok" and "city" in (v[1] or ""))
    log(f"T3 5并发工具", "PASS" if ok_count == 5 else "FAIL", f"ok={ok_count}/5 " + str(results))
    return ok_count == 5, results


def t3_builtin_search_batch():
    print("\n== T3 内置网络搜索批量 ==", flush=True)
    cases = [
        ("web_search 普搜", [{"type": "web_search"}]),
        ("web_search_premium 优搜", [{"type": "web_search_premium"}]),
        ("双搜冲突应 422", [{"type": "web_search"}, {"type": "web_search_premium"}]),
        ("搜索+自定义 function 混", [{"type": "web_search"},
                                   {"type": "function", "function": {"name": "noop", "description": "noop",
                                                                                    "parameters": {"type": "object", "properties": {}, "required": []}}}]),
    ]
    for name, tools in cases:
        payload = {"model": MODEL, "messages": [{"role": "user", "content": "current date today please."}],
                   "tools": tools, "max_tokens": 32000}
        st, chunks, ttfb, err = _post(payload, timeout=120)
        st_desc = st
        if st == 200:
            try:
                j = json.loads(b"".join(chunks))
                ans = j["choices"][0]["message"].get("content", "")[:60]
                usage = j.get("usage", {})
                log(f"搜索 {name}", "DATA", f"st=200 usage={usage} content={ans!r}...")
            except Exception as e:
                log(f"搜索 {name}", "FAIL", f"parse {e}")
        else:
            log(f"搜索 {name}", "DATA", f"st={st} body={b''.join(chunks)[:120]!r}")


def t3_daily_chat_search_on_vs_off():
    print("\n== T3 日常对话 in 开启/关闭搜索对比 ==", flush=True)
    q = "What is the capital of France? Just say its name."
    for tag, tools in [("关闭", None), ("开启搜索", [{"type": "web_search"}])]:
        payload = {"model": MODEL, "messages": [{"role": "user", "content": q}], "max_tokens": 32000}
        if tools:
            payload["tools"] = tools
        st, chunks, ttfb, err = _post(payload, timeout=90)
        if err or st != 200:
            log(f"对话 {tag}", "FAIL", f"st={st} err={err}")
            continue
        j = json.loads(b"".join(chunks))
        ans = j["choices"][0]["message"].get("content", "")
        usage = j.get("usage", {})
        log(f"对话 {tag}", "DATA", f"content={ans[:60]!r} usage={usage}")


# ---------- T4: 非 JSON 模式 high 空回 10 连打 ----------

def t4_empty_reply_high_budget():
    print("\n== T4 非 JSON high thinking 空回探测(10 连,max_tokens=32000) ==", flush=True)
    empty_count = 0
    short_count = 0
    details = []
    for i in range(10):
        msgs = [{"role": "user", "content": f"Briefly explain photosynthesis in 2-3 sentences. (run {i})"}]
        content, finish, usage, ttfb, total, err = _call(msgs, stream=True, max_tokens=32000, high=True)
        if err:
            details.append((i, "err", err))
            continue
        ct = usage.get("completion_tokens") if usage else 0
        clen = len(content) if content else 0
        tag = "ok"
        if clen == 0:
            tag = "EMPTY"
            empty_count += 1
        elif clen < 15:
            tag = "SHORT"
            short_count += 1
        details.append((i, tag, f"ct={ct} clen={clen} ttfb={ttfb:.1f}s"))
        log(f"T4 run {i}", "DATA", f"finish={finish} ct={ct} clen={clen}")
        time.sleep(0.5)
    log("T4 空回统计", "PASS" if empty_count == 0 else "WARN",
        f"empty={empty_count}/10 short={short_count}/10")
    return empty_count == 0


# ---------- main ----------

def main():
    if not KEY:
        print("MISTRAL_KEY not set", file=sys.stderr)
        sys.exit(1)
    which = sys.argv[1] if len(sys.argv) > 1 else "all"

    if which in ("all", "t1"):
        t1_usage_and_progressive_cache()
    if which in ("all", "t2"):
        t2_ttft_and_throughput()
    if which in ("all", "t3"):
        t3_stream_concurrent_tools()
        t3_builtin_search_batch()
        t3_daily_chat_search_on_vs_off()
    if which in ("all", "t4"):
        t4_empty_reply_high_budget()

    print("\n== deep_probe done ==", flush=True)


if __name__ == "__main__":
    main()
