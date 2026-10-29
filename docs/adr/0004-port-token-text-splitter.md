# Port Spring AI TokenTextSplitter for knowledge-base chunking

Java 版知识库切分用 Spring AI 2.0 `TokenTextSplitter`（chunkSize=800 token、中文句末标点集），并把策略版本 `token-v1` 写进向量 metadata 与 `vector_config` 快照；eino 生态的 splitter（recursive / semantic / markdown）算法均与之不同。为保证同一文档在 Java / Go 两实例下产出一致的切片并诚实沿用 `token-v1` 版本号，Go 侧忠实移植 `TokenTextSplitter` 算法（tiktoken cl100k_base 计数），对外包装为 eino Transformer 接口，不引入新的分块语义。

## Considered Options

- eino recursive / semantic splitter 调参近似：分块语义不同，须改版本号（如 `go-recursive-v1`），两实例对同一文档切片不一致，检索质量存疑。
- 按 rune 数简化切分：实现最省，但与 token 语义偏差最大，中文场景尤甚。

## Consequences

- 切分器须有表驱动单测（黄金用例）锁定行为：同输入同输出，防止移植走样。
- 引入 tiktoken 依赖，词表加载策略见知识库 spec（ADR 0005 与 spec 不覆盖的部分）。
