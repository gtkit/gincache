## 1. TwoLevelStore 行为

- [x] 1.1 为 TwoLevelStore 增加 Redis Pub/Sub L1 失效广播。
- [x] 1.2 将广播客户端契约改为 `redis.UniversalClient`。
- [x] 1.3 增加 `WithTwoLevelLogger` 并记录发布、订阅、解析、本地失效错误。
- [x] 1.4 增加订阅启动确认超时，失败时禁用广播并记录日志。
- [x] 1.5 为监听 goroutine 增加 panic recover 和正常关闭识别。
- [x] 1.6 为 TwoLevelStore 内部 singleflight 增加超时 `Forget`。

## 2. 验证与文档

- [x] 2.1 覆盖跨实例 Set/Delete 失效广播测试。
- [x] 2.2 覆盖 publish 失败、payload 解析失败、本地删除失败、订阅启动失败日志测试。
- [x] 2.3 覆盖正常 Close 不记录订阅异常关闭测试。
- [x] 2.4 更新 README 和 example 展示广播、日志、订阅超时和 singleflight 超时配置。
