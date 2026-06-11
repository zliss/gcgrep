# 我给 AI 编程助手写了一个"带索引的 grep"：30 万行代码毫秒级搜索，Win11 提速 50 倍

> 开源地址：https://github.com/zliss/gcgrep （MIT，单二进制，免安装免配置）

## 起因：AI 助手把我的 CPU 烧满了

用过 Claude Code、Cursor 这类 AI 编程助手的人应该有体会：AI 干一个活要 grep 几十次——找定义、找调用、改完再确认。在 macOS 上还好，在 **Windows 11 上是灾难**：NTFS 文件打开开销大，Defender 实时扫描每一个被读取的文件，一次全仓库搜索动辄几秒，AI 一轮任务下来风扇起飞。

问题的本质是：**grep 每次都全量遍历文件系统，而代码 99% 没变**。IDEA 们早就用索引解决了这个问题，但 AI 助手用不了 IDEA 的索引——它们只会调命令行。

所以我做了 gcgrep：把 IDE 的索引思路装进一个 grep 兼容的命令行工具。

## 效果（kubernetes 仓库，30482 个文件，实测）

| 操作 | macOS | Windows 11 |
|---|---|---|
| `grep -rn` / `findstr /s` | 260ms | 1.8s |
| PowerShell `Select-String` | — | 9.4s |
| **gcgrep 热查询** | **5ms** | **37ms** |
| 首次建索引（一次性） | 8s | 55s |

不止文本搜索，还有 IDE 风格的符号搜索（Go/Java/Python/TypeScript）：

```text
$ gcgrep def NewSchedulerCommand ./kubernetes
cmd/kube-scheduler/app/server.go:93: [func NewSchedulerCommand] func NewSchedulerCommand(...)

$ gcgrep refs NewSchedulerCommand ./kubernetes      # 6ms 找到全部调用点
cmd/kube-scheduler/scheduler.go:30: [call] command := app.NewSchedulerCommand()

$ gcgrep symbols UserService.java                   # 文件大纲
```

## 原理：daemon + trigram 索引 + 文件监听

```
CLI ──unix socket / named pipe──> 常驻 daemon
                                    ├─ trigram + 符号索引（内存）
                                    ├─ fsnotify 监听增量更新
                                    └─ 持久化，重启秒级对账
```

- **首次搜索某目录时自动建索引**（trigram 倒排，ripgrep/zoekt 同款原理），之后查询不再碰文件系统——这正是绕开 Win11 Defender 税的关键。
- **文件监听增量更新**：改一个文件只重建一个文件的索引，debounce 合并事件风暴（git checkout 触发 overflow 时自动降级全量对账）。
- **不占端口**：unix socket / Windows named pipe 通信，daemon 按需自动拉起。
- **重启不丢**：索引落盘，重启后只做 stat 对账，离线期间的增删改全部补上。

### 一个容易被忽略的杀手细节：写后读一致性

AI 助手的工作循环是"改文件 → 立刻搜索验证"。任何带缓存的搜索工具在这里都有一个静默出错的窗口：事件还没送达，搜到的是旧内容，而且**不报错**。gcgrep 用了 Facebook watchman 的 cookie 文件方案：每次查询前往目标目录丢一个 cookie 文件，等它的事件从 OS 队列穿出来（证明之前的所有写入事件都已处理）再应答。实测开销约 1ms，换来的是"搜到什么就是什么"的确定性。这是我认为它和"自己拿 redis 缓存个 grep 结果"的本质区别。

### 符号搜索的诚实边界

- Go 用标准库 AST 解析，**精确**；Java/Python/TS 用注释/字符串剥离 + 词法启发式，**ctags 同级精度**（zoekt/Sourcegraph 的符号层就是这个级别）
- `refs` 是**语法级候选集**：分不清重载、分不清同名方法属于哪个类——那需要类型推断，是 IDE/LSP 的领域。但对 AI 来说候选集恰好够用：它拿到 20 个候选会自己读上下文过滤

## 给 AI 助手用：往 CLAUDE.md 里贴一段就行

```markdown
- 代码搜索优先用 gcgrep（未安装则回退 grep）。输出格式和 exit code 与 grep 一致。
  - 文本: gcgrep PATTERN [DIR]   找定义: gcgrep def NAME [DIR]
  - 找调用: gcgrep refs NAME [DIR]   文件大纲: gcgrep symbols FILE
  改完文件立刻搜索是安全的（写后读一致）。
```

## 工程上的几个坑（做类似工具的可以参考）

1. **Go 正则的 `(?i)` 慢得离谱**：大小写不敏感字面量搜索 1.1s，换成手写 ASCII case-fold + `bytes.Index` 后 66ms，17 倍。
2. **macOS 上 `cp` 覆盖正在运行的二进制会让新进程直接被 SIGKILL**（代码签名缓存），要先 `rm` 再 `cp`。
3. **fsnotify 的事件缓冲区会溢出**（Windows 的 ReadDirectoryChangesW 尤其容易），必须有 overflow → 全量对账的降级路径，否则索引会静默腐烂。
4. **tree-sitter 虽好但 cgo 会毁掉交叉编译**。ctags 路线（纯 Go 词法启发式）精度够用，换来单二进制 + 一台 Linux 机器编译三平台。

## 安装

[Releases](https://github.com/zliss/gcgrep/releases) 下载对应平台的单二进制放进 PATH 即可，无服务注册无配置。也可 `go build ./cmd/gcgrep`。

测试覆盖：索引正确性、gitignore、实时监听、持久化对账、无 sleep 的写后立查压测、四语言符号提取套件，macOS/Win11/Linux 三平台真机验证。

欢迎 Star / Issue / PR——加一个新语言 = 一个提取器文件 + 测试。
