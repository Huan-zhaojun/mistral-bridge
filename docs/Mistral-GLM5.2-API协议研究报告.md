# GLM-5-2 @ Mistral API 协议研究(渠道协议档案)

> 研究日期:2026-08-17(同日二轮深化:官方 SDK 源码逐文件比对 + 12 项空白补测)
> 研究对象:Mistral AI Studio Playground(console.mistral.ai)中 glm-5-2 模型的完整协议
> 方法:浏览器抓包 + API key 直连实测 + 官方 SDK 源码分析 + 官方文档比对
> 范围:**本档案只记录上游渠道的协议事实**(端点/字段/行为/坑位);本项目桥(mistral-bridge)的设计与实现决策见 [design.md](design.md)
> 状态:研究收官,渠道协议事实已钉死

## 1. 模型基本信息

| 项 | 值 |
|---|---|
| 模型 ID | `glm-5-2`(别名 `zai-glm-5-2`) |
| 描述 | Official Z.ai GLM 5.2 model(Mistral 转租 Z.ai) |
| **上下文长度** | **1,048,576 tokens(1M)** |
| capabilities | completion_chat ✅ / function_calling ✅ / reasoning ✅ |
| vision | ❌ 不支持——**GLM-5-2 是纯文本模型,非多模态**,平台 vision=false 与其相符(实测图像输入 400 code 3051) |
| 底层架构 | **litellm + Hosted_vllm**(从报错泄露,工具 ID 前缀 `chatcmpl-` 佐证;Kong 网关,响应头带 `x-kong-request-id`) |

---

## 2. Playground vs API key(两者关系)

**结论:本质同一个东西,完全等价。**

| 维度 | Playground(演练场) | API key 直连 |
|---|---|---|
| 端点 | `console.mistral.ai/api-ui/bora/v1/conversations` | `api.mistral.ai/v1/conversations` |
| 认证 | 浏览器 session cookie | `Authorization: Bearer <key>` |
| 配额池 | **同一池**(实测剩余额联动) | 同一池 |
| 请求格式 | 完全一致 | 完全一致 |

- Playground 的 "Export as code" 导出的就是直连代码(SDK 2.0 `client.beta.conversations.start()`)
- 限流头实测:两个通道均为 `x-ratelimit-limit-tokens-minute: 500000`

**⚠️ 重要:存在另一套 `/v1/chat/completions`(OAI 兼容)端点,glm-5-2 在该端点上 TPM=0,直接 429(其他模型正常)。OAI 兼容端点无法调用 GLM-5-2,只能走 conversations 端点。**

---

## 3. Playground UI 功能全貌

- **Model**: glm-5-2
- **Capabilities**:
  - 内置工具:Code(代码解释器)、Image(图像生成)、Search(web_search)、Premium Search(web_search_premium)
  - Reasoning effort:仅 `None` / `High` 两档
  - Functions:自定义函数(Add)
- **Response Format**: Text / JSON / JSON-Schema
- **Instructions**: 系统提示词
- **Code**: 导出 Python SDK 2.0 / TypeScript / cURL
- **Create Agent**: 一键创建 Agent

---

## 4. 抓包协议(Playground 实际请求)

```
POST /api-ui/bora/v1/conversations
```

```json
{
  "model": "glm-5-2",
  "instructions": "",
  "completion_args": {
    "temperature": 0.7,
    "max_tokens": 4096,
    "top_p": 1,
    "reasoning_effort": "high"
  },
  "tools": [],
  "stream": true,
  "inputs": [
    {"object": "entry", "type": "message.input", "role": "user", "content": "你好", "prefix": false},
    {"object": "entry", "type": "message.output", "role": "assistant",
     "content": [
       {"type": "thinking", "thinking": [{"type": "text", "text": "思考过程..."}]},
       {"type": "text", "text": "回复文本"}
     ]}
  ],
  "name": "会话标题"
}
```

关键点:assistant 消息的 `content` 是**块数组**(thinking 块 + text 块)。

---

## 5. /v1/conversations 完整协议

### 5.1 请求顶层字段(白名单,其余 422)

与官方 SDK `ConversationRequest` 逐字段比对**完全一致**,共 13 个字段:

