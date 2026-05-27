# Agent2API

Agent2API 是一个高性能、轻量级的网关服务，旨在将支持 **Agent Client Protocol (ACP)** 的底层智能体（Agents）封装并转换为标准的 **OpenAI API** 与 **Claude Messages API** 格式。

## 主要特性

- **双协议转换**：支持将标准的 OpenAI `/v1/chat/completions` 请求以及 Anthropic Claude `/v1/messages` 请求实时转换为底层 ACP 有状态会话交互。支持**流式 (Streaming)** 与**非流式 (Non-Streaming)** 请求。
- **精确会话路由与复用**：基于历史消息的整体哈希匹配（N-1 路由），实现无状态请求在有状态底层 Agent 上的秒级 Session 状态复用。
- **多模态与视觉支持**：
  - **OpenAI 格式**：支持解析 `data:image/` Base64 数据 URI，也支持远程图片 URL（网关会自动下载并转码供智能体使用）。
  - **Claude 格式**：支持解析包含 `source` 对象的 Base64 图片 block。

## 安装

在 Releases 页面下载最新版本的可执行文件。

## 配置说明

使用前，需要准备一个配置文件。通过以下命令打印默认配置模板：

```bash
./agent2api -template > config.yaml
```

**`config.yaml` 示例解释：**

```yaml
listen: "0.0.0.0:8080" # 服务监听地址
api_keys:
  - "sk-your-gateway-key" # 客户端的 API Key

agents:
  - name: "codex"
    command: "E:/bin/codex-acp.exe"
    cwd: "E:/tmp/workspace/codex"
    models:
      - name: "gpt-5.4-mini-low"
        max_idle_sessions: 1
        options:
          - model: "gpt-5.4-mini"
          - reasoning_effort: "low"

  - name: "kiro"
    command: "E:/bin/kiro-cli.exe"
    cwd: "E:/tmp/workspace/kiro"
    args: ["acp"]
    models:
      - name: "deepseek-3.2"
        max_idle_sessions: 1
        extra_args: ["--model", "deepseek-3.2"]

  - name: "cursor"
    command: "E:/bin/cursor-agent.cmd"
    cwd: "E:/tmp/workspace/cursor"
    args: ["acp"]
    models:
      - name: "auto"
        max_idle_sessions: 1
        options:
          - model: "default[]"
          - mode: "ask"
```

## 运行网关

网关默认读取当前工作目录下的 `config.yaml`。

```bash
./agent2api

# 使用 -config 参数指定自定义配置文件
./agent2api -config /path/to/your/config.yaml

# 开启调试模式以打印详细的 agent 通信流和可配置 options 信息
./agent2api -debug
```

## API 使用示例

服务启动后，可以把它当做 OpenAI 或 Claude 服务器来使用，兼容各自生态的 UI 和 SDK 工具。

### 1. OpenAI 格式 (Chat Completions)

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer sk-your-gateway-key" \
  -d '{
    "model": "cursor/auto",
    "messages": [
      {
        "role": "user",
        "content": "hello"
      }
    ]
  }'
```

### 2. Claude 格式 (Messages API)

支持使用 Anthropic Claude 官方 SDK 或是 curl 直接调用：

```bash
curl http://localhost:8080/v1/messages \
  -H "Content-Type: application/json" \
  -H "x-api-key: sk-your-gateway-key" \
  -d '{
    "model": "cursor/auto",
    "max_tokens": 1024,
    "messages": [
      {
        "role": "user",
        "content": "hello"
      }
    ]
  }'
```
