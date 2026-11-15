# CloudNest Bug 修复记录

## 2026-11-15——目录项联测用户夹具遗漏必填展示名称

### 问题现象

第 104 步首次真实 MySQL 联测中，目录项单元测试通过，但三次目录项集成测试均在创建用户夹具时失败，MySQL 返回 Error 1364，提示 display_name 没有默认值。修复前记录于此，接下来核对正式 users 表必填列，并补齐普通与最大整数用户夹具。

### 根因与修复方案

正式 users.display_name 为 NOT NULL 且没有默认值。新增联测的普通用户和最大整数用户插入语句仅提供了登录名与密码摘要，未按正式表结构补齐展示名称。为两类夹具补充 display_name，继续复用正式表和原有独占库清理机制。

### 验证结果

修正后目录项六组主场景及用户资料真实 MySQL 联测连续三次通过。目录项基础投影、用户隔离、历史状态、未提交写入下的已提交快照、完整 uint64 和故障分类通过。go test -count=1 ./...、go vet ./...、gofmt 检查、git diff --check 和学习记录 PowerShell 示例语法检查均通过，独占库清理复核通过。

## 2026-11-13——修改设备名称的 HTTP 测试保留了退出请求提示断言

### 问题现象

第 090 步首轮定向测试中，名称 DTO 与登录校验回归通过，HTTP 校验测试有七个名称字段错误场景失败，提示固定字段错误提示不正确。检查新增测试发现，字段断言仍比较退出请求的“客户端类型须为 web 或 native”，实际名称 DTO 使用设备名称规则提示。修复前记录于此，接下来调整测试期望并重新运行定向及全量检查。

### 根因与修复方案

新增测试复用已有退出请求的读取和错误验证结构时，遗漏替换字段错误的固定提示。将断言更新为设备名称的既有公开规则，并继续校验 name 字段、请求 ID、禁止缓存与错误输入停止后续处理。

### 验证结果

修正后七个失败场景与本轮全部 32 组 HTTP 输入场景通过。22 组名称场景、65 个控制字符、JSON 往返与登录单元/HTTP 回归通过。go test -count=1 ./...、go vet ./...、gofmt 检查及 git diff --check 均通过。

## 2026-11-13——撤销其他账号会话时等待了目标行锁

### 问题现象

第 078 步新增真实 MySQL 并发用例，独立事务持有其他账号目标会话的行锁，当前账号尝试撤销该 ID。原方法直接使用 user_id/id 条件执行 FOR UPDATE，在 200 毫秒请求期限后返回 context deadline exceeded，未按资源隔离规则返回会话不存在。修复前记录于此，接下来检查主键锁定与归属筛选的执行路径。

### 根因与修复方案

原方法直接进行带用户过滤条件的主键锁定读取。独占库实测表明，其他账号目标行已被锁定时，该查询会等待目标行，SQL 中的 user_id 条件无法保证在锁等待前完成归属拒绝。

在加锁前复用已有 FindSessionAuthorization，以普通一致性读取定位当前用户的目标。其他账号或不存在的 ID 直接返回 ErrSessionNotFound；通过后继续 FOR UPDATE 读取目标当前状态，复查完整性并执行条件更新。同一用户的认证写入继续由外层用户锁串行。

### 验证结果

修正后的新增方法及既有当前会话撤销 MySQL 联测连续三次通过。其他账号的目标行锁保持未释放时，200 毫秒期限内正确返回 ErrSessionNotFound；本人目标行锁保持时，正确返回 context.DeadlineExceeded 并回滚。重复及并发撤销保留首次时间，错误/panic/取消回滚、当前身份失效和完整 uint64 标识验证通过。独占验证库已自动清理并复核。go test -count=1 ./...、go vet ./...、gofmt 检查及 git diff --check 均通过。

## 2026-11-12——刷新输入测试对静态 map 字段使用类型断言

### 问题现象

第 059 步首轮测试中，RefreshRequest 单元测试通过，HTTP 测试因对 body.Error.Details 使用类型断言而编译失败，提示 map[string]any 不是接口类型。修复前记录于此，接下来按现有 ErrorDetail 字段定义修正断言。

### 根因与修复方案

