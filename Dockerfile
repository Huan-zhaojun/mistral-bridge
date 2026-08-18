# syntax=docker/dockerfile:1
# mistral-bridge 多阶段构建:官方源直连,纯静态二进制
# FROM 用 ARG 必须在首个 FROM 之前统一声明
ARG BUILD_IMAGE=golang:1.26-alpine
ARG BASE_IMAGE=gcr.io/distroless/static-debian12:nonroot

# ---- build stage ----
FROM ${BUILD_IMAGE} AS build
WORKDIR /src

# 依赖层缓存:先拉依赖再拷源码(GOPROXY 默认官方 proxy.golang.org,可用 --build-arg 覆盖)
ARG GOPROXY=""
ENV GOPROXY=${GOPROXY}
COPY go.mod go.sum ./
RUN go mod download

# 纯静态编译(CGO 关闭,trimpath+strip 去符号表)
# tokenizer 资产经 embed 编译进二进制(包内 glm52.json),无需运行时外置
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mistral-bridge ./cmd/mistral-bridge \
    && mkdir -p /stage/logs && cp /out/mistral-bridge /stage/mistral-bridge

# ---- runtime stage ----
# distroless nonroot:无 shell 无包管理器(攻击面最小);自带 nonroot 用户 uid:65532
FROM ${BASE_IMAGE}
WORKDIR /app
COPY --from=build --chown=65532:65532 /stage/mistral-bridge /app/mistral-bridge
# 日志目录属主预置:named volume / bind mount 首次挂载继承镜像内属主 → 免宿主侧任何权限配置
COPY --from=build --chown=65532:65532 /stage/logs /app/logs

USER 65532:65532
EXPOSE 8080
# 零配置启动:全部配置走环境变量(均有默认值);零 key 依赖——Authorization 由下游透传
ENTRYPOINT ["/app/mistral-bridge"]