| 字段 | 说明 | 实测 |
|---|---|---|
| `inputs` | 对话输入条目(必填) | ✅ 甚至接受**纯字符串** `inputs:"Say OK"`;空数组 422 "inputs is required" |
| `stream` | SSE 流式 | ✅ SDK 非流式模型里该字段 Literal[False],分开建模 |
| `store` | 是否存储会话 | ✅ **store=false 时服务端不保留任何记录**(事后 GET 404) |
| `handoff_execution` | agent 专用 client/server | ❌ 模型模式(model 字段)下拒绝 |
| `instructions` | 系统提示词 | ✅ 仅 instructions 无 inputs → 422 |
| `tools` | 工具列表 | ✅ |
| `completion_args` | 采样参数(见 5.2) | ✅ 可整个省略 |
| `guardrails` | 内容过滤(须恰好一个配置) | 格式:`block_on_error` + `moderation_llm_v1`(官方文档) |
| `name` / `description` / `metadata` | 会话元数据 | ✅ |
| `agent_id` / `agent_version` | 接 Agents | ✅(与 model 互斥,422 双路校验) |
| `model` | 模型 ID | ✅ 与 agent_id 互斥 |
| OAI 顶层字段(`messages`/`n`/`user`/`max_completion_tokens`/`parallel_tool_calls`) | — | ❌ 全部 422 |

**同池其他模型**:`mistral-large-latest` 在同端点可用;`magistral-medium-latest` 报 400 code 3003 "currently not in use"(模型可用性按工作区启用范围)。

### 5.2 completion_args 白名单

与官方 SDK `CompletionArgs` 比对一致(11 字段):

| 参数 | 范围/取值 | 实测 |
|---|---|---|
| `temperature` | **0 ~ 1**(≤1) | >1 422 |
| `max_tokens` | **≤ 1,048,576**(1M−prompt) | 64000~1000000 全接受 |
| `top_p` | 任意 | ✅ |
| `reasoning_effort` | **仅 `none` / `high`** | ⚠️ SDK 枚举有 6 档(none/minimal/low/medium/high/xhigh),但**服务端收窄**只收 none/high |
| `stop` | 字符串或数组 | ✅ |
| `random_seed` | int | ✅(`seed` 不支持) |
| `frequency_penalty` / `presence_penalty` | float | ✅ |
| `response_format` | `text` / `json_object` / `json_schema` | ✅ 严格生效(深测见 5.9;注意流式+high 的首 delta 重复 bug) |
| `tool_choice` | **`auto` / `none` / `required`** | ⚠️ SDK 枚举含 `any` + 对象形态,但服务端 422:"Input should be 'auto', 'none' or 'required'" |
| `prediction` | OAI 预测输出 | ✅ 生效(预测前缀 2.4s 返回) |
| `top_k` / `seed` / `n` / `safe_prompt` / `min_tokens` | — | ❌ 全部 422 extra_forbidden |

### 5.3 消息条目类型(inputs)

- `message.input`(user/assistant 消息,`content` 可为**字符串或块数组**)
- `message.output`(assistant 回复,回传历史时用)
- `function.result`(工具结果回传:`tool_call_id` + `result` 字符串)
- `function.call`(模型发起的工具调用,响应中出现)
- **role 仅 `user` / `assistant`**;系统提示词必须用 `instructions` 字段
- `prefix`(⚠️ **勘误**:此字段不是缓存标记,是**续写/prefill 语义**——实测 assistant 条目带 `prefix:true` + content `"The capital of France is"` → 模型直接续写输出 `" Paris."`;user 条目上无效果)。转换器**始终不传/传 false**,严禁把历史 assistant 消息标成 prefix(否则会被当成待续写文本)
- 图像输入 ❌(400 code 3051)
- **`tool_call_id` 无硬校验**:乱配对的 `function.result`(bogus id)实测 200——模型只把它当上下文噪音,不报错(但转换器仍应严格 zip 配对,勿依赖服务端宽容)

### 5.4 工具调用完整协议

**合法工具类型**(服务端错误信息原文):
```
code_interpreter, connector, document_library, function, image_generation, web_search, web_search_premium
```

