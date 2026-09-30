# 真实 pi 请求性能与 Responses 重连调查

## 结论与证据边界

原请求已通过 NewAPI request ID、endpoint、完成时间、completion/cache token
和本机 pi usage 交叉关联。它有 343,776 prompt tokens，343,296 cached tokens，
命中率 **99.8604%**，输出 340 tokens，其中 reasoning 133。

主要等待发生在 **Proxy 发出请求至 Cline 返回响应头：6,213 ms**。
并不是响应头之后又等待了 5–8 秒：响应头至首 SSE 只有 348 ms。
旧记录不能进一步区分上传、连接和 Cline 内部等待。新生产 trace 表明，
相近大上下文请求使用复用的 HTTP/2 连接，上传后仍可等待 5 秒以上。
因此目前可定位到 Cline 返回响应头之前，**不能直接断言是某个 provider 的 prefill**。

原请求是 `thinking + toolCall`，没有 text。NewAPI 的 8.637 秒是收到首个
非空 data 的时间，不是 pi 显示首个文本的时间。
`340 / 1.256 = 270.7 t/s` 是首 SSE 至完成的 reasoning/tool 合计吞吐近似值，
不是可见文本 decode TPS。NewAPI 的 `340 / 10 = 34 t/s` 确实是端到端算法。

截图的重连错误是 `ResponseCompleted` 缺少 `reasoning_tokens`。
这是完成事件的协议解析错误；不能据此判断 TCP 断开。
v0.1.9 已修复零值/缺失值的序列化，并已由 Actions 构建和部署。

## 1. 原请求身份

| 字段 | 已核实值 |
|---|---|
| NewAPI channel / log ID | #53 / 312851 |
| NewAPI request ID | `202609300255413858373568268d9d6YTC9A2Xx` |
| Proxy request ID | `2be4c1a10fb88f8a` |
| endpoint / stream | `/v1/messages?beta=true` / true |
| 模型 / reasoning effort | `cline-pass/deepseek-v4.1-flash` / max |
| credential | www |
| NewAPI session fingerprint | `791584ff`（pi） |
| Proxy affinity | `prefix:7ce03def74cb5d9d`，single-credential |
| request / upstream bytes | 19,160,149 / 19,205,241 |
| prompt / cached / completion / reasoning | 343,776 / 343,296 / 340 / 133 |
| 状态 / failover | 200 / 0 |

本机 pi session 的 assistant `128dbbb5`（第 3232 行）有相同 usage：
input 480、cacheRead 343,296、output 340，stopReason 为 toolUse，
content 类型是 thinking、toolCall。session append 时间是完成后保存消息的时间，
message timestamp 是请求创建附近的时间；二者都不是首 token 显示时间。

## 2. 原请求逐阶段 timing

下表时间均为 +08:00。Proxy 内部绝对时间由 started_at 加 elapsed_ms 得出，
精度约 1 ms；客户端与服务器未建立历史时钟偏差校正。
“累计”以 Proxy handler 的 request_received 为零点。

| Stage | 绝对时间 | 前一已测阶段间隔 | Proxy 累计 |
|---|---|---:|---:|
| NewAPI received（request ID 编码） | 10:55:41.385837356 | — | — |
| Proxy request_received | 10:55:45.939 | 4,553.402 ms，自 NewAPI received | 0 ms |
| body_read_done | 10:55:45.985 | 46 ms | 46 ms |
| parse_done | 10:55:46.180 | 195 ms | 241 ms |
| credential_selected | 10:55:46.314 | 134 ms | 375 ms |
| translate_done | 10:55:46.479 | 165 ms | 540 ms |
| upstream_request_start | 10:55:46.524 | 45 ms | 585 ms |
| upstream_headers_received | 10:55:52.737 | 6,213 ms | 6,798 ms |
| first_upstream_event | 10:55:53.085 | 348 ms | 7,146 ms |
| first_protocol_event / first_downstream_write（旧写前标记） | 10:55:53.085 | 同一 ms | 7,146 ms |
| stream_complete / request_complete | 10:55:54.341 | 1,256 ms | 8,402 ms |
| NewAPI GIN handler 完成（由 received + GIN duration 推算） | 10:55:54.383 | 约 42 ms，自 Proxy complete | — |

原请求没有 first_reasoning_event、first_text_event、实际 flush 时间等新字段。
没有 text，所以 first_visible_text_token / pi first visible text 不适用；
pi 首个 thinking/tool 显示时刻也未记录。

4,553 ms 是 NewAPI 入口到 Proxy handler 的整个区间，可能包含输入读取、
解析、第一次渠道选择、后续 relay 准备和发送，**不是纯网络耗时**。
NewAPI 的 request-start 在 distributor 读取输入和初次选渠道之后才设置；
policy 又有独立 StartedAt。policy 中 channel_selected=1,341 ms、
request_completed=9,925 ms 不能直接拼接到上表。

