<div align="right">
  <a href="README.md">English</a> | <strong>简体中文</strong>
</div>

# Command Code Go CLIProxyAPI (CPA) 插件

专为 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) (CPA) 设计的高性能 C-shared 动态库插件，将 Command Code Go ($1/月套餐) LLM 生态系统无缝接入标准 OpenAI 兼容端点，无需在后台额外常驻运行 Command Code CLI。

---

## 特性

- **全协议与全能力支持**：全面支持流式对话（SSE）、非流式对话、Tool Calls（函数调用）、Reasoning Content（思考过程 / 思考 Token）以及多模态图片识别。
- **独立模型命名空间**：所有模型均挂载至 `cmdcode-go/<model_name>` 独立命名空间（例如 `cmdcode-go/deepseek-v4-flash`、`cmdcode-go/Kimi-K3` 等），省略中间的模型厂商段，保留 `cmdcode-go/` 前缀以区分其他渠道。
- **零配置灵活凭据识别**：支持在 CPA 配置文件中显式配置 `api_key`、读取 `COMMANDCODE_KEY` 环境变量，或自动读取本地已有 `cmd login` 登录态（`~/.commandcode/auth.json`）。
- **动态模型清单与云端自动同步**：内置编译期模型底账，同时默认自动从云端同步最新模型列表（通过 GitHub Actions 追踪官方 CLI 更新），亦支持指定本地 JSON 文件覆盖。
- **高韧性流式架构**：基于有界 NDJSON 状态机和受控的 Host 回调生命周期设计，保证在动态配置重载或服务关闭时优雅注销工作线程，杜绝连接泄漏与内存冲突。

---

## 精选可用模型

本插件自动发现并路由 Command Code Go 套餐所包含的全部授权模型，通过 `cmdcode-go/<model_name>` 短 ID（省略模型厂商）进行调用：

| 模型名称 | CPA 模型 ID | 上下文参考值 | 能力特性与说明 |
| :--- | :--- | :--- | :--- |
| **DeepSeek V4 Flash** | `cmdcode-go/deepseek-v4-flash` | 1M | 极速混合注意力推理，输出思考过程 |
| **DeepSeek V4 Flash Fast** | `cmdcode-go/deepseek-v4-flash-fast` | 1M | 超低延迟极速推理 |
| **Kimi K3** | `cmdcode-go/Kimi-K3` | 1,048,576 | 高吞吐深度推理与 Agentic 工具任务 |
| **MiMo V2.6 Pro** | `cmdcode-go/mimo-v2.6-pro` | 1,048,576 | 多模态模型；推理控制方式取决于服务商 |
| **GLM-5.3** | `cmdcode-go/GLM-5.3` | 1M | 推理与工具调用模型 |
| **GPT-6 Luna** | `cmdcode-go/gpt-6-luna` | 1.05M | 长上下文多模态模型 |

对外 ID 和 Name 统一使用 `cmdcode-go/<model_name>`，例如 `cmdcode-go/Kimi-K3`；请求及 `models` / `disable_models` 配置均使用此格式，支持 `(high)` 等推理等级后缀。不再接受带厂商的旧 ID 或无前缀裸名。发送给上游的 ID 仍为原始 `moonshotai/Kimi-K3`；含有重复短名的目录会被拒绝。

> 当前 Go 名单参见 [models.json](models.json)。套餐归属取自官方 Go 页面，能力参数来自 models.dev 参考资料，不代表已验证的网关限制。缺失限额统一采用标注为 fallback 的 262,144 上下文 / 65,536 输出默认值。详见[目录生成与匹配说明](docs/MODELS.md)。

---

## 安装与部署

### 1. 下载预编译动态库