**工具定义**(OAI 风格):
```json
{"type": "function", "function": {"name": "get_weather", "description": "...", "parameters": {...}}}
```

**调用响应**(outputs 中出现):
```json
{
  "object": "entry", "type": "function.call",
  "tool_call_id": "chatcmpl-tool-b5164e42dea738dc",
  "name": "get_weather",
  "arguments": "{\"city\": \"Beijing\"}",
  "confirmation_status": null
}
```
`confirmation_status` 枚举(SDK):`pending` / `allowed` / `denied`。

**结果回传**(追加到 inputs):
```json
{"object": "entry", "type": "function.result",
 "tool_call_id": "chatcmpl-tool-xxx",
 "result": "{\"city\":\"Shanghai\",\"temperature\":\"28C\"}"}
```

**工具确认机制**(可选审批,agent 场景):
```json
{"tool_call_id": "chatcmpl-tool-xxx", "confirmation": "allow"}   // allow / deny
```
⚠️ function.result 回传后工具即视为已处理,重复提交确认报 400。

**流式工具调用**:`function.call.delta` 事件增量传输 arguments,**且每个 delta 都带全量 name + tool_call_id + output_index**(映射成 OAI tool_calls 时零状态)。

### 5.5 流式格式(SSE)

**传输格式**:`event: <类型>` + `data: <JSON>` 标准双行 SSE(解析时只取 data 行)。

**事件类型**(与官方 SDK `SSETypes` 枚举 10 种完全一致):
- `conversation.response.started`(含 conversation_id)
- `conversation.response.done`(含 usage + created_at)
- `conversation.response.error` ⚠️(此前未列入,SDK 有)
- `message.output.delta`(内容块增量)
- `function.call.delta`(工具参数增量)
- `tool.execution.started` / `.delta` / `.done`(内置工具执行)
- `agent.handoff.started` / `.done`(agent 模式)

**text delta 是裸字符串,thinking delta 是 dict**:
```
event: message.output.delta
data: {"type":"message.output.delta","output_index":0,"content_index":0,"id":"msg_...","role":"assistant","content":"17 * 23 is"}

event: message.output.delta
data: {"type":"message.output.delta","output_index":0,"content_index":0,"id":"msg_...","role":"assistant",
       "content":{"type":"thinking","thinking":[{"type":"text","text":"思考片段"}],"closed":true}}
```
⚠️ 每条 thinking delta 都带 `closed:true`(SDK 注释:closed 目前仅用于 prefix 场景),**不能靠 closed=true 判定思考结束**,只能靠后续 text delta 出现判定转换。

**事件序列**(thinking + text 场景):`started → N×thinking delta → N×text delta → done`,索引字段 `output_index` + `content_index` 用于多输出槽寻址。

### 5.6 响应结构(非流式)

```json
{
  "object": "conversation.response",
  "conversation_id": "conv_...",
  "outputs": [
    {"object": "entry", "type": "message.output", "role": "assistant",
     "id": "msg_...", "model": "glm-5-2",
     "created_at": "...", "completed_at": "...",
     "content": [{"type": "thinking", ...}, {"type": "text", "text": "..."}]},
    {"object": "entry", "type": "function.call", "tool_call_id": "...", "name": "...", "arguments": "..."}
  ],
  "usage": {"prompt_tokens": 154, "completion_tokens": 24, "total_tokens": 178},
  "guardrails": null
}
```

⚠️ 三个实测要点:
1. **简单回复(无 thinking/工具)时 `content` 是纯字符串**(`"content": "PONG"`),非块数组——消费端必须双态兼容。
2. **无 finish_reason / stop_reason 任何字段**(SDK `ConversationResponse` 证实)。
3. **usage 无 reasoning 拆分字段**:thinking 文本直接计入 `completion_tokens`(实测 effort=high: completion=208 vs none: 13)。

### 5.7 有状态会话 API(超出 OAI)

