# -*- coding: utf-8 -*-
"""web_search vs web_search_premium 质量对比实测。
同一批有时效性/事实深度要求的问题,各用两种搜索各打一次,人肉评审质量差异。
约定:max_tokens=32000, reasoning_effort=high(与后续测试保持一致)。
"""
import json
import os
import sys
import time
import urllib.request

BASE = os.environ.get("BRIDGE", "http://127.0.0.1:8080")
KEY = os.environ.get("MISTRAL_KEY", "")
MODEL = "glm-5-2"
MAX_TOKENS = 32000

QUESTIONS = [
    ("date_today", "What is today's exact date (year-month-day)?"),
    ("go_latest", "What is the latest stable Go language version, and its release date?"),
    ("claude_latest", "What is the latest Claude model released by Anthropic?"),
    ("mistral_news", "Mistral AI 最近(2026年8月)有什么新发布或新闻?"),
]


def call(question, tool_type):
    payload = {
        "model": MODEL,
        "messages": [{"role": "user", "content": question}],
        "tools": [{"type": tool_type}],
        "max_tokens": MAX_TOKENS,
        "reasoning_effort": "high",
        "stream": False,
    }
    data = json.dumps(payload).encode()
    req = urllib.request.Request(BASE + "/v1/chat/completions", data=data,
                                 headers={"Authorization": f"Bearer {KEY}", "Content-Type": "application/json"}, method="POST")
    t0 = time.time()
    try:
        with urllib.request.urlopen(req, timeout=900) as r:
            j = json.loads(r.read())
    except Exception as e:
        return None, f"EXC {type(e).__name__}: {e}", time.time() - t0
    m = j["choices"][0]["message"]
    usage = j.get("usage", {})
    return {
        "content": m.get("content", ""),
        "reasoning_len": len(m.get("reasoning_content") or ""),
        "usage": usage,
        "finish": j["choices"][0]["finish_reason"],
        "wall": time.time() - t0,
    }, None, time.time() - t0


def main():
    if not KEY:
        print("MISTRAL_KEY not set", file=sys.stderr)
        sys.exit(1)
    for tag, q in QUESTIONS:
        print(f"\n{'='*70}\nQ[{tag}]: {q}\n{'='*70}", flush=True)
        for tool in ("web_search", "web_search_premium"):
            res, err, _ = call(q, tool)
            if err:
                print(f"--[{tool}] ERR {err}", flush=True)
                continue
            u = res["usage"]
            print(f"--[{tool}] wall={res['wall']:.0f}s finish={res['finish']} reasoning_len={res['reasoning_len']} usage={u['prompt_tokens']}+{u['completion_tokens']}={u['total_tokens']}", flush=True)
            print(res["content"][:900], flush=True)
            if len(res["content"]) > 900:
                print(f"...(+{len(res['content'])-900} more chars)", flush=True)


if __name__ == "__main__":
    main()
