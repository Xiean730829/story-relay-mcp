# 故事接龙：灵矽语音 + 设备端 / 私有 MCP

一个本地开发演示：浏览器模拟设备，Go 程序维护故事并执行 MCP，灵矽负责 ASR / LLM / TTS。支持设备端 MCP 与账号私有 MCP Proxy 两种模式，无需单独购买服务器。

## 私有 MCP Proxy 模式

启动后会将故事工具注册到你自己的灵矽账号，设备端不再上报同名工具：

```text
浏览器 / 硬件 ↔ 灵矽智能体 → 账号内私有 MCP → 灵矽 MCP Proxy ↔ 本机故事工具
```

浏览器演示的音频仍由本地网关桥接；支持灵矽的硬件理论上可以连接绑定了私有 MCP 的智能体，具体需根据硬件完成接入并实测。代码和游戏状态在本机执行，私有 MCP 的服务记录、开通实例及智能体绑定存在灵矽账号中。关闭电脑或程序后工具会离线。

在本目录启动：

```powershell
.\start-private.ps1
```

脚本会询问专用 Agent ID 与 API Key，Key 隐藏输入，只传入当前进程环境，不保存文件或修改用户永久环境变量。关闭窗口或 Ctrl+C 停止。启动前先关闭已运行的本地程序，避免同账号同时运行多个 MCP Proxy 实例。

先在灵矽控制台创建名为“故事接龙·私有演示”的专用智能体，并复制其 Agent ID；启动流程会同步该专用角色的提示词，避免覆盖其他智能体。

等价环境配置为 `LINX_API_KEY`、`LINX_AGENT_ID`、`STORY_MCP_MODE=proxy` 后执行 `go run .`。程序会：

1. 查询 Key 对应的内部账号用户名。七牛 SSO 账号的内部用户名可能是数字 UID，不能当作登录手机号；页面明确标注为“平台账号标识”。
2. 获取当前账号的 MCP Proxy 凭据，主动建立 WSS 长连接；仅在 HTTPS 管理接口返回同源 WS 地址时升级为 WSS，始终使用 TLS。
3. 返回三个 MCP 工具，创建或复用“故事接龙·私有MCP”，明确设置 `is_private=true`。其发布状态表示私有记录可开通，不表示公开到公共市场。
4. 创建或更新自己的开通实例，把代理凭据配置到 `X-MCP-Token` Header；凭据不出现在本地状态接口、页面或日志。
5. 保留目标智能体已有的 MCP 绑定，追加本服务，同步专用故事提示词。

控制台对应入口：“自定义 MCP”查看服务定义；“我的 MCP”查看开通实例；“我的智能体”进入专用角色查看绑定。MCP Proxy 凭据通常30天有效，页面显示实际到期时间；到期后用 Key 重启程序，自动获取新凭据并更新现有实例，不重复创建服务。

Proxy 当前不转发设备/会话身份，因此工具在 start 返回随机 `story_id`，continue/end 必须携带它。浏览器另通过提示词替换传入随机 `session_key`，仅用于将该连接的工具执行结果推回页面；没有此字段的硬件调用仍可依赖返回的 story_id 独立游戏，不使用账号级共享故事。

浏览器断连会清理其故事；其他故事在下次工具调用时清理30分钟未活动的记录，进程重启不恢复。浏览器的沉默结束继续由其播放完成事件计时；硬件直接连接灵矽时，工具本身无法感知最后一帧的播放时间，完全相同的沉默策略还需硬件事件或平台空闲策略配合，当前未把硬件链路当作已验收。

## 运行

需要 Go 1.26.1 或更高版本、Chrome / Edge、灵矽 API Key。首次连接时会在你的灵矽账号创建专用的“故事接龙·本地演示”智能体，并为浏览器生成的虚拟 MAC 预注册、激活。只用于本演示，不填写已有硬件的 MAC。

```powershell
cd story-relay-mcp
go run .
```

打开 http://127.0.0.1:8787，在密码框填写完整 API Key，选择“新建专用故事智能体”，点击“连接并开始”。页面拿到新 Agent ID 后会记住非敏感的 Agent ID 与虚拟 MAC，之后复用。API Key 不写文件、不记日志、不存 localStorage，断连后重新填写；可改用 `LINX_API_KEY` 环境变量。

API ID 是密钥记录的标识，不是 Agent ID，本演示不需要它。首次创建也不要求已有 Agent ID。使用现有智能体时，取消新建选项，填写 Agent ID，并先在控制台把本目录 `prompt.txt` 配置为系统提示词，意图设为 `Intent_function_call`；程序不会修改已有智能体提示词。

连接成功后点击“开启麦克风”，允许浏览器使用麦克风。也可以用文字输入验证相同的灵矽 → MCP → 游戏状态链路。

## 玩一轮

1. 选“小星先开头”并连接：AI 调用 `self.story.start`，页面出现第一句，并播放语音。
2. 开启麦克风，说“它在门后发现了一只戴帽子的兔子”：AI 调用 `self.story.continue`，记录孩子与 AI 各一句。
3. 说“换个故事”或点击按钮：AI 再次调用 start，新故事替换当前故事。
4. 说“结束故事”或让情节自然收尾：AI 调用 `self.story.end`。
5. AI 的音频实际播放完毕后开始等待，默认 60 秒没有孩子继续发言就结束。不催促孩子。调试可选择 15 秒。

选“我先开头”时，欢迎语后等待孩子第一句，再通过 start 保存孩子开头与 AI 接句。右侧显示真实的 `initialize`、`tools/list`、`tools/call`；点击按钮或打字也交给灵矽决定工具调用，不在页面伪造 MCP 成功。

