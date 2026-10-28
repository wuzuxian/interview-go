# 03: DashScope TTS 实时协议客户端

**What to build:** 独立的语音合成（TTS）协议客户端（ADR 0003）：每次合成新建 WebSocket 连接（commit 模式）、提交文本、收集增量音频帧为完整 PCM（24kHz/16bit/mono）、建连超时 5s、合成超时默认 8s（可配），失败/超时向调用方返回错误（跳句策略由管道票决定）。附 WAV 封装辅助（44 字节头、24000Hz）。配套进程内假 TTS 服务端夹具。客户端不设接口接缝——URL 来自语音配置，测试打假端点。

**Blocked by:** None（可立即开始）

**Status:** ready-for-agent

- [ ] 单次合成闭环：连服务端 → 提交文本（含 model/voice/format/sampleRate/mode/languageType/speechRate/volume 运行时参数）→ 收齐增量音频 → 返回完整 PCM；每次合成独立新建连接，前一次失败不影响下一次
- [ ] 建连超时 5s：服务端挂起握手时按超时失败（对齐 Java）
- [ ] 合成超时默认 8s：增量中途停滞按超时失败返回错误
- [ ] API Key 由语音配置源解密提供，仅用于握手鉴权，不出现在日志与错误信息中
- [ ] WAV 封装辅助：PCM → WAV 字节级单测（头 44 字节、24000Hz/16bit/mono、长度字段正确）
- [ ] 假 TTS 服务端夹具：可脚本化正常合成、部分后停滞（触发超时）、拒连
- [ ] 契约测试驱动真客户端 × 假服务端，全程无外网
