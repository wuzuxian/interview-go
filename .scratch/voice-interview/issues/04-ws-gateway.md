# 04: WebSocket 网关

**What to build:** 求职者可以连接 `/ws/voice-interview/{sessionId}`（gorilla/websocket，gin 路由，来源校验沿 CORS 白名单）：建连后收到 welcome 与 asr_ready；上行音频帧（base64 PCM）转发 ASR，识别部分结果实时下发 subtitle(isFinal=false)，最终结果按多段智能拼接进话语合并缓冲并下发 subtitle(isFinal=true)；ASR 连接失效自动重启并重发当前块（15 次 × 80ms）；回声冷却（800ms）与处理中丢弃迟到结果与麦克风输入；control 消息分发（submit 可携带修正文本覆盖合并缓冲、CAS 抢处理锁防双答；end_interview 结束会话并触发评估投递；start_phase 预留）；断开连接时停 ASR 并自动结束进行中的会话。LLM 回复通过回合挂钩接入——本票内置空实现（仅日志），06 替换为真实管道。下行六类消息 JSON 形状逐字对齐。管道测试用真网关（httptest 真 WS 服务端）× 假 ASR 服务端 × 内存存储。

**Blocked by:** 01（会话服务与消息仓储）, 02（ASR 客户端）

**Status:** resolved

- [ ] 建连校验会话存在与状态；welcome / asr_ready 控制消息形状逐字对齐；ASR 建连慢时推 asr_reconnecting，10s×2 次重试仍失败推 error
- [ ] 上行 `{"type":"audio","data":...}` 解码转发 ASR；ASR 未就绪期间音频帧静默丢弃
- [ ] 部分→subtitle(isFinal=false)、最终→拼入话语合并缓冲并 subtitle(isFinal=true)；多段拼接（joinSegments）语义对齐 Java；最终结果不自动触发模型
- [ ] ASR 连接失效：自动重启（02 的重启语义）+ 当前块重发至多 15 次×80ms，仍失败下发 error 且会话不断
- [ ] 回声冷却：回合挂钩置"AI 说话"后 800ms 窗口内丢弃上行音频；处理中丢弃迟到识别结果
- [ ] control submit：data.text 可覆盖合并缓冲；CAS 抢处理锁，失败短暂等待后重试，杜绝同一话语双答
- [ ] control end_interview → 复用结束语义（COMPLETED + 评估 PENDING + 投递）；异常断开同样自动结束进行中的会话并停 ASR
- [ ] 下行编码六类形状逐字：control / subtitle(text,isFinal) / text(content,final) / audio(data,text) / audio_chunk(data,index,isLast) / error(message)
- [ ] 发送保护：写超时 10s、积压上限、会话关闭后发送安全丢弃；单条上行消息上限 256KB
- [ ] 每会话状态互斥保护，会话间隔离；优雅关闭钩子预留（09 接管）
- [ ] 管道测试（真网关 + 假 ASR 服务端 + 内存存储）覆盖：字幕两级下发、多段拼接、重连重发、回声冷却丢帧、submit 覆盖与双答拒绝、断开自动结束

## Comments

- 2026-10-29：验收项「建连校验会话存在与状态」中的**状态校验已撤销**（bug2 修复）。Java 原版握手无任何校验；状态校验导致 StrictMode 重连被 409 拒绝。现仅保留 404 存在性防御，登记为 spec 偏离 #11。
