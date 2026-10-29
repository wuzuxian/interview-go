# 08: 评估链路（语音报告）

**What to build:** 面试结束后异步生成语音评估报告：消费 voice:evaluate:stream（幂等跳过非 PENDING、置 PROCESSING）；把 DIALOGUE 消息流做问答配对（连续提问归入待答状态机、回答回填最近未答问题、分类关键词推断），SUMMARY 行不参与；复用文字面试评估引擎（批次、结构化输出、降级共用其配置与接缝），按方向注入评估参考资料（复用既有截断规则）；报告落 voice_interview_evaluations（JSON 列）并置 COMPLETED；失败重投至多 3 次后置 FAILED + evaluate_error（截断 500）。REST 评估接口：GET 三分支（COMPLETED 内嵌报告 / PROCESSING 原样 / 其余返回状态）+ POST 重试（COMPLETED 返回缓存、PROCESSING 原样、否则置 PENDING 入队）。空对话场次生成占位空评估。恢复巡检：PENDING 超 3min 补投、PROCESSING 超 30min 置 FAILED。

**Blocked by:** 01（会话/消息仓储与投递）

**Status:** resolved

- [ ] 问答配对：pendingQuestion 状态机（连续提问累积、回答回填最近未答）、分类推断关键词表对齐 Java inferCategory；只处理 DIALOGUE 行
- [ ] 评估明细 DTO 与文字面试明细逐字对齐：sessionId/totalQuestions/overallScore/overallFeedback/strengths/improvements/answers[]，AnswerDetail 七字段（questionIndex/question/category/userAnswer/score/feedback/referenceAnswer/keyPoints）
- [ ] 复用文字面试评估引擎并注入按方向的评估参考资料（截断规则一致）；评估状态 DTO（evaluateStatus/evaluateError/evaluateStatusUpdatedAt/evaluation）对齐
- [ ] 消费者顺序契约：跳过非 PENDING → 置 PROCESSING → 评估 → 置 COMPLETED + 落库；失败重投 retryCount+1，上限 3 次后 FAILED + evaluate_error（截断 500）
- [ ] GET evaluation 三分支与 POST evaluation 重试三分支语义对齐 Java
- [ ] 空对话场次生成占位空评估（totalQuestions=0 的合法报告，非失败）
- [ ] 恢复巡检基于 DB 状态时间戳（PENDING>3min 补投、PROCESSING>30min 置 FAILED），时间可注入便于测试
- [ ] 测试（内存假件，先例 evaluation_fakes）：先 PENDING 再入队顺序、重试上限、空评估、恢复补投与置败、QA 配对与分类推断表驱动用例