ErrorDetail.Details 已声明为 map[string]any，原测试按动态接口值处理并再次断言为 map。改为直接取得该 map，再检查长度及 field 值。

### 验证结果

修正后 17 组 HTTP JSON 读取与字段校验用例通过，DTO 校验及凭证格式边界用例通过。go test -count=1 ./...、go vet ./...、gofmt 检查及 git diff --check 均通过。

## 2026-11-12——刷新事务的到期测试未进入预期拒绝分支

### 问题现象

第 053 步首轮真实 MySQL 独占库测试中，用 UTC_TIMESTAMP(6) 将会话截止时间设为数据库当前时间后，刷新事务仍执行了回调，未返回预期 ErrSessionExpired。其余锁定、状态复查、提交、回滚及并发用例通过。修复前记录于此，接下来核对测试夹具与后端采用的时间基准。

### 根因与修复方案

夹具把数据库当前时间直接当作应用时钟下已经过去的时间。三次定向诊断中，读取回来的截止时间比 Go time.Now 分别晚约 3.49、3.97 和 4.32 毫秒，两次因此仍在有效期内。数据库与宿主机的当前时间存在毫秒级差异，原到期断言依赖两个时钟的先后关系。

将真实数据库夹具的截止时间设置为数据库当前时间减一分钟，明确表达已经到期的状态。应用继续在取得锁后使用 time.Now 检查截止时间，精确边界由上一步可控时钟的服务单元测试验证。移除临时诊断输出。

### 验证结果

修正后完整真实 MySQL 刷新事务测试连续三次通过，包含状态复查、三类行锁、等待后代次检查以及提交/回滚。27 组单元结果、入口与嵌套边界、查询取消及期限验证通过。go test -count=1 ./...、go vet ./...、gofmt 检查与 git diff --check 均通过，各次独占测试库已自动清理并复核。

## 2026-11-12——登录交付测试的长度头与 Secure Cookie 断言失败

### 问题现象

第 048 步首轮定向测试中，五组成功交付在组合响应头断言处失败，失败输入与已提交响应用例通过。HTTPS Cookie Jar 用例中，将本机回环 URL 改为 HTTP 后仍读到 Cookie，原断言未通过。修复前记录于此，接下来核对本机 Gin 渲染和 Go Cookie Jar 的实际行为。

### 根因与修复方案

本机 Gin v1.12.0 的 Data.Render 会按编码正文设置新的 Content-Length，原测试错误地要求该头为空。断言改为与实际正文的字节长度一致，继续检查旧长度和编码已被正确处理。

本机 Go 1.27 Cookie Jar 将 localhost 与回环 IP 视为安全来源，因此本机 HTTP 的行为无法证明普通主机的 Secure 限制。保留真实 HTTP/HTTPS 收取 Cookie 的验证，另用内存中的 cloudnest.example HTTPS/HTTP URL 核对普通主机的 Secure 范围，无外部网络请求。

### 验证结果

修正后五组成功交付、八组失败输入、已提交响应及 HTTP/HTTPS Cookie Jar 用例通过，普通主机的 Secure 限制也已验证。go test -count=1 ./...、go vet ./... 与 git diff --check 均通过。

## 2026-11-12——启动测试的用户配置目录隔离需要按系统选择

### 问题现象

第 046 步自检发现，新增测试只为 Windows 设置 APPDATA、为其他系统设置 XDG_CONFIG_HOME。Go 在 macOS/iOS 使用 HOME 下的 Library/Application Support，原测试写法在这些系统上无法确保密钥位于临时目录。

### 根因与修复方案

按本机 Go 的 os.UserConfigDir 系统分支设置测试环境，并使用该方法返回的实际配置目录断言文件路径。环境改动由 t.Setenv 在测试结束时恢复。

### 验证结果

Windows 下配置测试、两次完整启动与重启测试通过，密钥位于临时配置目录，测试库已删除并复核。其他系统分支仅与本机 Go 源码核对。go test -count=1 ./...、go vet ./... 与 git diff --check 均通过。

## 2026-11-12——当前用户测试在正文解码后检查空缓冲区

### 问题现象

第 045 步测试自检发现，JSON 解码器直接读取 httptest 响应缓冲区后，后面的正文隐私断言读取到已消耗的缓冲区，无法覆盖原始正文。