| 操作 | 方法/路径 | 说明 |
|---|---|---|
| start | POST /v1/conversations | 创建会话 |
| append | **POST /v1/conversations/{id}** | 追加条目续聊(保持上下文) |
| get | GET /v1/conversations/{id} | 会话配置(store=false 的会话 GET → 404 code 3000) |
| list | GET /v1/conversations | 会话列表 |
| delete | DELETE /v1/conversations/{id} | 删除(204) |
| history / messages | GET /v1/conversations/{id}/history \| /messages | 历史条目 |
| restart | POST /v1/conversations/{id}/restart | 从 `from_entry_id` 重启,返回新会话(流式版 `restart#stream`) |
| 流式版 | start_stream / append_stream | 方法体加 `"stream": true` |

> ⚠️ **store 与 prompt caching 无关**(2026-08-19 A/B 实测钉死):store=true/false 两态同前缀连打,TTFT 同噪声分布(1.2~5.6s)无命中提速,usage 恒 `{prompt/completion/total}` 三字段且 prompt 全额计费,无任何 cached 字段。store=true 仅是会话存储(续聊省重发),不产生缓存折扣。渠道(glm-5-2)**不存在 prompt caching 能力**。

### 5.8 错误响应双 schema ⚠️

实测存在**两种不同形状**,消费端必须双路归一:

**业务错误**(400/404/422 等):
```json
{"object": "Error", "message": "...", "type": "invalid_request_error", "code": 3003}
```
或 pydantic 校验数组:
```json
{"object": "Error", "detail": [{"type": "extra_forbidden", "loc": [...], "msg": "Extra inputs are not permitted", ...}]}
```

**鉴权错误**(401):
```json
{"detail": "Invalid API Key"}
```

已知 code:3000=not found、3003=model not in use、3051=image input disabled、1300=rate limited(chat 端点)。

### 5.9 自定义工具与结构化输出实测矩阵(二轮深测)

**自定义工具:function calling 支持度 = 完美可用** ✅

| 用例 | 结果 |
|---|---|
| 单次 auto 调用 | ✅ 正确选中并生成合法参数 |
| **并行调用(一轮 3 城天气)** | ✅ 单响应 3 个 function.call,ID 唯一、参数各自正确,附前言文本 |
| **复杂参数完整性** | ✅ 嵌套对象/字符串数组/中文/integer 类型全保真(`reminder_minutes: 30` 数字不字符串化) |
| **完整 roundtrip**(call→function.result→最终答复) | ✅ 结果字段全部进入自然语言回复 |
| **流式工具调用** | ✅ function.call.delta 每个都带全量 name+tool_call_id,**拼接后 arguments 是合法 JSON** |
| tool_choice=none(带工具但恳求调用) | ✅ 0 次调用,纯文本回复(规格行为正确) |
| tool_choice=required(任何来源) | ⚠️ 洪水:单工具时 91 个**完全相同**call(2/2 复现,确定性);双工具时 26 个 call 参数各异(13+13 交替);数量随 max_tokens 线性(~tokens/11),completion_tokens 顶满上限,无文本输出(见 §7) |
| tool_choice=any / 指定函数对象形态 | ❌ 422 服务端收窄 |

**结构化输出:非流式 = 严格可靠** ✅(流式+推理有 bug)

| 用例 | 结果 |
|---|---|
| json_object | ✅ 合法 JSON |
| json_schema 扁平 strict | ✅ 键集合精确(additionalProperties:false 生效)、enum 命中、number 类型正确 |
| json_schema 嵌套+数组+unicode+bool | ✅ 全部保真 |
| **json_schema + stream + reasoning_effort=high** | ❌ **首 text delta 被重复发两次**(`"{\n"` ×2,2/2 复现)→ 拼接后非法 JSON。同路径 effort=none 正常;high+普通文本流式也正常。**判定为上游在 guided-JSON 前缀 + thinking 组合下的 bug** |
| json_schema + stream + effort=none | ✅ 8 delta 干净拼接,VALID |

---

### 5.10 token 计数对齐与默认行为(四轮测试)

**本地 tokenizer 与上游计费精确对齐** ✅(方案:本地加载 GLM 官方 tokenizer.json)

先厘清资源:**`zai-org/GLM-5.2` 在 HuggingFace 上是存在的**(此前误以为只有 GLM-4.x),且 5.2 词表扩容到 **154,820**(GLM-4.5 为 151,329,+3,491 tokens)。实测对上游计费的对齐度:

