# mistral-bridge

Mistral 平台 glm-5-2 的 OAI Chat Completions 协议转换转发器,Go 单二进制实现(CGO_ENABLED=0,distroless nonroot 容器)。把 `/v1/conversations` 包装成标准 `/v1/chat/completions`,让任意 OpenAI 客户端可直连。**桥不持有 key,不管理任何东西,只做纯粹的格式翻译与修复**。

## 为什么需要桥

| 项目 | 现状 |
|---|---|
| OAI 兼容端点 `/v1/chat/completions` | glm-5-2 TPM=0,直接 429 不可用 |
| `/v1/conversations` 端点 | 可走(TPM 500k 宽裕),但与 OAI 差异巨大:无 finish_reason、SSE 10 种事件、无 system role、图像 400、usage 偶发归零、required 会产全同调用洪水 |

所以要桥。

## 快速开始(本机开发)

```bash
go run ./cmd/mistral-bridge

curl -X POST http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer $MISTRAL_KEY" -H "Content-Type: application/json" \
  -d '{"model":"glm-5-2","messages":[{"role":"user","content":"Say OK"}]}'
```

忘记了:Authorization 头原样透传;无环境依赖可启动。

## Docker 部署

```bash
docker compose up -d --build      # 零配置一键
# newapi 渠道:base_url = http://mistral-bridge:8080/v1    key=填上游 Mistral key
```

- 运行在 nonroot(65532) 用户、只读根 FS、`no-new-privileges`、`cap_drop ALL`、**不接宿主端口**
- 日志落宿主机 `./logs`(bridge-init 一次性容器自动 mkdir/chown,宿主零命令)——`docker compose logs -f bridge` 实时观看
- 加入网络 `new-api_default`,不存在本 compose 会自动创建;down 不删除外部创建的网络
- 容器内无外置依赖:tokenizer 资产 embed 进二进制(20MB),读写只在 `./logs` 与 `/tmp`

## 配置(全可选 env)

| Env | 默认 | 说明 |
|---|---|---|
| `LISTEN_ADDR` | `:8080` | 监听地址 |
| `UPSTREAM_BASE` | `https://api.mistral.ai` | 上游基址 |
| `UPSTREAM_TIMEOUT_S` | `600` | 上游整体读超时(非流式可挂 >480s) |
| `BUILTIN_TOOLS` | `""`(不注入) | 可选默认开启:web_search / web_search_premium / code_interpreter / image_generation |
| `PASS_REASONING` | `true` | 历史 thinking 回传为 reasoning_content(可关省 context) |
| `PROXY` | `""` | 自定义代理(socks5://、http://、裸 host:port) |
| `SYSTEM_PROXY` | `auto` | auto=自动检测(env > Windows 注册表);off=完全直连 |
| `LOG_LEVEL` | `info` | debug/info/warn/error |
| `LOG_DIR` / `LOG_CONSOLE` / `LOG_FILE` | `logs` / `true` / `true` | 日志输出三通道 |

**内置工具默认不注入**:客户端 `tools` 携带就透传,配置项只在需要默认开启时使用。开普通搜索和 premium 搜索永远二选一(premium 优先);`tool_choice: required` 遇内置工具自动降 auto。

中文输入法宽容解析:全角逗号 `,`、顿号 `、`、`【】`、弯引号都原样接受(未知项 WARN 并过滤,不阻断启动)。

## 测试

```bash
go vet ./... && gofmt -l . && go test ./...     # L0-L2 每次修改必跑,全绿为底线

# L3 真实上游 E2E(需桥在跑):
MISTRAL_KEY=<key> python test/e2e/e2e.py           # 全量(34 项)
MISTRAL_KEY=<key> python test/e2e/e2e.py --skip-slow   # 快测仅功能+负面
MISTRAL_KEY=<key> python test/e2e/deep_probe.py t3    # 深层(空回/高上下文/工具并发/缓存探测)
MISTRAL_KEY=<key> python test/e2e/search_quality.py # 双搜索质量对比

# L4 在容器内跑(模拟生产):
docker compose up -d --build
docker run --rm --network new-api_default -v "$PWD/test/e2e:/e2e" python:3.13-slim python /e2e/docker_suite.py   # 7 项业务绿
```

max_tokens 测试约定:**所有脚本一律 32000 起步,schema/结构化场景 64000**——历史教训:不拉满会因思考吃满预算报假错。

## 项目结构

```
mistral-bridge/
├── cmd/mistral-bridge/main.go       入口(env→log→proxydial→http.Server)
├── internal/
│   ├── config/builtin_tools.go      BUILTIN_TOOLS 中文宽容解析(全角/顿号/【】/弯引号归一)
│   ├── convert/                     chat.go(request/response/stream/errors 转换)+ 全部 _test.go
│   ├── logging/                     slog JSON + lumberjack 轮转(logs/mistral-bridge.log)
│   ├── oai/                          OAI 类型与 messages 解析
│   ├── mistral/                      conversations 类型
│   ├── proxydial/                    出口四态代理(direct|system|custom|chain)
│   ├── repair/                       jsonfold.go(fold ①) + flood.go(required 熔断③)
│   └── tokenizer/                    自研字节级 BPE + glm52.json embed
├── test/e2e/                         真实上游测试脚本(见「测试」节)
├── docs/
│   ├── design.md                           桥自身的设计与决策(架构/修复件/D-20+ 现场新定案)
│   └── Mistral-GLM5.2-API协议研究报告.md   渠道协议事实(端点/字段/坑位)
├── Dockerfile / compose.yaml / .dockerignore / .env.example
└── CLAUDE.md                               Agent 规则文件
```

## 文档导航

| 想了解什么 | 去读 |
|---|---|
| 渠道协议的客观事实(端点/字段/实测数据) | [docs/Mistral-GLM5.2-API协议研究报告.md](docs/Mistral-GLM5.2-API协议研究报告.md) |
| 桥怎么设计/为什么这里这样/决策记录 D-20+ | [docs/design.md](docs/design.md) |
| AI 协作时的命令/规则/结构 | [CLAUDE.md](CLAUDE.md) |

## 升级/重构提示

- 如果上游修了 JSON 重复 bug,折叠器可移除(access 日志搜 `json_dup_folded` 归零即可下线)
- 500k TPM 免费池够用,桥内任何新增能力都不要引入重试/记账逻辑,边界不松