### 根因与修复方案

测试改为使用独立 bytes.Reader 解码响应字节，使原响应缓冲区保留完整正文，供字段与隐私断言共同验证。

### 验证结果

当前用户的 18 组场景均通过完整正文与日志的敏感标记检查，真实 JWT/TCP GET/HEAD 联测通过。go test -count=1 ./...、go vet ./... 与 git diff --check 均通过。

## 2026-11-11——全量检查中 MinIO 超时测试未保留预期错误类型

### 问题现象

第 040 步全量执行 go test -count=1 ./... 时，既有 TestNewClientTimeoutAndCancellation/10s 在约 5 秒后失败，返回固定 MinIO 桶访问检查错误，未满足测试预期的超时错误语义。本轮登录会话服务单元测试及真实 MySQL 独占库验证通过。修复前记录于此，接下来单独复查既有超时路径与测试条件。

### 根因与修复方案

原超时用例单独重跑通过。检查现有实现发现，传输层 ResponseHeaderTimeout 和 CheckBucket 上下文都设置为 5 秒，但错误处理只在 checkContext.Err() 已非空时保留超时类型。传输层先返回且上下文仍有效时，错误落入通用网络提示，丢失可判断类型。新增用例把传输层期限缩为 20 毫秒、调用方期限设为 1 秒，在调用方上下文仍有效时稳定复现该问题。

修复将 SDK 返回的 net.Error 超时也转换为包裹 context.DeadlineExceeded 的固定本地提示，既有上下文取消分支保持优先。原始地址、凭证与错误文本继续由固定提示屏蔽。

### 验证结果

新回归在修复前稳定失败，修复后与原 100 毫秒/默认 5 秒超时、预先取消用例连续三次通过。新用例同时确认调用方上下文仍有效，返回错误可通过 errors.Is 判断 DeadlineExceeded，固定提示未回显地址或凭证。重新执行 go test -count=1 ./...、go vet ./... 与 git diff --check 均通过。

## 2026-11-11——校验 JWT 时结构体解码接受声明名大小写变体

### 问题现象

第 034 步检查校验器时发现，直接复用签发声明结构体会经过 Go JSON 的字段大小写兼容匹配，可能将 SUB/SID 等变体读作规范字段。JWT 声明名按精确名称处理，接下来使用签名有效但改写字段名的定向用例验证该边界。修复前记录于此。

首轮定向命令先被测试草稿预留的未使用 import 阻断，需移除这些导入后继续执行协议边界用例。

### 根因与修复方案

移除预留导入后，三个定向用例复现 SUB/SID/ISS 变体均错误地返回了身份。根因是 jwt.RegisteredClaims 及自定义结构体经过 Go JSON 的兼容字段匹配。校验器改用 jwt.MapClaims，按精确键读取标准声明和 sub/sid，继续使用库的签名与标准校验选项，同时检查时间字段及规范 ID。

### 验证结果

声明名变体回归用例通过，完整定向测试覆盖签发到校验、26 组声明异常、两类 ID 各 11 种异常、14 组异常文本/签名及 32 个并发请求，错误不返回身份。gofmt、go test -count=1 ./...、go vet ./... 与 git diff --check 均通过。

## 2026-11-11——JWT 声明测试的时间比较断言失败

### 问题现象

第 033 步首轮签发定向测试中，ClaimsAndSignature 的签名解析返回 nil 错误，但组合声明断言失败。其他有效期、异常输入、篡改、并发及持久化密钥用例通过。修复前记录于此，接下来检查解码后的时间表示与各声明的比较结果。

### 根因与修复方案

JWT 库将 NumericDate 解码为 time.Unix 的时间对象，测试期望值使用 UTC。两者的 Equal 比较均为 true，但 time.Time 的结构体 == 比较还涉及 Location，结果为 false。将解码后的时间断言改为 Equal，并分别检查身份、接收方及时间字段，移除定位时的临时布尔诊断输出。

### 验证结果

修正后的签发定向测试通过，包含完整 uint64 ID、独立 HMAC 核对、8 组有效期边界、无效输入、5 组验证拒绝、并发签发和持久化密钥复用。gofmt、go test -count=1 ./...、go vet ./... 与 git diff --check 均通过。