`NewAPI frt 8,637 - Proxy first event 7,146 = 1,491 ms` 混合了不同起点，
不是已证明的响应缓冲时间。GIN 全程 12,997.417 ms 减 Proxy 8,402 ms，
得到约 4,595 ms 的 Proxy 区间之外时间；其中约 4,553 ms 在 Proxy 到达之前。
NewAPI→pi 传输和客户端绘制时间未知。

## 3. 吞吐与 cache

| 指标 | 原请求值 | 解释 |
|---|---:|---|
| NewAPI end-to-end TPS | 34 t/s | 340 / 整数 use_time 10 秒 |
| Proxy end-to-end TPS | 40.47 t/s | 340 / 8.402 秒 |
| 首 SSE 至完成合计 TPS | 270.70 t/s | 340 / 1.256 秒；包含 reasoning/tool，阶段吞吐近似值 |
| visible text / reasoning 独立 TPS | 未测 | 本轮无 text；没有旧 reasoning 首末时刻 |
| cache hit | 99.8604% | 343,296 / 343,776 |

这条请求不能支持“生成阶段只有 34 t/s”的结论。观察到的主要问题是等待输出开始。
首 SSE 至完成的速率包含协议尾部和 token 统计口径，不能当成精确模型内部 decode 速度。

同一 pi session 的连续 7 轮如下。credential 均为 www、affinity 均相同。

| Proxy 开始时间 | Prompt | Cached | Cache hit | 首上游事件 | 总耗时 |
|---|---:|---:|---:|---:|---:|
| 10:55:25.912 | 343,319 | 1,536 | 0.447% | 6.760 s | 12.240 s |
| 10:55:45.939（目标） | 343,776 | 343,296 | 99.860% | 7.146 s | 8.402 s |
| 10:56:03.032 | 344,122 | 343,680 | 99.872% | 5.778 s | 7.799 s |
| 10:56:19.008 | 344,836 | 344,064 | 99.776% | 6.620 s | 8.254 s |
| 10:56:35.330 | 345,170 | 344,832 | 99.902% | 6.793 s | 7.081 s |
| 10:56:49.930 | 345,430 | 345,088 | 99.901% | 6.629 s | 10.316 s |
| 10:57:08.496 | 345,773 | 345,344 | 99.876% | 6.581 s | 7.187 s |

第一轮是明显冷 cache，后面不是持续 cache miss。上述历史 provider 未正确提取，
无法从它们证明同一路由/provider。新样本中也发现个别大 pi 请求 cache 突降，
不能把原目标的高 cache 状态推广到每一轮。

## 4. 多 session 与上下文分桶

历史快照包括 1,490 条 NewAPI 同渠道同模型记录和 1,515 条 Proxy 同模型记录。
以 endpoint、cached/completion tokens 和完成时间关联到 1,482 条；这是关联统计，
不是新增测试请求。旧 ttft_ms 混合 reasoning/tool、且 tool-only 可为 0，
所以这里只比较 **首上游事件**，不把旧 ttft 当作可见文本时间。

| Session fingerprint | 客户端 | 成功关联数 | 首上游事件 median | request body median |
|---|---|---:|---:|---:|
| 791584ff | pi | 40 | 7.021 s | 20.221 MB |
| 11cea375 | pi | 15 | 0.845 s | 0.341 MB |
| da010cd4 | pi | 1 | 1.093 s | 0.118 MB |
| a337ce4b | Codex | 1,064 | 1.286 s | 0.309 MB |
| 3ed93177 | Codex | 239 | 1.864 s | 0.799 MB |
| 53dd01eb | Codex | 107 | 1.447 s | 0.633 MB |

下面限定同模型、www、状态 200、cache hit >90%。p90 使用 nearest-rank。

| Prompt tokens | n | 首上游事件 median | p90 | Cache median | 首 SSE 后合计 TPS median |
|---|---:|---:|---:|---:|---:|
| <20k | 1 | 0.607 s | 0.607 s | 96.482% | 307.69 |
| 20–50k | 163 | 0.882 s | 2.789 s | 97.126% | 219.72 |
| 50–100k | 544 | 1.221 s | 3.796 s | 98.228% | 252.27 |
| 100–200k | 324 | 1.465 s | 3.308 s | 99.598% | 279.53 |
| 200–300k | 85 | 2.006 s | 2.353 s | 99.784% | 391.76 |
| 300k+ | 98 | 2.479 s | 7.471 s | 99.899% | 213.98 |

300k+ 桶必须按协议/客户端拆开，不能把不同请求结构当成同一 A/B：

