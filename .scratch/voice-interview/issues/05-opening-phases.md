# 05: 开场白与阶段机

**What to build:** 求职者建连后自动听到面试官开场白：按方向从嵌入的开场白资产取文案（9 个方向 + 默认），先落库一条面试官消息、推 text(final=true)，再合成音频下发（开场音频按方向内存缓存，预热默认关）。阶段机：INTRO→TECH→PROJECT→HR→COMPLETED 按各阶段时长（min/suggested/max）与题量（min/max）判据推进，创建时的四阶段开关影响推进路径；control start_phase 可主动切换并落库。阶段判据以纯函数暴露，供 06 的回合管道在生成下一问时消费。

**Blocked by:** 03（TTS 客户端）, 04（网关）

**Status:** ready-for-agent

- [ ] voice-interview-opening.yml 资产拷贝嵌入（9 方向 + 算法/后端默认）；无匹配方向时走默认文案
- [ ] 无历史消息的会话建连后自动收到开场：text(final=true) 先于音频；开场消息先落库（DIALOGUE、对应阶段、序号）再推送
- [ ] 开场音频按方向缓存内存，同方向重复建连不重复合成；预热开关默认关
- [ ] 阶段判据纯函数：输入阶段配置 + 已问轮数 + 阶段内耗时，输出 保持 / 推进下一阶段 / 收尾进 COMPLETED；转移条件对齐 Java 判据
- [ ] start_phase 仅接受 INTRO/TECH/PROJECT/HR，落库并更新缓存；非法值忽略不报错（对齐 Java）
- [ ] 四阶段开关（intro/tech/project/hrEnabled）跳过被关闭的阶段
- [ ] 阶段配置四参默认值对齐 Java application.yml（phase.* 段）
- [ ] 单测（判据纯函数、资产解析）+ 管道测试（假 TTS 服务端：开场文本与音频顺序、缓存命中、start_phase 切换）