## 2026-11-11——JWT 配置测试输入与 Windows 环境变量不兼容

### 问题现象

第 031 步首轮定向测试中，invalid_utf8 用例没有按预期进入无效 UTF-8 分支，audience_control 用例在 t.Setenv 阶段报 invalid argument。两条用例分别尝试把无效 UTF-8 字节和含 NUL 的字符串传入 Windows 环境变量，测试在校验预期或环境设置阶段失败。修复前记录于此，继续检查平台输入表示与测试边界。

### 根因与修复方案

本机 Go 的 syscall/env_windows.go 使用 UTF16PtrFromString 设置环境变量。无效 UTF-8 转换后无法保持原始字节，NUL 则被环境变量接口拒绝，因此这两项数据无法通过该测试路径原样抵达应用校验函数。将无效 UTF-8/NUL 改为直接调用 validateJWTIdentity，环境变量控制字符用例改用可表示的制表符，继续验证完整 LoadJWTConfig 路径。

### 验证结果

修正后的 JWT 配置定向测试全部通过，覆盖环境变量覆盖、配置错误及校验函数的原始异常文本，正常中文标识也通过。gofmt、go test -count=1 ./...、go vet ./... 与 git diff --check 均通过。

## 2026-11-05：响应已提交后的 panic 在完成日志中被当作普通成功

### 问题现象

第 014 步核对异常恢复边界时，使用生产路由、假数据处理函数复现：先写出 200 与正文再 panic，HTTP 状态已经不能改为 500，现有完成日志却只有 INFO、status=200，没有异常标记。该问题不在于保留实际状态，而在于日志无法识别这次已恢复的异常。

### 根因与修复方案

原完成日志只按最终状态分级；响应已经写出后，Gin 不能再改状态，因此这类已恢复的 panic 会被误记为普通成功。新增 Recovery 包装，在回调前只设置布尔异常标记，不存原始载荷。InternalError 对未提交响应返回统一 500 JSON，对已提交响应仅 Abort。RequestLog 保留实际状态，对异常使用 ERROR 并增加 recovered_panic=true。

### 验证结果

先写出 200 正文后 panic、仅提交 202 头后 panic，两种响应的状态、正文与已有头均未被覆盖或拼接；完成日志都为 ERROR、recovered_panic=true，状态仍为实际值。未提交响应的多种 panic 返回固定 INTERNAL_ERROR JSON，正文、头和日志 ID 一致；原始异常及请求敏感标记不在响应或日志中。32 个正常与异常混合并发请求没有串号或标记泄漏。真实 TCP 测试确认统一 500、待发送头修正、HEAD 无正文及异常后继续服务；契约、全项目测试、go vet、git diff --check 通过。

## 2026-11-05：默认异常转储可能输出 Cookie 和查询参数

### 问题现象

第 012 步核对请求日志时发现，现有 gin.Default 附带的 Recovery 在调试模式发生 panic 时会输出请求转储。使用仅含假数据的独立程序复现，日志同时包含测试 Cookie 和 URL 查询参数；当前版本只对 Authorization 请求头进行遮盖，不能满足项目排除会话凭证和签名查询参数的日志约定。

### 根因与修复方案

Gin v1.12.0 的 secureRequestDump 只遮盖 Authorization，调试异常转储仍包含请求 URI 与其他请求头。改为 gin.New，显式装配 RequestID → RequestLog → gin.RecoveryWithWriter(nil)。Recovery 仍恢复异常并返回 500，但不输出原始请求或 panic 转储；RequestLog 在恢复之后只记录最终 ID、方法、URL.Path、状态与耗时。

### 验证结果

9 种响应场景（含正常响应、提前终止、取消和 panic 恢复）均只输出一条 JSON 完成日志，最终状态、级别和 ID 与响应一致。测试同时捕获默认异常输出，确认查询、Cookie、其他请求头、正文、c.Errors 和 panic 原始载荷均未回显。32 个并发请求的响应与日志一一对应。真实服务验证三个系统接口与 404，JSON 正文保持原样，日志不含测试敏感标记，也没有旧 Gin 文本访问日志；正常退出顺序和退出码 0 通过。go test ./...、go vet ./... 与 git diff --check 通过。