| 300k+ warm cache | n | 首事件 median | 本地准备 median | 发起→响应头 median | Body median | 合计 TPS median |
|---|---:|---:|---:|---:|---:|---:|
| pi /messages | 39 | 7.146 s | 672 ms | 5,911 ms | 20.315 MB | 203.09 |
| Codex /responses | 59 | 2.141 s | 74 ms | 1,987 ms | 1.335 MB | 218.01 |

同样 300k+ 并不等于相同 prompt/body/provider。pi 历史带图片等内容；
本机保存会话用已安装客户端 buildSessionContext 重建的投影包含 2,837 messages、
110 images，base64 图片数据约 28.70 MB。**这不是实际 HTTP body 捕获**，
不能据此断言每个字节都被发出，或直接断言图片导致全部延迟。

## 5. 新 httptrace：真实 warm-cache pi 示例

v0.1.9 新字段 timing_version=2，不再把协议 prologue 当作可见文本 TTFT。
下面是已经发生的请求，不是 synthetic prompt。

请求 `3d105a572ace6c54`：开始 11:45:54.881；prompt 311,892，cached 311,296，
cache 99.809%；upstream 6,312,247 bytes；completion 389，reasoning usage=1；无 text。

| Stage | 绝对时间（+08:00） | 上阶段间隔 | 累计 |
|---|---|---:|---:|
| request_received | 11:45:54.881 | — | 0 ms |
| body_read_done | 11:45:54.899 | 18 ms | 18 ms |
| parse_done / affinity_start | 11:45:54.958 | 59 ms | 77 ms |
| affinity_done / credential_selected / translate_start | 11:45:55.005 | 47 ms | 124 ms |
| translate_done | 11:45:55.106 | 101 ms | 225 ms |
| upstream_request_start | 11:45:55.134 | 28 ms | 253 ms |
| get_conn → got_conn | 11:45:55.135 | 约 0.000672 ms | 253.201651 ms |
| wrote_headers | 11:45:55.135 | 0.348 ms，自 got_conn | 253.549506 ms |
| wrote_request | 11:45:55.218 | 83.042 ms | 336.591121 ms |
| response_headers | 11:46:00.630 | **5,411.598 ms** | 5,748.189495 ms |
| first_upstream_event / first_tool_event | 11:46:00.753 | 约 124 ms | 5,872 ms |
| first_tool_write（flush 后）/ stream_complete | 11:46:01.444 | 691 ms | 6,563 ms |

连接为 reused HTTP/2；没有 DNS/TCP/TLS 新建事件，不能把缺失事件显示为重新握手。
该请求上传只需约 83 ms，主要等待在上传完成后、响应头前。
`wrote_request` 是 Go 客户端写完请求的边界，不是 Cline 服务端接收完成的证明。

该轮工具事件到首下游工具输出差 691 ms。当前 Claude converter 在 finish reason
时 finalizeBlocks 才发出完整工具调用；期间还在接收参数，最后上游 tool event
在 6,519 ms。它不是 691 ms 的纯 convert CPU/flush 耗时，也不是丢失 text。
本次没有修改此行为；需要真实 tool-stream fixture 与 parity 验证后才能改变。

新 trace 快照中，27 个大 pi 和 80 个 Codex 请求全部复用 HTTP/2。
大 pi 上传 median=164.27 ms、上传后等响应头 median=3,612.68 ms；
Codex 上传 median=6.52 ms、上传后等响应头 median=1,833.17 ms。
新 pi body 约 6.27 MB，**不能用它的上传时间回填原请求 19.2 MB 的缺失字段**。

另外，reasoning 与文本确实可以分离：已观察到 Codex reasoning flush≈9.030 s、
文本 flush≈16.993 s，总耗时≈21.787 s；另一 pi 小会话 reasoning≈1.950 s、
文本≈22.002 s。这些是新样本，不是原目标请求的 reasoning 分解。

## 6. actual provider 诊断

旧 observeChunk 仅读取顶层 provider。参考已 fork 的渠道监控源码及其 fixtures，
Cline 会在 `choices[].delta/message.provider_metadata.gateway.routing.finalProvider`
报告实际路由。因此旧 1,515 条 provider 全空是缺失取证，不能证明 provider 恒定。

v0.1.10 补充这几个路径和顶层 metadata：finalProvider 优先；后到的 final 覆盖
早先 serving-provider fallback；resolvedProvider/candidate/fallback 列表不算实际 provider。
只保留 provider 名称，不保留 planningReasoning、metadata 原文或 secret。

v0.1.10 在 12:07:20 启动；截至 12:10:38，新增 44 条同模型真实请求，
均为 `/v1/responses`，全部提取到 particle。两个 session 分别有 24、20 条，
在这个有限窗口没有观察到 provider 切换。

