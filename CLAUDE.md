# mistral-bridge

Mistral 平台 GLM-5.2 的 OAI Chat Completions 协议转换转发器(Go 单二进制,纯静态 CGO_ENABLED=0)。对外模型名 `glm-5.2`(上游真名 glm-5-2/别名 zai-glm-5-2 兼容收,统一归一发上游)。
仅供 Claude Code 在此项目工作时遵循。**规则/结构定生死；动态实验进展属 git 与测试脚本,不写在这里**。

---

## 0. 文档地图

| 需求 | 去读 |
|---|---|
| 快速开始 / 部署 / 配置表 | `README.md` |
| 桥为什么这样设计、修复件细节、Docker 安全、D-20+ 现场定案 | `docs/design.md` |
| 上游渠道协议事实(端点/字段/坑位/实测数据) | `docs/Mistral-GLM5.2-API协议研究报告.md` |

---

## 1. 铁律边界

| # | 约束 | 原因 |
|---|---|---|
| R1 | **桥不持有 key**:`Authorization` 从下游原样透传上游 | 渠道生命周期归 newapi，桥纯转发 |
| R2 | **`store=false` 恒等**:上游+桥两侧均不留会话 | 协议纯翻译点 |
| R3 | **日志红线永不记**:Authorization 值、请求 body 全文、large 用户数据 | 安全隐私 |
| R4 | **不 clamp 参数**：上游拒收(temperature>1、max_tokens>1M)，透传其 422、规范化为 400 | 不改用户语义 |
| R5 | **tool_calls.index 按新 tool_call_id 出现顺序分配**:绝不用上游 output_index | 上游槽位被 thinking/text 混占 |
| R6 | **绝不编造 usage / reasoning_tokens**:usage=0 时走 tokenizer 精确兜底，tokenizer 不可用时如实回传原值或整体省略;`cached_tokens` 恒如实回 0(渠道无缓存,D-34) | 计费信号不可伪造 |
| R7 | **测试 max_tokens≥32000**:thinking 可能先吃空预算导致正文截断出现假 bug | 历史教训 |

---

## 2. 结构与模块（一看便知去哪里改)