## 2026-11-04：第 004 步连接器初始化的字符集设置失败

### 问题现象

连接池的取消与超时测试通过；第一次真实 MySQL 集成检查失败，返回 Error 1193 (HY000): Unknown system variable 'charset'。当前通过 mysql.NewConnector 直接传入 Config，将 charset 放入 Params 后，驱动将其当作普通会话系统变量处理。

### 根因与修复方案

当前 v1.9.2 驱动在 ParseDSN 时特殊处理 charset，将其保存到内部字符集字段；直接 NewConnector 不会经过此解析，将 Params 中的 charset 当作 SET 系统变量执行，因此报错。改为 driverConfig.Apply(mysql.Charset("utf8mb4", "")) 设置字符集，Params 仅保留会话 time_zone。连接池的只读集成用例保留为回归检查。

### 验证结果

真实 MySQL 集成检查已通过：连接可用、目标库正确、会话时区为 +00:00、字符集为 utf8mb4、DATETIME 解析为 UTC；连接池关闭后连接归零且不能继续使用。取消、100 毫秒调用方期限与默认 5 秒超时检查通过，超时连接在服务端确认已关闭。全项目 Go 测试、go vet 和 git diff --check 通过。真实服务启动后 /ping 返回 200；错误密码、不存在的数据库、不可达端口和 HTTP 端口占用均按预期失败，错误密码未出现在日志中。

## 2026-11-04：Trae 提示 MySQL 驱动无法导入

### 问题现象

Trae 打开 cmd/database/main.go 时提示 could not import github.com/go-sql-driver/mysql，原因显示 no required module provides package。当前根 go.mod 已声明 v1.9.2，项目根目录的 go test ./cmd/database 可以通过，首次验证重新下载了当前 GOPATH 中的驱动依赖。

### 排查结果

- 项目根目录与 Trae 工作区均为 F:\CloudNest，根 go.mod 已直接声明 MySQL 驱动 v1.9.2。
- 当前 Go 工具为 E:\Go\bin\go.exe，GOPATH 为 E:\Go\gopath。首次验证在此缓存重新下载了 MySQL 驱动及 edwards25519 依赖。
- Trae 的 gopls v0.24.0 日志出现 no package data for import github.com/go-sql-driver/mysql，确认编辑器曾未取得该包数据。
- 补齐缓存后，go list 和独立 gopls 检查均正常。缓存缺失与旧包加载状态是当前判断，编辑器内是否消除提示需刷新语言服务器后确认。

### 处理与验证

- 已通过 go test ./cmd/database 补齐当前缓存，模块版本沿用 go.mod。
- go list github.com/go-sql-driver/mysql 成功，go list -e -json ./cmd/database 的 Error 和 DepsErrors 为空。
- gopls check cmd/database/main.go 通过，未输出诊断。
- Trae 如仍显示旧错误，执行 Go: Restart Language Server，必要时 Developer: Reload Window 后复核。
- 用户后续确认 Trae 已正常，驱动导入提示已消除。
- 另一次完整 go mod verify 报告 mimetype v1.4.12、go-urn v1.4.0、x/arch v0.22.0 三个间接依赖的本机缓存内容变化，作为独立环境问题保留记录。

## 2026-11-04：SQL 迁移文件换行稳定性

### 问题现象

仓库启用了 Windows 自动换行转换，SQL 文件原先缺少专用换行约定。迁移以文件内容 SHA256 校验，换行被转换可能导致重新检出后校验失败。

### 修复方案与验证

为 SQL 文件设置 text eol=lf，保证迁移字节稳定。验证属性解析和实际文件摘要与数据库迁移记录一致，并执行重复迁移检查。

## 2026-11-03：Web 页面 Demo 首轮验证

### 问题现象

- 首次构建被未使用的图标 import 阻断（TS6133）。
- 静态检查发现上传的 Object URL 在 React 状态更新器内部创建，需避免重复执行造成资源泄漏。

### 根因

- 页面搭建时保留了未实际使用的图标 import，触发 TypeScript noUnusedLocals 检查。
- 上传任务原先在状态更新器中创建 Object URL，该更新器应保持纯计算，不能承担资源创建副作用。

### 修复方案

