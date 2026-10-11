# Sail Gateway

[Sail Research](https://www.sailresearch.com/) 多 Key 轮询代理，带 Web 管理后台。

- 兼容 OpenAI Chat Completions / Responses 和 Anthropic Messages 接口
- 多个 Sail key 轮询或固定使用，失败自动换 key；额度用完、key 失效自动报废
- 按模型配置思考强度，客户端可直接选 `模型名-high` 这类变体
- 完成窗口 asap / balanced / flex 可全局或按模型设置（默认 balanced）

## 编译

需要 Go 1.21+：

```bash
# Linux / macOS
./build/build.sh

# Windows
build\build.bat
```

## 启动

```bash
cp config.example.txt config.txt   # 改 WEB_AUTH_TOKEN
./main
```

1. 浏览器打开 `http://服务器地址:9090`，用 `WEB_AUTH_TOKEN` 登录
2. 添加 Sail key（`sk_` 开头），在「设置」页生成客户端 API Key
3. 客户端 Base URL 填 `http://服务器地址:9090/v1`，API Key 填刚生成的那个

数据保存在 `data/` 目录（key、模型设置、配置）。
