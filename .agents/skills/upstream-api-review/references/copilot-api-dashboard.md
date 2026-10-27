# Copilot API Dashboard 项目参考

本文件列出 `NERVEbing/copilot-api-dashboard` 的代码入口与需要核对的业务口径。路径相对各自仓库根目录；判断以当前检出的实现和目标提交下的上游源码为准。

## 代码入口

上游是 [caozhiyuan/copilot-api](https://github.com/caozhiyuan/copilot-api)，与 Dashboard 自己的 `origin` 不同。下列路径相对各自仓库根目录。

| 检查对象 | Dashboard | 上游位置 |
| --- | --- | --- |
| HTTP 路径、鉴权、参数、响应校验 | `internal/upstream/client.go` | `src/server.ts`、`src/lib/request-auth.ts`、相关 route |
| Copilot 身份与额度 | `internal/upstream/types.go` | `src/routes/usage/route.ts`、`src/services/github/get-copilot-usage.ts` |
| 用量汇总、每日数据和事件 | `internal/upstream/types.go` | `src/routes/token-usage/route.ts`、`src/lib/token-usage/store.ts` |
| 账号解析与聚合 | `internal/dashboard/service.go`、`internal/dashboard/aggregate.go` | 对应响应的生成代码 |
| 历史快照 | `internal/store/sqlite/store.go` | 每日分桶与快照生成代码 |
| 计算、展示与排序 | `web/app.js`、`web/app.test.cjs` | 原始服务契约、权威客户端及测试 |
| 契约来源与样例 | `internal/upstream/testdata/README.md` 及相邻 JSON | 精确提交下的源码 |

## 当前调用边界

- 客户端使用 `GET /usage`、`GET /token-usage`、`GET /token-usage/daily`、`GET /token-usage/events`，凭据头为 `x-api-key`。
- 周期、分页和必须存在的字段以客户端校验为准。特别检查 `login`、`period`、`totals`、`byModel`、`costs`、`days`、`items`。
- 账号按 `/usage` 返回的 Copilot `login` 识别和去重。统一订阅响应 `/usage/{provider}` 不等价于账号发现响应；增加 provider 的额度展示还需要明确身份与页面维度。
- 额度和请求事件实时读取；SQLite 保存每日 token、请求、费用及模型统计，不保存额度 Used/Usage。不要将额度展示修正误报为历史库迁移。
- Docker discovery 只从容器环境变量读取 `COPILOT_API_KEY`，不会读取上游 `config.json` 的 `auth.apiKeys`。现有最小配置路径是同一规范化 URL 的 YAML endpoint 指定普通 API key，YAML 优先；不要为此自动新增配置读取机制。

## 检查重点

### 字段未变也可能需要修正

日期分桶或时区修正可能在 JSON 结构不变时改变每日统计。检查这类变化时，要同时核对上游日期、`TZ`、日区间、快照版本和 Dashboard 的覆盖规则。

当前历史存储允许较新的快照纠正过去日期，并保留上游数据库重建的分段保护。评估进一步修改时，应查看现有 reset、明细完整性和快照顺序检查；不要以计数下降一项就推断数据无效，也不要无条件覆盖或累加。上游省略的日期与显式零值日期含义不同。

### 额度计算需要原始口径

额度计算和回退规则位于 `web/app.js`，对应边界条件见 `web/app.test.cjs`。有限额度应分别核对绝对值和百分比：

- Used：有效总额度下的 `max(0, entitlement - quota_remaining)`；缺少剩余额度时，才由有效百分比推算。
- Usage：`100 - percent_remaining`，并按当前实现限制百分比范围。
- `credits_used` 没有 `entitlement` 这个分母，不能用两者计算有限额度使用率。
- 无限额度的有效 `credits_used` 可以单独展示，但 `has_quota === false` 时不能当成可用额度。

保留零与缺失的区别、可空字段和数字字符串形式的 `entitlement`。表格、详情与排序需要使用同一口径。原始 `/usage` 已有计算所需字段，不能仅为获得统一格式就切换接口。

口径依据见项目契约说明及 [VS Code `getQuotaUsage` 的参考实现](https://github.com/microsoft/vscode/blob/3ac0f0563e382d8e3480c787d04a2d05afca6746/src/vs/workbench/services/chat/common/chatEntitlementService.ts#L865-L894)。如果上游计费契约变化，应重新核实原始字段含义和权威实现。

### 缓存率与性能数据不能直接平均

缓存命中率为 `cache_read_input_tokens / (input_tokens + cache_read_input_tokens + cache_creation_input_tokens)`。多账号应先合并计数再计算比例；缺失、非法、负数或零分母不能显示为有效的 0%。

`duration_ms` 和 `request_outcome` 已接入，缺失或 `null` 的耗时不能变成零。`completed` 表示 HTTP/body 完成，不保证模型成功；正常结束的 SSE body 中仍可能含协议错误。

比较上游平均 TTFT、输出速度或 `/token-usage/sessions` 时，先核对 `internal/upstream/types.go` 和页面代码的接入范围。没有合适的样本权重或生成时间，不能直接合并多账号平均值。

## 适配与验证约定

- 上游契约来源、对比基线及合成样例说明维护在 `internal/upstream/testdata/README.md`。不为记录上游跟进向公共 `README.md` 增加来源或版本追踪内容；用户功能说明按实际变化处理。
- 只读检查可按风险运行相关 Go 包和 `node --test web/app.test.cjs`。实施后的完整检查使用当前 Makefile 的 `make check`；有竞态相关改动时再运行必要的 race tests。
- 合成响应验证字段映射、空值和计算；隔离 HTTP/浏览器验证交互与实时、持久化两条路径；真实上游实例的验收另外说明。报告本次实际执行的检查和结果。
- 用户要求设计或实现的独立审阅时，按本次约定执行；设计审阅不能替代最终当前 diff 的代码审阅。只读比较本身不自动引入设计文档或审阅门槛。