| Actual provider | Warm requests | 首上游事件 median | p90 | 首 SSE 后合计 TPS median | Cache hit median |
|---|---:|---:|---:|---:|---:|
| particle | 32 | 2.6045 s | 3.089 s | 468.97 | 99.608% |

该表限定 www、同模型、成功、cache >90%；其余 12 条没有混入 warm 统计。
只有一个 provider，没有可比较的其他 provider；上述 468.97 也不是可见文本 TPS。
最近的 pi `/messages` 请求开始于 12:02:31，早于这次部署；部署后尚未收到新的
pi 样本。因此不能证明旧慢 pi 也由 particle 服务。

旧请求原始响应未保存，旧 provider 不能追溯补造。即便新样本能识别，
也不能替代原请求的 actual provider。

## 7. 截图中的 Responses 重连

旧 Usage() 在 reasoning 为 0 时输出 `output_tokens_details:{}`。
Codex 完成事件解析器要求 reasoning_tokens，报错后重试，表现为 Reconnecting。
v0.1.9 共享 Usage() 现在输出 `{"reasoning_tokens":0}`；stream 和 non-stream 都受益。
完全没有 upstream usage 时仍不伪造 token 数。

可重复检查：

```powershell
go test ./internal/translate -run TestResponsesUsageReasoningTokens -count=1 -v
go test ./internal/serving -run TestObserveChunkProvider -count=1 -v
```

Usage 回归覆盖缺失、显式零、非零、无 usage，在 stream/non-stream 两条路径共 8 个
场景。修复前零值/缺失值的 4 个场景失败，修复后全部通过。
NewAPI 生产版本的 native Responses relay 使用原 data string 转发，未重新序列化
usage，因此这条修复位于生成 malformed event 的共享位置。

截至 12:10:41，v0.1.9 部署之后留存的 395 条请求全部记录为 200，
systemd NRestarts=0。requests.jsonl 是滚动历史，这不是无限期错误率证明。
部署后 Proxy 成功记录不能证明客户端没有解析重试：Proxy 可以正常发完 HTTP 200，
客户端随后拒绝 completed event。未捕获生产零值 completed 的实际客户端 wire，
所以这里不声称所有断连都已解决。历史还出现过上游 connection reset、500 empty
response content、客户端取消；它们与截图的缺字段错误不是同一原因。

## 8. 发布与未完成的验证

v0.1.9 commit `5c94bb9`，CI run 36664803587、Release run 36664977696 均成功。
部署前等待 active_requests=0，服务优雅退出再启动；Actions binary SHA256 为
`4bf7fb1dc2ab0061972080cea29522b5a8ff2d671493dad004205f57102789a6`。
未在本机或 VPS 构建生产产物；保留上一版本回滚产物。

provider 诊断修复 v0.1.10 commit `5641c08`；
[CI](https://github.com/nonlog/ClinePassProxy/actions/runs/36667015499) 和
[Release](https://github.com/nonlog/ClinePassProxy/actions/runs/36667017850) 全部 jobs 成功，
包括 race tests、两种架构的 binary/connector 和容器镜像。
部署时再次等待 active_requests=0；journal 显示 draining、stopped、正常启动，
没有强制中止请求。`/health`、`/ready` 均 200，实际版本为 v0.1.10。
部署 binary SHA256 为
`49380f69938a2d9fc63e4165d8aaeddda6380e23a402011e6929ca76d296e48a`，
已比对 Actions release 的 checksum；v0.1.9 回滚 binary 保留。

没有更改 input converter、prompt prefix、SSE 批处理、连接池配置、NewAPI/CPA/Bridge，
没有新增 synthetic inference 测试请求或通过 inference 试探模型参数。

尚不能精确回答的部分：

- 原请求上传/DNS/TCP/TLS、provider 内部排队/prefill/cache restore 分解。
- 原请求 NewAPI 首 data 的绝对时间、各 relay 内部阶段、pi UI 绘制时间。
- 同一精确 body 的 A（NewAPI）/B（直连 Proxy）/C（直连 Cline）回放：未捕获原 HTTP
  body，不用不同结构或 tiny prompt 替代。
- 原请求 exact-body allocation/pprof：本地 585 ms 有优化空间，但占比小于响应头前
  等待。未用重建的不同 body 做 profile，也未据 allocation 猜测优化。
- provider-specific cache/affinity 因果结论：需实际路由证据与同 body 对照。

当前最明确的判断是：**原目标的 cache 正常；34 t/s 不是 decode 速度；主要延迟
在 Cline 响应头前，以及 NewAPI 输入/调度到 Proxy 到达的区间。**
大 pi 与小 Codex 在请求结构、body 和上下文不同的情况下只能提供关联证据，
不能据此声称某客户端、provider 或 Proxy 实现就是唯一原因。