| 测试文本 | 上游 Δusage | `zai-org/GLM-5.2` | `zai-org/GLM-4.5` |
|---|---|---|---|
| 中英混合常见文本(增量法,信封抵消) | 292 | 291(**±1**) | 291(±1,幸运) |
| **罕见文本压力**(emoji 连发/rare 词) | 347 | 346(**±1**) | 382(**偏 35,≈10%**) |
| completion 直接比对(早测) | 22 | — | 21(±1) |

→ **定案:用 `zai-org/GLM-5.2` 的 tokenizer.json 作为兜底计数器**,与上游 ±1 对齐;GLM-4.5 在常见文本侥幸对齐、罕见文本显著漂移——不能用作兜底。
(Go 侧用 HF tokenizers 兼容库加载该 tokenizer.json 即可)

**max_tokens 缺省行为** ✅
- 不传 max_tokens 实测自然生成 **2729 tokens 完整响应**(1000 行数字全部出完),配合此前 40000-tokens 申请流式跑 540s+ 未截断 → **上游缺省无小额度封顶,行为 = 生成到自然停止**,与 OpenAI 语义一致
- 结论:**转换器缺省直通,不注入任何兜底值**

**图像输入精确报错**(400, code 3051)— http URL / base64 data URL / 纯图无文 三种形态全部:
```json
{"object":"Error","message":"Image input is not enabled for this model","type":"invalid_request_error","code":3051}
```

### 5.11 内置工具(built-in tools)全景(官方文档 + SDK + glm-5-2 实测)

**总表**(工具定义极其简洁,基本都是 `{"type": "<类型>"}` 一个字段):

| 类型 | 定义形态 | glm-5-2 实测 | 服务端行为与产出 |
|---|---|---|---|
| `web_search` | `{"type":"web_search"}` | ✅ | **服务端执行**,outputs 出现 `tool.execution` 条目(args 含 query/limit;info.result 为完整 JSON 结果:url/title/snippets),模型自动多轮检索后给带来源的正文;**usage.connectors.web_search 计调用次数**(实测 2 次检索) |
| `web_search_premium` | `{"type":"web_search_premium"}` | ✅ | 同上但结果质量更高(实测返回 AFP 等通讯社结构化条目) |
| `code_interpreter` | `{"type":"code_interpreter"}` | ✅ | 服务端沙箱执行 Python,info 含 `result`+`code`+`code_output`;实测 123456×789、π 20 位全对 |
| `image_generation` | `{"type":"image_generation"}` | ✅ | 后端是 Black Forest(FLUx 系),返回 Azure Blob **SAS 签名 URL**(时效约 1h),正文直接带 markdown 图片;connectors 计 1 |
| `document_library` | `{"type":"document_library","library_ids":["<UUID>"]}` | ❌ 不可用(我们无库) | 需要预先在平台上传文档库;假 ID 报 422 "not a valid UUID" |
| `connector` | `{"type":"connector","connector_id":"<id>","authorization"?}` | ⚠️ 静默无效 | 需要平台预注册的 connector(如 deepwiki 等);**假 id 不报错**,只是模型说"我没权限"空跑一轮——注意此静默失效不是错误 |
| `function` | OAI 风格自定义函数 | ✅ | (§5.9 全绿) |

**关键机制**:
1. **服务端执行**:与自定义 function(客户端执行)不同,内置工具由平台直接执行,outputs 多出 `tool.execution` 条目(含 name/arguments/info.result),模型在看到结果后继续生成
2. **确认机制(可选)**:`tool_configuration.requires_confirmation=["code_interpreter"]` → 服务端**不执行**,返回 `function.call` 待客户端回执 `tool_confirmations:[{"tool_call_id","confirmation":"allow"}]` 后才执行(实测验证)
3. `tool_configuration` 还支持 `include`/`exclude`(限定工具内子能力)
4. **可用性边界**(官方文档原话):web_search / web_search_premium / code_interpreter **仅 conversations/agents API,不在 chat/completions**;image_generation 两边都有;本测试 key 在 glm-5-2 上实测全部放行(含 premium)
5. **计费信号**:usage 新增 `connectors: {web_search: 2, ...}` 字段按调用计数(付费档按此计费;本 key 免费池实测直接可用)