- 移除无用 import，将本地预览资源创建放入上传事件处理，再用纯状态更新追加任务。
- 补齐非文件页的面包屑导航和下载失败状态；提高主工作区文字可读性及移动端操作区域。

### 验证结果

- TypeScript 和 Vite 构建通过；浏览器控制台无错误或警告。
- 新建、重名及非法名称提示、重命名、收藏、目录移动、搜索、类型筛选、列表/网格、文件预览均通过浏览器验证。
- 上传暂停/继续、取消后不发布、拖拽添加、下载实际文本内容核对通过。
- 回收站恢复到原目录、永久删除确认、刷新重置演示状态通过。
- 390 px 手机与 1024 px 平板无页面级横向溢出，移动导航能正确打开并在跳转后关闭；完成 1440 px 桌面视觉检查。
- 构建存在组件库单包超过 500 kB 的提示，不影响本地 Demo；正式界面定稿后再进行分包优化。

## 2026-11-03：开发环境与基础容器准备

### 问题现象

- 现有 Compose 使用 MySQL 8.0，与修订方案约定的 8.4 LTS 不一致。
- Redis 没有启用认证，无法满足本项目统一开发账号密码的要求。
- MinIO 使用未固定的 latest 镜像，且密码 root 少于官方要求的 8 个字符，不能正常初始化。
- 容器尚无健康检查、项目隔离和端口冲突预检；本机已有其他项目容器，后端默认端口 8080 也已被占用。
- 首次启动发现 Docker 默认地址池耗尽（all predefined address pools have been fully subnetted），需要为本项目选择不与已有 Docker 网络或主机路由冲突的显式子网。

### 根因

- 旧 Compose 未同步修订后的数据库版本和存储来源要求，Redis 认证与健康检查缺失。
- MinIO 标准版本要求 secret key 至少 8 位，root 无法通过校验；latest 也不能提供可复现的学习环境。
- Docker 既有网络已耗尽默认地址池；仅检查当前监听会遗漏已停止容器的端口预留。
- 后端硬编码 8080，与用户已明确保留给其他项目的 8080–8090 冲突。

### 修复方案

- 固定 MySQL 8.4.13、Redis 7.4.12-alpine 及镜像摘要；MySQL 使用 root/root，Redis 配置 root/root ACL 并禁用匿名 default 用户。
- MinIO 使用官方 RELEASE.2025-10-15T17-29-55Z 对应固定源码提交和 SHA256，构建本地镜像；凭证采用 root/rootroot，保留官方最小长度规则。
- 因 Docker 构建器直接下载 GitHub 归档发生 EOF，改由宿主机下载，宿主机及构建阶段双重校验来源；未关闭 Go 模块校验。
- Compose 使用独立 cloudnest 项目、127.0.0.1 端口映射、命名数据卷、健康检查和内存上限；经冲突核验使用 10.253.81.0/24 显式网段，不清理其他网络。
- 增加端口/子网预检、启动脚本、真实协议验收脚本及开发会话环境加载脚本；后端默认改为 127.0.0.1:18081，并支持 CLOUDNEST_HTTP_ADDR。
- 复用现有 F:\Android\Sdk；为当前执行会话加载正确路径后，Flutter 全部检查通过。补装 Wails v3.0.0 CLI。

### 验证结果

- Compose 配置、端口绑定、所有现存容器（含已停止容器）的映射、Docker/主机子网检查通过；三个组件均 healthy。
- MySQL 映射端口握手、root 网络认证和 SQL 实际读写通过；Redis 匿名拒绝、root ACL 登录及 SET/GET/DEL 通过。
- MinIO 私有 cloudnest 桶、SigV4、Multipart 初始化/上传/列举/完成/中止、SHA256 与 Range 校验、匿名访问拒绝通过；控制台返回 HTTP 200。
- Go 编译及 /ping 冒烟、go test ./...、go vet ./... 通过；当前项目没有自动测试用例，不据此宣称文件业务已经实现。
- flutter doctor -v、wails3 doctor 通过；Start-Infra.ps1 已实际执行成功。
- 对比 32 个既有容器的状态和 StartedAt，均未变化；临时后端校验进程已退出。
- 真机 APK、托盘 PoC、安装包签名、长期存储选型及备份恢复仍按后续阶段执行，未计作本次已完成验证。
