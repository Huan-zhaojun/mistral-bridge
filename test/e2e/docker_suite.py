# -*- coding: utf-8 -*-
"""请在 docker network 内跑(或本机直连 http://mistral-bridge:8080):
容器内完整业务套件,覆盖非流式/流式/工具/JSON/内置搜索/错误路径。
"""
import json
import os
import sys
import time
import urllib.request
import urllib.error

BASE = "http://mistral-bridge:8080"
KEY = os.environ.get("MISTRAL_KEY", "")
PASS, FAIL = 0, 0
RESULTS = []


def record(name, ok, detail=""):
    global PASS, FAIL
    st = "PASS" if ok else "FAIL"
    if ok: PASS += 1
    else: FAIL += 1
    RESULTS.append((st, name, detail))
    print(f"[{st}] {name} {detail}", flush=True)


def call(payload, stream=False, timeout=900):
    t0 = time.time()
    data = json.dumps(payload).encode()
    req = urllib.request.Request(BASE + "/v1/chat/completions", data=data,
                                 headers={"Authorization": f"Bearer {KEY}", "Content-Type": "application/json"}, method="POST")
    try:
        resp = urllib.request.urlopen(req, timeout=timeout)
        ttfb = None
        chunks = []
        for raw in resp:
            if ttfb is None: ttfb = time.time() - t0
            chunks.append(raw)
        if not chunks: ttfb = time.time() - t0
        return resp.getcode(), dict(resp.headers), chunks, ttfb, time.time() - t0, None
    except urllib.error.HTTPError as e:
        return e.code, dict(e.headers), [e.read()], time.time()-t0, time.time()-t0, None
    except Exception as e:
        return -1, {}, [], time.time()-t0, time.time()-t0, f"EXC: {type(e).__name__}: {e}"


def t1_nonstream():
    st, hdr, chunks, *_ = call({"model":"glm-5-2","messages":[{"role":"user","content":"Say: docker-ok"}],"max_tokens":64000,"reasoning_effort":"high"})
    j = json.loads(b"".join(chunks))
    m = j["choices"][0]["message"]
    ok = st==200 and m.get("content") is not None and (j.get("usage") or {}).get("total_tokens",0)>0
    return ok, f"st={st} content={(m.get('content') or '')[:60]!r} usage={j.get('usage')}"

def t2_stream():
    st, hdr, chunks, ttfb, total, err = call({"model":"glm-5-2","messages":[{"role":"user","content":"Count 1 to 3"}],"max_tokens":64000,"stream":True}, stream=True)
    if err or st!=200: return False, f"st={st} err={err}"
    text, tool, finish, usage, done = "", None, None, None, False
    for raw in chunks:
        line = raw.decode("utf-8",errors="replace").rstrip("\r\n")
        if not line.startswith("data:"): continue
        d = line[5:].strip()
        if d=="[DONE]": done=True; continue
        try: j = json.loads(d)
        except: continue
        for c in j.get("choices",[]) or []:
            if c.get("delta",{}).get("content"): text += c["delta"]["content"]
            if c.get("finish_reason"): finish=c["finish_reason"]
        if j.get("usage"): usage=j["usage"]
    ok = done and finish in ("stop","length","tool_calls") and usage and text
    return ok, f"ttfb={ttfb:.1f}s finish={finish} usage_ok={usage is not None} done={done} text={text[:40]!r}"