## 边界与配置

- 游戏状态仅存在当前本地会话内存，断连不恢复；页面会保留已显示的故事供查看。没有音频文件保存或数据库。语音会发送到灵矽，平台侧记录策略需在灵矽控制台配置。
- 沉默结束是本地计时逻辑，通过 `listen.detect.text_response` 播放结束语，不伪装成 AI 发起的 MCP 调用。
- 音频使用 16kHz、单声道 PCM16，20ms 一帧；遵循服务端 hello 的下行音频参数。服务端需支持 PCM，避免本地安装 Opus/CGO。
- 当前只监听回环地址，面向开发验证。公网部署需要增加应用认证、会话持久化和 HTTPS；硬件接入需要实现相同的浏览器到本地网关消息协议，或另做硬件协议适配。
- 关闭程序不会删除灵矽账号下已创建的智能体或设备，可以在灵矽控制台管理。

| 环境变量 | 默认值 / 作用 |
| --- | --- |
| `STORY_ADDR` | `127.0.0.1:8787`，仅支持回环地址 |
| `LINX_API_KEY` | 无默认值；API 认证，只在服务端内存使用 |
| `LINX_AGENT_ID` | 可选，使用已配置好的智能体 |
| `LINX_API_BASE` | `https://xrobo.qiniu.com` |
| `LINX_OTA_URL` | API base + `/xiaozhi/ota/` |
| `LINX_WS_URL` | `wss://xrobo-io.qiniuapi.com/v1/ws/`；OTA 返回地址时优先使用 |
| `STORY_MCP_MODE` | 默认 `device`，设为 `proxy` 启动账号私有 MCP |

本地 HTTP API 仅 `GET /api/status`，响应为 `{code,msg,reqid,data}`，不包含密钥。Proxy 模式额外返回账号内部用户名、Agent / MCP 标识、代理在线状态、到期时间与调用计数。`/ws` 是浏览器网关，不是公网 HTTP MCP 端点；设备端模式通过语音连接提供工具，Proxy 模式通过独立 WSS 连接向平台的 `/v1/mcpproxy/mcp` 端点提供工具。

故障定位：API 401 检查 Key；未绑定提示检查设备激活；MCP 没有列出检查意图模式；只有文字没有声音检查浏览器音频权限；有 AI 回答但没有工具调用检查模型的工具能力及系统提示词。语音尚未连接时不会用假故事替代真实调用。

## 浏览器网关协议

浏览器连接本地 `/ws`，Origin 必须与页面一致。第一条文本消息为 `{"type":"connect","options":{...}}`。`options` 包含 `api_key`（可用服务端环境变量替代）、`agent_id`、`device_id`（MAC）、`create_agent`（bool）、`first_speaker`（`ai` / `child`）、`silence_seconds`（15–300，默认60），可选 `opening` 为孩子的首句。新建模式忽略已有 Agent ID，不修改其他智能体。

后续文本消息格式为 `{type,text?}`：

| type | 含义 |
| --- | --- |
| `text` | `text` 是用户输入的一句，最多400字，通过灵矽触发工具 |
| `replace` | 请求 AI 新开一个故事 |
| `end` | 请求 AI 调用结束工具 |
| `interrupt` | 中止当前 AI 播放 |
| `voice.activity` | 麦克风检测到声音，延后等待计时 |
| `playback.done` | 下行音频真正播放完毕，开始等待孩子 |

上行二进制帧为16kHz、单声道、little-endian PCM16，每帧320采样（20ms）。下行二进制遵循 `platform.data.audio_params`。同一连接的文本与二进制按发送顺序处理。

服务端文本格式为 `{type,text?,data?}`：`phase` 是连接阶段及非敏感设备身份；`ready` 表示工具列表已回复灵矽；`error` 是可展示的错误提示；`platform` 是脱敏后的 hello / STT / TTS；`trace` 记录请求方法、工具名、请求ID与成功状态；`story` 是 `{id,title,status,reason?,lines:[{role,text}]}`。`status` 为 `ready` / `playing` / `ended`，结束原因是 `child_stop` / `ai_finish` / `silence`。断连后不重放或恢复旧状态。

灵矽侧复用官方 WebSocket 的 `hello` / `listen` / `mcp` 格式，MCP 通过 `payload` 承载 JSON-RPC 2.0。`initialize` 返回版本 `2024-11-05`，`tools/list` 一次返回三个工具，无后续分页。`tools/call` 结果为 `content:[{type:"text",text:...}]` 和 `isError`；业务校验失败不更新故事，未知 RPC 方法返回 `-32601`，错误参数返回 `-32602`。

灵矽管理接口的设备预注册响应是 `{success_count,failed}` 裸对象，与其他管理接口的 `{code,msg,data}` 不同，在接入边界明确处理。

## 检查

在当前目录执行 `go build ./...`、`go vet ./...`、`go test ./...`、`staticcheck ./...`；缓存和二进制请放系统临时目录。尚未新增自动化测试用例。实际验收以真实 Key 连接后的语音、工具调用、换故事与结束路径为准。

已通过浏览器进行语音与私有 MCP Proxy 联调；硬件接入及硬件端沉默结束策略尚未实测。

参考：[Palpable MCP 代理](https://github.com/LiusCraft/palpable/blob/main/server/internal/bridge/mcp/proxy.go)、[灵矽 WebSocket](https://linx.qiniu.com/docs/xrobot/platform/websocket)、[设备端 MCP](https://linx.qiniu.com/docs/xrobot/mcp/hardware-mcp)、[智能体 API](https://linx.qiniu.com/docs/xrobot/api/agent)。
