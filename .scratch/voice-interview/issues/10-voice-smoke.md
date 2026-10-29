# 10: 语音冒烟命令

**What to build:** 开发者可以无麦克风端到端验收语音链路：cmd/smoke 增加 voice 模式（-mode，默认 text 保持原文字冒烟完全不变）——真实创建场次 → WS 连接 → 收开场白 → 用真实 TTS 合成一段"回答"音频 → 线性重采样到 16kHz/200ms 帧 → 逐帧喂 ASR → 断言最终字幕非空 → control submit（必要时带修正文本）→ 断言流式 text 与 audio_chunk/audio_complete → end_interview → 轮询评估至 COMPLETED → 校验报告结构 → DELETE 清理（-keep 跳过）。前置检查：voice_config 未配置真实 ASR/TTS Key 时给出清晰错误。

**Blocked by:** 01–09（全部）

**Status:** resolved

- [ ] -mode 默认 text，既有文字冒烟行为与 flags 完全不变
- [ ] voice 模式步骤化输出（对齐既有 smoke 风格），任一步失败即停并给出非零退出码
- [ ] TTS→重采样→ASR 回环：重采样为线性插值纯函数（带单测），帧长 200ms、16kHz/16bit/mono、base64 装帧
- [ ] 断言项：welcome、开场音频非空、subtitle(isFinal=true) 非空、text(final=true) 非空、至少一个 audio_chunk、audio_complete、评估 COMPLETED、报告必备字段齐全
- [ ] 超时与轮询参数沿既有 flags 约定（-base-url/-timeout/-eval-timeout/-keep 等复用，新增 -skill/-difficulty 透传）
- [ ] 真实 DashScope Key 前置检查与友好报错；帮助文本更新