def t3_tool_call():
    st, hdr, chunks, *_ = call({"model":"glm-5-2","messages":[{"role":"user","content":"Weather in Seoul? use get_weather."}],"tools":[{"type":"function","function":{"name":"get_weather","description":"...","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}],"tool_choice":"auto","max_tokens":64000})
    j = json.loads(b"".join(chunks))
    tc = (j["choices"][0]["message"].get("tool_calls") or [])[0]
    args = json.loads(tc["function"]["arguments"])
    ok = st==200 and tc["function"]["name"]=="get_weather" and args.get("city")=="Seoul" and j["choices"][0]["finish_reason"]=="tool_calls"
    return ok, f"st={st} city={args.get('city')} args_ok={args is not None}"

def t4_json_schema():
    st, hdr, chunks, ttfb, total, err = call({"model":"glm-5-2","messages":[{"role":"user","content":"Person Alice age 30."}],"response_format":{"type":"json_schema","json_schema":{"name":"person","strict":True,"schema":{"type":"object","properties":{"name":{"type":"string"},"age":{"type":"integer"}},"required":["name","age"],"additionalProperties":False}}},"max_tokens":64000,"reasoning_effort":"high","stream":True}, stream=True)
    if err or st!=200: return False, f"st={st} err={err}"
    text, done = "", False
    for raw in chunks:
        line = raw.decode("utf-8",errors="replace").rstrip("\r\n")
        if not line.startswith("data:"): continue
        d = line[5:].strip()
        if d=="[DONE]": done=True; continue
        try: j = json.loads(d)
        except: continue
        for c in j.get("choices",[]) or []:
            if c.get("delta",{}).get("content"): text += c["delta"]["content"]
    try:
        obj = json.loads(text)
        ok = obj.get("name")=="Alice" and obj.get("age")==30
        return ok, f"parsed={obj}"
    except Exception as e:
        return False, f"json illegal: {e}, content={text[:200]!r}"

def t5_builtin_premium():
    st, hdr, chunks, *_ = call({"model":"glm-5-2","messages":[{"role":"user","content":"current date today please."}],"tools":[{"type":"web_search_premium"}],"max_tokens":64000})
    j = json.loads(b"".join(chunks))
    m = j["choices"][0]["message"]
    ok = st==200 and bool(m.get("content"))
    return ok, f"st={st} (有 search 内容) content_len={len(m.get('content') or '')}"

def t6_error_401():
    """无 Authorization 头时桥应返回 401 invalid_api_key(不上游到上游)。"""
    data = json.dumps({"model":"glm-5-2","messages":[{"role":"user","content":"hi"}]}).encode()
    req = urllib.request.Request(BASE + "/v1/chat/completions", data=data,
                                 headers={"Content-Type": "application/json"}, method="POST")
    try:
        urllib.request.urlopen(req, timeout=15)
        return False, "expected 401, got 200"
    except urllib.error.HTTPError as e:
        j = json.loads(e.read())
        ok = e.code == 401 and j["error"]["code"] == "invalid_api_key"
        return ok, f"st={e.code} msg={j['error']['message'][:60]}"

def t7_image_cleaned():
    st, hdr, chunks, *_ = call({"model":"glm-5-2","messages":[{"role":"user","content":[{"type":"text","text":"what image?"},{"type":"image_url","image_url":{"url":"https://example.com/x.png"}}]}],"max_tokens":64000,"reasoning_effort":"high"})
    j = json.loads(b"".join(chunks))
    m = j["choices"][0]["message"]
    ok = st==200 and m.get("content")
    return ok, f"st={st} content={(m.get('content') or '')[:50]!r}"

def _call_wrapper(name, f):
    print(f"[RUN ] {name} ...", flush=True)
    t0=time.time()
    try: ok, detail = f()
    except Exception as e: ok, detail = False, f"EXC {type(e).__name__}: {e}"
    print(f"[{'PASS' if ok else 'FAIL'}] {name} ({time.time()-t0:.1f}s) {detail}", flush=True)
    record(name, ok, detail)

# 修正 call 的 key 需要用全局(t6 要无鉴权测试,则不入默认头):
_GLOBAL_KEY = KEY
def call_patch(payload, **kw): return call(payload, **kw)

if __name__ == "__main__":
    if not KEY:
        print("MISTRAL_KEY env not set", file=sys.stderr); sys.exit(1)
    print("== mistral-bridge 容器内完整套件 ==", flush=True)
    _call_wrapper("t1 nonstream docker", t1_nonstream)
    _call_wrapper("t2 stream docker", t2_stream)
    _call_wrapper("t3 tool_call docker", t3_tool_call)
    _call_wrapper("t4 json_schema docker", t4_json_schema)
    _call_wrapper("t5 builtin premium docker", t5_builtin_premium)
    _call_wrapper("t6 401 docker", lambda: t6_error_401())
    _call_wrapper("t7 image cleaned docker", t7_image_cleaned)
    print(f"\n== SUMMARY pass={PASS} fail={FAIL} ==", flush=True)
    sys.exit(0 if FAIL==0 else 1)