项目发布的 [Releases](https://github.com/megumin31/cpa-cmdcode-go/releases) 均在原生验证门禁通过后自动打包（提供 Linux `amd64` 与 `arm64` 预编译包）。下载对应架构的压缩包，校验 SHA-256 后解压出 `cmdcode-go-v0.1.0.so` 并放置于 CPA 的 `plugins/` 目录中：

```bash
cp cmdcode-go-v0.1.0.so /path/to/cliproxyapi/plugins/
chmod 700 /path/to/cliproxyapi/plugins/cmdcode-go-v0.1.0.so
```

> **注意**：请勿重命名该 `.so` 文件，CPA 要求严格保留版本号命名。详见 [发布说明文档](docs/RELEASE_zh.md)。

### 2. 源码本地编译

环境要求：Go 1.26.9 及 C 编译器（GCC 或 Clang）：

```bash
git clone https://github.com/megumin31/cpa-cmdcode-go.git
cd cpa-cmdcode-go
mkdir -p build
LIBRARY="$(cd go && go run ./cmd/buildmeta --field filename)"
(cd go && CGO_ENABLED=1 go build -buildmode=c-shared -o "../build/$LIBRARY" .)
cp "build/$LIBRARY" /path/to/cliproxyapi/plugins/
chmod 700 "/path/to/cliproxyapi/plugins/$LIBRARY"
```

本地源码构建默认为未发布的 `0.0.0`（`build_tag=dev`）。给测试通过的提交推送新版本标签后，CI 自动注入版本并创建 Release。详见[标签发版说明](docs/RELEASE_zh.md)。

### 3. 配置 CPA

在 CPA 的配置文件 `config.yaml` 中添加插件声明：

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    cmdcode-go:
      enabled: true
      priority: 1
      api_key: "YOUR_COMMAND_CODE_KEY" # 若主机已执行过 cmd login，此项可省略
```

### 4. 重启并验证 CPA

```bash
systemctl --user restart cliproxyapi
# 或直接重启 CPA 主进程
```

调用模型列表端点确认模型是否已正确挂载：
```bash
curl -s http://localhost:8317/v1/models -H "Authorization: Bearer <your-cpa-api-key>" | jq '.data[].id'
```

---

## 配置参数

配置项位于 `plugins.configs.cmdcode-go` 下：

| 配置项 | 类型 | 说明 | 默认值 |
| :--- | :--- | :--- | :--- |
| `api_key` | string | Command Code 账号 Key（`user_...`） | 优先读取环境变量 / CLI 登录态 |
| `models` | list | 允许启用的模型列表（白名单） | 全部启用 |
| `disable_models` | list | 禁用的模型列表（黑名单） | 无 |
| `models_refresh_interval` | string | 远程模型清单刷新间隔 | `"6h"`（设为 `"0"` 则仅使用内置编译清单） |
| `models_file` | string | 本地模型清单 JSON 文件路径 | 无（设置后优先于远程 URL） |
| `models_url` | string | 远程模型清单 JSON 地址 | 本仓库 `main` 分支的 `models.json` |

---

## 调用示例

### 1. 对话补全（Chat Completions，`/v1/chat/completions`）

```bash
curl -N http://localhost:8317/v1/chat/completions \
  -H "Authorization: Bearer <your-cpa-api-key>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "cmdcode-go/deepseek-v4-flash",
    "messages": [{"role": "user", "content": "用 Go 语言写一个快速排序算法。"}],
    "stream": true
  }'
```

### 2. 工具调用（Tool Calling）

客户端传入的 Tools 会被自动转换并由模型原生调用：

```bash
curl http://localhost:8317/v1/chat/completions \
  -H "Authorization: Bearer <your-cpa-api-key>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "cmdcode-go/Kimi-K3",
    "messages": [{"role": "user", "content": "计算 123 * 456 的结果。"}],
    "tools": [{
      "type": "function",
      "function": {
        "name": "multiply",
        "description": "计算两个数的乘积",
        "parameters": {
          "type": "object",
          "properties": {
            "a": {"type": "number"},
            "b": {"type": "number"}
          },
          "required": ["a", "b"]
        }
      }
    }],
    "tool_choice": "auto"
  }'
```

### 3. 思考过程（Reasoning Tokens）

对于支持推理的模型（如 DeepSeek 和 Kimi），流式输出中会以 `reasoning_content` delta 的形式发射思考过程：

```bash
curl -N http://localhost:8317/v1/chat/completions \
  -H "Authorization: Bearer <your-cpa-api-key>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "cmdcode-go/deepseek-v4-flash",
    "messages": [{"role": "user", "content": "求解方程：x^2 + 5x + 6 = 0"}],
    "stream": true
  }'
```

---

## 架构设计与 CI 验证

项目核心模块边界清晰，各司其职：

- **C-ABI 适配层 (`go/main.go`, `go/abi_runtime.go`)**：管理 C 内存分配、宿主回调桥接、`dlclose` 跨生命周期安全重载，以及导出符号（`cliproxy_plugin_init`、`cliproxyPluginCall`、`cliproxyPluginFree`、`cliproxyPluginShutdown`）。
- **插件引擎 (`go/plugin.go`, `go/dispatch.go`, `go/executor.go`)**：组合根负责 RPC 分发、任务注册、请求快照与优雅停机。
- **配置中心 (`go/config.go`)**：线程安全 YAML 解码、校验、原子更新与防御性拷贝。
- **模型注册表 (`go/models_remote.go`, `go/models.go`)**：不可变模型快照索引，具备单飞背景刷新与编译期回退能力。
- **网关客户端 (`go/gateway_client.go`)**：可复用 HTTP 传输、认证头注入、超时与连接池管理。
- **事件解码器 (`go/event_decoder.go`)**：流式与缓冲响应共享的有界 NDJSON 状态机。

每次分支 push 与 PR 都会触发 Linux amd64/arm64 原生质量门禁检查：代码格式、vet、单元测试、race 竞态检测、Python fixtures 以及 C ABI 动态重载测试。详见 [架构设计文档](ARCHITECTURE.md) 与 [CI 规范文档](docs/CI.md)。

---

## 开源协议

[AGPL-3.0](LICENSE)

### 会话复用

只有可靠的显式会话标识、CPA 调用者隔离信息和所选账户标识齐全时，才复用同一上游会话。
缺失或冲突时使用独立请求；同会话并发续轮返回 HTTP 409，等待上一轮结束后可重试。
缓存仅保存在当前插件进程，闲置 24 小时过期、最多 4096 项；重启和多实例不保证延续。
完整限制、字段及测试说明见 [会话标识与生命周期](docs/SESSIONS.md)。
