# YakAIGateway

個人 AI Gateway：把 Claude Code、Codex 等本地客戶端轉發到 `aibalance.yaklang.com`。網關把所有請求頭與請求特徵改寫成 yaklang 官方客戶端的樣式，並在網關側統一處理 memfit TOTP 認證——本地客戶端無需任何 yaklang 特有邏輯。純 Go stdlib，SSE 流式透傳（逐塊 flush）。

## 客戶端指紋

上游免費模型有「Memfit 免費客戶端風控」，共兩層：

1. **請求頭/傳輸特徵**：網關對每個上游請求強制覆寫成 yaklang 客戶端樣式，客戶端自帶的同名頭一律丟棄：

| 特徵 | 值 |
|---|---|
| `Authorization` | `Bearer free-user`（`-api-key`） |
| `User-Agent` | 默認不發送（與 yaklang 對話請求一致） |
| `Accept` | `application/json` |
| `Accept-Encoding` | `gzip, deflate, br`（響應 gzip/deflate 由網關解壓後再轉發） |
| `X-Yak-Version` / `X-Yak-Build-Time` | `1.4.9-beta6` / 對應官方構建時間 |
| `X-Yak-AI-Model-Usage-Type` | 默認 `lightweight` |
| `X-User-Id` | 持久化 UUID v4（yaklang 的客戶端標識，上游按此分桶） |
| `X-Memfit-OTP-Auth` | `memfit-*` 模型自動注入 |
| HTTP 版本 | 僅 HTTP/1.1（與 yaklang lowhttp 一致，禁用 HTTP/2） |

2. **system 提示詞簽名**：上游會掃描 system 提示詞開頭，命中 `You are Claude Code` 或 `You are Codex`（大小寫不敏感、單空格）即返回 429 `memfit_client`（`reason: client_blacklisted`）。網關在轉發前把這兩個簽名改寫成 `You are an AI assistant`，其餘字節保持不變——這是 Claude Code / Codex 能使用免費模型的前提。

## 使用

```bash
go build -o yakaigateway.exe .
./yakaigateway.exe                       # 監聽 127.0.0.1:8787
./yakaigateway.exe -listen 0.0.0.0:8787 -verbose
```

交叉編譯三平台（輸出到 `dist/`）：

```powershell
./build.ps1              # windows/amd64、linux/amd64、darwin/arm64
./build.ps1 -SkipTests   # 跳過 vet/test
```

客戶端指向 `http://127.0.0.1:8787`，API key 任意（會被丟棄替換）。

### Claude Code

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:8787
export ANTHROPIC_AUTH_TOKEN=free-user          # 任意值
export ANTHROPIC_MODEL=memfit-standard-free    # 或 memfit-deepseek-v4.1-flash-free 等
claude
```

### Codex CLI

在 `~/.codex/config.toml` 中把 provider 指向網關（Responses API）：

```toml
model = "memfit-standard-free"
model_provider = "yakaigateway"

[model_providers.yakaigateway]
name = "YakAIGateway"
base_url = "http://127.0.0.1:8787/v1"
env_key = "YAKAIGATEWAY_API_KEY"
wire_api = "responses"
```

## 路由

| 路由 | 說明 |
|---|---|
| `GET /v1/models` | 模型列表 |
| `POST /v1/chat/completions` | 對話（支持 SSE 流式） |
| `POST /v1/responses` | OpenAI Responses API（Codex 用） |
| `POST /v1/messages` | Anthropic Messages API（Claude Code 用） |
| `POST /v1/messages/count_tokens` | Claude Code 的 token 計數 |
| `POST /v1/embeddings` | 向量 |

所有 POST 路由：body 的 `model` 爲 `memfit-*` 時自動注入 `X-Memfit-OTP-Auth`（實測上游對 embeddings 同樣有 TOTP 門檻），TOTP 失敗自動刷新種子並重試一次。

## Flags

| Flag | 默認 | 說明 |
|---|---|---|
| `-listen` | `127.0.0.1:8787` | 監聽地址 |
| `-upstream` | `https://aibalance.yaklang.com` | 上遊 base URL |
| `-api-key` | `free-user` | 對所有上游請求強制使用的 Bearer token（客戶端值被丟棄） |
| `-user-agent` | 空 | 對所有上游請求強制使用的 User-Agent（空則不發送，與 yaklang 對話請求一致） |
| `-user-id` | 自動 | 對所有上游請求強制使用的 `X-User-Id`（空則在用戶配置目錄生成並持久化 UUID v4） |
| `-yak-version` | `1.4.9-beta6` | X-Yak-Version 強制值（舊版本會被上游 429 `memfit_client_version` 拒絕） |
| `-yak-build-time` | `Fri Nov  6 18:35:24 2026 +0800` | X-Yak-Build-Time 強制值（對應 1.4.9-beta6 發布構建） |
| `-usage-type` | `lightweight` | X-Yak-AI-Model-Usage-Type 強制值（空則省略；注意 lightweight 會使 memfit-standard-free 降級到 memfit-light-free） |
| `-max-body` | `33554432` | 請求體上限（字節），超限 413 |
| `-verbose` | 關 | 調試日誌（含 TOTP 種子） |

所有 `X-Yak-*`、`Authorization`、`User-Agent`、`Accept`、`Accept-Encoding`、`X-User-Id` 與 `X-Memfit-OTP-Auth` 均由網關完全接管，客戶端值一律丟棄。其餘客戶端頭（Cookie、X-Forwarded-*、anthropic-* 等）不轉發。

`X-User-Id` 是 yaklang 的持久化客戶端標識：上游免費額度按此標識分桶，缺失時會落入匿名桶，容易觸發 429 `memfit_client`。網關默認在 `%AppData%\yakaigateway\client-id`（Windows）持久化生成，亦可用 `-user-id` 指定。

## 測試

```bash
go vet ./... && go test ./...
```

- `totp_test.go`：RFC 6238 官方測試向量 + 與 yaklang `twofa` 實現的等價性
- `proxy_test.go`：上游頭指紋、提示詞簽名脫敏、TOTP 重試、SSE 逐事件 flush、Agent 路由、gzip 解壓、413、hop-by-hop 剝離