**对转换器的价值**:**搜索/沙箱/生图三种服务端工具 = 白捡的能力**——免费 key 直通(含 premium 搜索);OAI Chat 客户端原生无法表达这类工具,桥侧开放方式见 [design.md](design.md) 内置工具开放设计。

**混用实测矩阵(五路,回答"CC 类 agent 场景会发生什么")**:

| 场景 | 结果 |
|---|---|
| 内置 web_search + 自定义 function 同请求 | ✅ 单响应同时产出 `function.call`(给客户端)与 `tool.execution`(服务端已搜完)+ 前言文本,互不干扰 |
| **CC 式复杂混合历史**(thinking 回传 + function.call + function.result + 文本 + 内置 web_search + effort=high) | ✅ 全通;outputs 呈"message.output → tool.execution → message.output……"**多阶段链**,thinking 块可多次出现(实测 2~3 块) |
| `web_search` + `web_search_premium` 同时携带 | ❌ **422 互斥**:"Can't add web_search and web_search_premium together"——注入时必须二选一 |
| `tool_choice: required` + 携带内置工具 | ❌ **422**:"Can't set 'tool_choice' to 'required' when using built in connectors"——**required 与内置工具互斥** |
| 高并发下偶发 HTTP 502(gateway "invalid response from upstream") | ⚠️ 一次性波动(4 项混测重跑全通),记录为**偶发**,非系统性 |

## 6. 并发与限额

| 项 | 实测值 |
|---|---|
| **免费 TPM 配额** | **500,000 tokens/分钟**(`x-ratelimit-limit-tokens-minute`) |
| 10 并发 | 20/20 成功,p50=2.5s |
| 50 并发 | 100/100 成功,p50=5s,p90=19s |
| 100 并发 | 200/200 成功,p50=9s,max=27s(**无 429,只有排队**) |
| 串行吞吐 | ~0.3 req/s(单请求 ~3.7s) |
| RPM 硬限制 | 未观察到(60 连发无 429) |

结论:免费额度非常宽裕(500k TPM),无 RPM 硬墙,高并发表现为排队延迟而非拒绝。

---

## 7. 已知坑与限制 ⚠️

| 现象 | 详情 |
|---|---|
| ~~30 秒生成超时~~ **(已证伪,勘误)** | **三轮反复实测推翻**:① 404 tokens 流式跑到 33.5s 正常完成;② 20000 行流式持续 540s+ 仍在出 token(被测试方超时打断,非服务端);③ 同 prompt 两次分别 12.6s/49.7s 结束。**不存在服务端墙钟截断上限**,此前"30s/4.7k tokens"是平台慢速期 + usage 归 0 bug 叠加造成的误判 |
| **真实的长输出形态** | 无截断墙,但:① 并发下 TTFT 可达 ~29s(排队);② 平台吞吐波动极大(同日实测 5~25 字符/秒,曾日 ~150 tokens/s);③ 非流式会一直挂到生成完(>480s 0 字节返回)→ **代理读写超时要给足 ≥600s,客户端大输出尽量用流式** |
| **usage 偶发归 0(flaky)** | 上游**正常是会返回真实用量**的(prompt/completion/total 三字段);但偶发三个数全变 0——同 prompt 隔天复现(3/5),今天并发复测又全正常(3/3)。与截断无关联,是上游上报 bug。→ 转换侧修复策略见 [design.md](design.md) 修复件清单 |
| **`tool_choice: "required"` 产生调用洪水** | **多轮复测钉死**:单工具 mt=1000 → 2/2 次均为 91 个**完全相同**的 call(确定性);单工具 mt=100 → 9 个;双工具 mt=300 → 26 个 call(weather/time 各半、**参数每次不同**城市轮换);completion_tokens **精确顶满 max_tokens**,无文本输出。本质:required 约束下模型每"步"都被强制调工具,无结果时只能继续调,直到预算耗尽。→ 转换侧熔断策略见 [design.md](design.md) 修复件清单 |
| **截断无任何标记** | max_tokens 精确截断时 done 正常返回、无 length 标记、无 truncated 字段,只能靠 `completion_tokens >= max_tokens` 猜测 |
| **guided-JSON + 流式 + high:正文开头重复** | json_schema/json_object 流式 + reasoning_effort=high 时,正文开头的 `{` 被发两遍——且**分块边界每次不同**(见过 `"{\n"`+`"{\n"` 全等双份,也见过 `"{\"`+`"{"` 错位双份),拼接即非法 JSON(多轮复现)。effort=none 与纯文本流式均不复现。修复方案见 [design.md](design.md) 修复件清单 |
| **guided-JSON + 非流式 + high:同样重复**(2026-08-19 新增) | 桥默认注入 reasoning high(D-35)后,E2E 实测**非流式** json_object/json_schema 响应正文同样以 `{\n{` 重复开头(2/2 复现);折叠器对该面同规适用(见 design.md D-38) |
| **high 模式下思考可吃满 max_tokens 致正文为空** | reasoning_effort=high + max_tokens 较小时,思考 delta 可占满全部额度(实测 300 全用完,正文 0 delta)——非 bug,是预算分配;客户端需给足 max_tokens |
| OAI 端点不可用 | glm-5-2 在 `/v1/chat/completions` 的 TPM=0,直接 429 |
| temperature 范围窄 | 仅 0~1(OAI 标准 0~2) |
| SDK 枚举 > 服务端实际 | `reasoning_effort`、`tool_choice` 服务端收窄,**不能信 SDK 枚举作为事实** |
| 图像输入禁用 | GLM-5-2 是纯文本模型(非多模态),平台 vision=false;触发报 **400 code 3051**。→ 转换侧占位清洗方案见 [design.md](design.md) 修复件清单 |