```
mistral-bridge/
├── cmd/mistral-bridge/main.go        # 入口:env → logging → proxydial → http.Server → 信号退出
│
├── internal/
│   ├── server/server.go              # 路由(/health /v1/models /v1/chat/completions)+ CORS + 256MB body 上限
│   │
│   ├── convert/                      # 桥业务核心(全部转换逻辑)
│   │   ├── chat.go                   #   HTTP handler:鉴权透传 → 转换 → 上游 → 错误归一 → 日志
│   │   ├── request.go                #   OAI → conversations 参数映射 + messages→inputs + 内置工具治理
│   │   ├── response.go               #   conversations → OAI 非流式映射 + finish_reason 合成
│   │   ├── stream.go                 #   SSE 状态机(双行进/单出行/即 flush/结束保证)
│   │   └── errors.go                 #   双 schema 错误归一化
│   │
│   ├── repair/                       # 协议修复件
│   │   ├── jsonfold.go               #   ① guided-JSON 流式开头 `{` 重复折叠(三形态)
│   │   └── flood.go                  #   ③ tool_choice=required 全同调用洪水熔断
│   │
│   ├── tokenizer/                    # 修复件②:usage=0 精确计数兜底
│   │   ├── bpe.go                    #   自研字节级 BPE(GLM Split regex 手工扫码器 + merges 贪心)
│   │   ├── glm52.json                #   embed 进的 HF tokenizer.json(20MB资产)
│   │   └── *_test.go / testdata/     #   黄金对齐集(与 HF Docker tokenizers 逐样本一致)
│   │
│   ├── config/config.go              # env 解析 + 默认值 + 校验
│   ├── config/builtin_tools.go       # BUILTIN_TOOLS 中文宽容解析(全角/顿号/【】/弯引号归一)
│   │
│   ├── logging/logging.go            # slog JSON handler + lumberjack 轮转(logs/mistral-bridge.log)
│   │
│   └── proxydial/                    # 出口四态代理:direct|system|custom(direct)|custom(via system)
│       ├── proxydial.go              #   平台无关逻辑(环境变量检测 + 四态分支)
│       ├── proxydial_windows.go      #   Windows 注册表 IE 代理检测(build tag)
│       └── proxydial_other.go        #   非 Windows 桩
│
├── test/e2e/                         # 真实上游/容器集成测试(max_tokens 约定全部 ≥32000)
│   ├── e2e.py                        #   A/B/C/D 34 项协议功能 + 并发压测 + 高上下文 + 缓存探测
│   ├── deep_probe.py                 #   T1-T4 深挖(100K/200K/工具并发/空回探测)
│   ├── search_quality.py             #   双搜索质量对比(web_search vs web_search_premium)
│   └── docker_suite.py               #   容器内同网套件(7 项业务真实调用)
│
├── docs/
│   ├── design.md                     # 桥设计档案(修复件细节、Docker 安全、D-20+ 现场定案)
│   └── Mistral-GLM5.2-API协议研究报告.md  # 渠道协议档案(上游客观事实,勿涉桥实现)
│
├── Dockerfile                        # 多阶段:golang:1.26-alpine → distroless nonroot
└── compose.yaml                      # 零配置一键;init 容器 mkdir/chown logs;共享网络默认整块注释,生产取消注释以 external 接入 new-api_default
```

---

## 3. 常用工作命令

```bash
# 修改后必跑(顺序守死)
go build ./... && go vet ./... && gofmt -l . && go test ./...

# 单测按包/按 case 过滤
go test ./internal/convert/ -run "TestA" -v     # 转换 A 类断言
go test ./internal/repair/ -v                    # 折叠/熔断器细节

# E2E(需桥在跑且 MISTRAL_KEY 环境变里注入)
go run ./cmd/mistral-bridge &                     # 起桥
MISTRAL_KEY=<key> python test/e2e/e2e.py              # 全量 34
MISTRAL_KEY=<key> python test/e2e/e2e.py --skip-slow  # 快测

# 容器部署(参考「能快速完成 production 部署」又清晰又安全)
docker compose up -d --build
docker compose logs -f bridge          # 实时 access 日志
docker compose down                    # 不删外部创建的网络
```

---

## 4. 踩坑速查

| 坑 | 避免方式 |
|---|---|
| 中文标点码点写成字形会被编码事故绑架(U+FF8C 伪装成全角逗号) | **所有中文标点一律 `\uXXXX` 转义书写代码/测试** |
| 测试 max_tokens 给 60 等小值，推理 high 会把预算吃光正文截断为空白 | **测试一律 ≥32000**,结构化场景 64000 |
| API 直连端点比 Playground 严：message.output **不允许带** prefix 字段 | 上游 prefix 只留 message.input 端 |
| tool_choice: required 遇内置工具会被上游 422 | 桥内 required 遇内置工具自动降 auto(违反义从 required 洪水Vì) |
| 上游 usage 偶发(研发期实测出现过,是真实 bug,**是问题**;修复件②就是为此储备的) | tokenizer 兜底,日志打 `usage_repaired:true` |
| 日志爆盘 / 类型错误无法快速暴露到控制台 | `LOG_CONSOLE=true` 留给磁盘臃肿不做主要值守 |
| Windows 文件系统大小写不敏感 | 新建文件 ls 验证现名后 Write 用全小写 |

---

## 5. Claude 在本项目的工作方式

1. **改动前看「铁律边界」**:如果需求触到鉴权/key管理/计费/会话，直接回绝或上报设计面
2. **改动后必须**:运行第 3 节的完整质量门
3. **日志敏感路径检查**:任何加日志的地方搜一下有无 key/value 泄露可能（参数 dumps、http body 打印、错误消息拌回原始值）
4. **`git add` 不运行**:仓库写入类型的 git 命令全程由用户手动执行，Claude 只报告 status/diff

---
