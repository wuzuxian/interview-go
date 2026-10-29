# Knowledge-base vectors share the Java vector_store table and metadata protocol

Go 与 Java 共用同一 PostgreSQL，RAG 检索按向量 metadata 的 `kb_id` 过滤，知识库向量因此必须写入同一张 `vector_store` 表，并完全兼容 Java 的 metadata 协议：`kb_id`、`source_kb_id`、`source_filename`、`chunk_strategy`，以及 `pending:<kbId>:<jobId>` 临时 id + promote 的两阶段写入（检索永不命中半成品）。向量化任务沿用与 Java 相同的 Redis Stream key 与 consumer group（简历模块 `resume:analyze:stream` 的既有先例），两实例共存时互为备份。简历向量既有的 `source='resume'` metadata 保持不变；`internal/vector` 的简历专用写入/检索方法泛化为按 metadata 键操作，以承载两类来源。

## Considered Options

- 知识库另立向量表：破坏共库契约，Java 实例检索不到 Go 写入的向量。
- 复用同表但自创 metadata 键名：与另立表无异，只是更难排查。
