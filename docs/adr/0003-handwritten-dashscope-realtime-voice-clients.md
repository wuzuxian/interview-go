# 语音面试 ASR/TTS 手写 DashScope 实时 WebSocket 客户端

语音面试需要实时双向音频：ASR 用 DashScope `qwen3-asr-flash-realtime`、TTS 用 `qwen3-tts-flash-realtime`，两者都是阿里专有的 WebSocket 事件协议。Go 侧没有 DashScope 官方 SDK（Java 版依赖 dashscope-sdk-java 的 OmniRealtime/QwenTtsRealtime 客户端），eino 也没有任何 ASR/TTS 组件（其流抽象只在进程内，无 WebSocket 出口）。决定：在语音面试模块内手写这两个协议客户端——连 wss、收发 JSON 事件、base64 音频帧——事件语义与 Java 版逐一对齐；不更换语音厂商、不引入社区非官方封装。

## Considered Options

- 更换有 Go SDK 的语音厂商 —— 拒绝：`voice_config` 表与设置页 `/api/llm-provider/voice/*` 契约已按 DashScope 字段复刻完成，换厂商等于推翻已复刻的配置契约。
- 用 DashScope OpenAI 兼容 REST 做非实时识别/合成 —— 拒绝：交互模型不同（非双向流式），实时字幕与逐句音频无法支持。
- 社区非官方 dashscope Go 库 —— 拒绝：实时协议覆盖不全、维护状况不明；协议本身只是"JSON 事件 + base64 帧"，自写可控，且协议是我们写的、可用进程内假服务端做契约测试。

## Consequences

- DashScope 协议演进（事件名/字段变化）需自行跟进，没有 SDK 升级通道。
- ASR/TTS 运行时配置沿用已复刻的 `voice_config` 表与热更新接口，客户端在会话建立时读取当前配置。
- 假 DashScope 服务端与真客户端同仓维护，作为协议客户端的契约测试夹具。