---

## 8. OAI 协议兼容性总结

```
OAI 采样参数      ████████████░░░░   ~70%(白名单子集,无并行/无 n/无 logprobs)
OAI 工具调用      ██████████████░   ~90%(定义/调用/回传/流式全通,缺 any/指定函数)
OAI 消息格式      ████████████░░░░   ~70%(块数组/多轮通,缺 system role/tool role/图像)
OAI 响应结构      ████░░░░░░░░░░░░   ~20%(完全自定义 conversation.response,无 finish_reason)
OAI 流式          ██████████░░░░░░   ~50%(SSE 但事件结构自定义,双行 event+data)
有状态会话        ████████████████   100%(OAI 无此能力,conversations 全支持)
```

**一句话:不能直接用 OAI 客户端库对接,需要按 conversations 协议封装;能力上除了并行工具调用和图像,主流的 agentic 功能(工具循环/流式/JSON 约束/推理)全部可用。**

---

## 9. 参考与研究产物

- 官方 API 文档:`https://docs.mistral.ai/api/`(chat completions 参数参考;conversations 端点页含 guardrails/restart 细节)
- 模型页:`https://docs.mistral.ai/models/zai-glm-5-2`
- SDK 源码:`github.com/mistralai/client-python`,本轮实际比对的文件:
  - 请求侧:`conversationrequest.py` / `completionargs.py` / `messageinputentry.py` / `toolchoice.py` / `toolchoiceenum.py` / `reasoningeffort.py`
  - 响应侧:`conversationresponse.py` / `conversationusageinfo.py` / `messageoutputentry.py` / `functioncallentry.py`
  - 流式侧:`conversationevents.py` / `ssetypes.py` / `messageoutputevent.py` / `functioncallevent.py` / `responsedoneevent.py` / `outputcontentchunks.py` / `thinkchunk.py`
- **上游接入信息(开发/调试用)**:
  - 端点:`https://api.mistral.ai/v1/conversations`
  - 模型:`glm-5-2`
  - 测试 key:由项目所有者持有,不入代码/文档/仓库;运行时以 `MISTRAL_KEY` 环境变量注入测试脚本
- **研究过程产物不归档**:16 个 glm52_*.py 探针脚本、tokenizer 副本、流式 dump 等中间产物未入库(属过程件;其中 tokenizer.json 转正为桥资产 `internal/tokenizer/glm52.json`,流式 dump 转正为测试素材 `internal/convert/testdata/gap_stream.jsonl`)

---

> **本文档仅记录 Mistral 平台 glm-5-2 渠道的协议事实。**
> 本项目(mistral-bridge)的桥设计与决策记录见同目录 [design.md](design.md)。
