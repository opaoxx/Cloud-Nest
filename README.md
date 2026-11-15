# CloudNest

学习导向的自用文件云盘，采用 Go 后端、React Web、Wails 桌面端和 Flutter 安卓端。

当前已完成本机开发环境、基础容器及首期八张业务表初始化。后端提供 /ping、/health/live、/health/ready、受保护的 GET /api/v1/entries、GET /api/v1/entries/{id}、GET /api/v1/me、GET /api/v1/sessions、GET /api/v1/devices、PATCH /api/v1/devices/{id}、DELETE /api/v1/devices/{id} 和 DELETE /api/v1/sessions/{id}，以及 POST /api/v1/auth/login、POST /api/v1/auth/refresh 和 POST /api/v1/auth/logout，支持配置及 CORS 白名单放行/预检、JSON 日志、依赖客户端、退出信号处理、请求 ID 和统一错误响应。启动时加载 JWT 配置并检查数据库连接、迁移记录及已有存储桶访问，文件业务按技术方案分阶段实现。

## 数据库迁移

开发库 cloudnest 已应用版本 1，包含八张业务表和 schema_migrations。业务账号为 root，开发密码为 root，密码以 bcrypt 存储，配额为 100 GiB，根目录已创建。

~~~powershell
go run ./cmd/database -database cloudnest -seed
~~~

重复执行会核对迁移摘要并复用已有账号与根目录。配置、隔离验证和异常处理参见 [迁移说明](migrations/README.md)。

## Web 页面 Demo

已提供可操作的前端假数据页面，直接访问 [CloudNest Demo](http://127.0.0.1:5173)。支持文件浏览、上传进度模拟、预览、搜索、收藏、移动与回收站，刷新后重置。

~~~powershell
pnpm --dir apps/web install
pnpm --dir apps/web dev
~~~

本 Demo 独立运行，不需要启动后端或容器。具体能力与演示边界见 [Web Demo 说明](apps/web/README.md)。

## 开发环境

在项目根目录打开 PowerShell：

~~~powershell
. ./scripts/Enter-Dev.ps1
./scripts/Start-Infra.ps1
python scripts/verify_infra.py
go run ./cmd/server
~~~

Enter-Dev.ps1 为当前 PowerShell 加载现有 Android SDK、ADB、Wails CLI 和后端地址；默认 SDK 为 F:\Android\Sdk，不修改系统级环境变量。

后端读取 CLOUDNEST_DB_ADDRESS、CLOUDNEST_DB_NAME、CLOUDNEST_DB_USER、CLOUDNEST_DB_PASSWORD，未设置时分别采用 127.0.0.1:13309、cloudnest、root、root。配置说明见 [第 003 步学习记录](docs/开发学习记录.md#第-003-步读取并校验-mysql-连接配置)。

CLOUDNEST_ALLOWED_ORIGINS 配置逗号分隔的前端来源，未设置时为 http://127.0.0.1:5173，例如 http://127.0.0.1:5173,http://localhost:5173。启动时校验、规范化并去重，无效配置停止启动；现已接入 CORS：允许来源附带明确跨域响应头，未知来源或非法预检返回统一 403；合法 OPTIONS 预检返回 204，无 Origin 请求沿用原流程。浏览器可读取 X-Request-ID，白名单包含来源不代表已经实现对应业务接口。具体规则与体验见 [第 016 步学习记录](docs/开发学习记录.md#第-016-步接入-cors-来源检查与预检处理)。

存储配置读取 CLOUDNEST_STORAGE_PROVIDER 及 CLOUDNEST_S3_ENDPOINT、PUBLIC_ENDPOINT、BUCKET、ACCESS_KEY、SECRET_KEY。默认 provider 为 local_minio，内部地址为 127.0.0.1:19000，公开地址继承内部地址，桶为 cloudnest，凭证为 root/rootroot。两个地址支持显式 HTTP/HTTPS；省略协议时按 HTTP 解析。配置规则见 [第 008 步学习记录](docs/开发学习记录.md#第-008-步读取并校验-minio-存储配置)。

后端现已在数据库检查后创建 MinIO 客户端，并在最多 5 秒内只读检查已有桶是否可访问；检查失败则停止启动，不自动创建桶。因此启动后端前需让 MySQL 与 MinIO 均处于可用状态。实现及只读测试方法见 [第 009 步学习记录](docs/开发学习记录.md#第-009-步创建-minio-客户端并检查已有桶)。

启动 HTTP 前会在最多 5 秒内检查 MySQL 连接，失败记录原因并退出。因此运行服务前需要先启动本项目 MySQL；/ping 保留原有响应，不承担数据库持续健康检查。连接池及只读集成验证见 [第 004 步学习记录](docs/开发学习记录.md#第-004-步建立-mysql-连接池并检查连通性)。

连接通过后，还会在最多 5 秒内核对 schema_migrations 的版本、dirty 状态和迁移 SQL 摘要；当前只接受版本 1 的完整记录。检查失败则停止启动，迁移仍由独立命令执行。检查范围及自动清理隔离库的测试方法见 [第 005 步学习记录](docs/开发学习记录.md#第-005-步启动时检查数据库迁移记录)。

启动成功后，GET /health/live 无需登录，返回 {"status":"ok"}，表示 HTTP 服务能够响应。这个请求不检查数据库或存储的持续可用性；接口契约见 [OpenAPI](api/openapi.yaml)，路由和处理函数的调用过程见 [第 006 步学习记录](docs/开发学习记录.md#第-006-步提供存活检查并拆分-http-路由)。

GET /health/ready 每次请求都复查 MySQL 连接和已有 MinIO 桶访问，共享最多 5 秒；全部可用返回 200 与 {"status":"ready"}，任一检查失败返回 503 与 {"status":"not_ready"}，响应不缓存也不返回底层错误。依赖故障时 live 仍可返回 200，恢复后下次 ready 重新检查。调用过程与验证见 [第 010 步学习记录](docs/开发学习记录.md#第-010-步提供依赖就绪检查接口)。

HTTP 响应现附带 X-Request-ID：单个标准 UUID v4 输入会保留并规范为小写，缺失、无效或重复输入则生成新值；同一个最终 ID 保存在请求上下文中。三个系统接口的 JSON 正文保持原样。体验及代码讲解见 [第 011 步学习记录](docs/开发学习记录.md#第-011-步增加请求-id-中间件)。

HTTP 处理完成后输出一条 JSON 日志，记录 request_id、method、path、status 和 duration_ms；可用响应头里的 ID 在终端查找对应请求。日志不记录查询参数、请求头或正文，异常恢复也不输出原始请求转储。字段、执行顺序和验证见 [第 012 步学习记录](docs/开发学习记录.md#第-012-步使用请求-id-记录-http-完成日志)。

未匹配到接口时，404 返回 error 与 request_id；错误码为 ROUTE_NOT_FOUND，details 明确为 null。正文、响应头和日志中的 ID 一致，便于查找对应请求。共享结构与体验方法见 [第 013 步学习记录](docs/开发学习记录.md#第-013-步统一未注册路由的-404-错误响应)。

普通 panic 被恢复且响应未提交时，返回相同错误结构的 500，错误码为 INTERNAL_ERROR，不回显原始异常。已提交的响应保持原样，不追加错误 JSON；完成日志以 ERROR 和 recovered_panic=true 标记异常，status 保留实际值。边界与测试方法见 [第 014 步学习记录](docs/开发学习记录.md#第-014-步统一可恢复异常的-500-响应)。

按 Ctrl+C 后，服务停止接收新请求，最多等待在途请求 30 秒，随后释放存储空闲连接、关闭数据库连接池并退出；等待超时会强制关闭 HTTP 连接并报告失败。信号与 HTTP 关闭的调用过程见 [第 007 步学习记录](docs/开发学习记录.md#第-007-步响应退出信号并关闭服务资源)。

Start-Infra.ps1 会先校验所有容器（包括已停止容器）的端口预留、主机端口绑定和子网，再按校验通过的官方源码构建本地 MinIO 并启动 CloudNest 容器。首次构建需要下载依赖。

普通 JSON 接口可调用 binding.ReadJSON，将单个 JSON 对象读入请求 DTO，并限制正文为 1 MiB；输入错误返回统一 400/413/415，不回显原始内容。当前系统 GET 不读取正文，登录端点尚未实现。调用示例及无需容器的完整验证见 [第 017 步学习记录](docs/开发学习记录.md#第-017-步封装有大小上限的-json-请求读取)。

已提供登录请求 DTO 和用户名、密码、安装 UUID、设备名称、平台及客户端类型校验；字段错误返回 400 与 details.field。当前登录端点尚未实现，校验通过不代表已认证。规则、调用示例和测试见 [第 018 步学习记录](docs/开发学习记录.md#第-018-步定义登录请求-dto-与字段校验)。

已提供按用户名只读查询登录账号的 Repository：GORM 复用现有连接池，只读取 ID、密码摘要和状态，区分账号不存在与数据库故障；查询最多 5 秒且服从调用方取消。当前通过测试使用，尚未接入真实登录。代码职责及只读集成验证见 [第 019 步学习记录](docs/开发学习记录.md#第-019-步按用户名只读查询登录账号)。

已提供 bcrypt 密码比对方法，区分匹配、不匹配和摘要格式异常；密码保持原文，不记录密码或摘要。现有 root 的查询与比对已通过只读测试，登录流程尚待编排。调用方式及验证见 [第 020 步学习记录](docs/开发学习记录.md#第-020-步使用-bcrypt-比对原始密码)。

已提供凭证校验服务，串联账号查询、密码比对和状态检查；账号不存在与密码错误返回相同结果，成功只返回内部用户 ID。当前不创建会话、设备或令牌，生产登录接口尚未接入。调用与验证见 [第 021 步学习记录](docs/开发学习记录.md#第-021-步组合账号凭证校验服务)。

已提供登录来源校验器：单个 X-CloudNest-Client 必须与正文 client_type 一致；web 必须携带白名单 Origin，native 必须无 Origin。不合规时在凭证查询前返回统一 403。当前通过测试专用入口验证，生产登录端点尚未注册；规则与测试见 [第 022 步学习记录](docs/开发学习记录.md#第-022-步校验登录请求的客户端来源)。

已提供登录失败限流的内存核心：按 IP 和用户名分别使用 60 秒窗口，失败加在途预留最多各 10，记录上限 4096；完成回调只结算一次，并发请求不能同时突破额度。计数只保存在本进程，HTTP 适配与 429 见第 024 步。使用和验证见 [第 023 步学习记录](docs/开发学习记录.md#第-023-步实现有界的登录失败限流核心)。

已提供 HTTP 登录限流适配器：只信任 TCP RemoteAddr，忽略客户端转发头；受限返回统一 429 和 Retry-After，允许的跨域页面可读取该头。取消或异常会释放名额，当前仍通过测试入口验证。调用及协议见 [第 024 步学习记录](docs/开发学习记录.md#第-024-步接入-http-登录限流边界)。

已提供按用户 ID 与安装 ID 只读查询设备的 Repository，保留撤销时间，区分不存在与数据库故障，避免仅凭安装标识跨账号查询。当前未写入设备或接入登录端点，使用和验证见 [第 025 步学习记录](docs/开发学习记录.md#第-025-步按用户与安装标识只读查询设备)。

已提供登录写入的用户事务入口：锁定并复查 ACTIVE 账号，回调使用同一事务 Store 与上下文，成功提交，错误/取消/异常时回滚。当前尚未写入生产设备或会话；调用方式及隔离库验证见 [第 026 步学习记录](docs/开发学习记录.md#第-026-步建立登录写入的用户事务入口)。

已提供事务内的设备创建方法：归属直接取已锁定用户，只插入安装 ID、名称和平台等必要字段，自增设备 ID 由数据库回填；重复安装标识返回明确冲突，不覆盖原记录。当前通过独占测试库验证，尚未接入生产登录；代码顺序与体验命令见 [第 027 步学习记录](docs/开发学习记录.md#第-027-步在用户事务内创建登录设备)。

已提供登录设备准备方法 EnsureLoginDevice，在用户事务内组合查询、缺失时创建和已有设备复用。按用户确认的规则，重新验证密码后可恢复已撤销设备并沿用原 ID，旧会话继续保持撤销。已有名称和平台沿用当前记录。执行过程及验证命令见 [第 028 步学习记录](docs/开发学习记录.md#第-028-步组合登录设备的查询创建与恢复)。

已提供 GenerateRefreshToken，使用 32 字节安全随机数生成 43 字符 Base64URL 令牌，并返回令牌文本的 32 字节 SHA256 摘要，供后续会话写入使用。生成过程与原文、摘要的职责见 [第 029 步学习记录](docs/开发学习记录.md#第-029-步生成-refresh-token-与存储摘要)。

已提供事务内的 CreateLoginSession，锁定并复查设备归属和撤销状态，写入用户、设备、刷新摘要及 UTC 到期时间，返回数据库会话 ID。设备准备与会话创建已通过同事务提交/回滚验证。调用示例及测试命令见 [第 030 步学习记录](docs/开发学习记录.md#第-030-步在用户事务中创建登录会话)。

已提供 LoadJWTConfig(localSecret)，支持环境变量覆盖传入的本地密钥，校验无填充 Base64URL 密钥和 issuer/audience，并读取两类令牌有效期，默认分别为 15 分钟与 30 天。参数与调用说明见 [第 031 步学习记录](docs/开发学习记录.md#第-031-步读取并校验-jwt-签名配置)。

已提供 LoadOrCreateJWTConfig(secretPath)，供后续启动装配调用。首次生成并保存 32 字节随机签名密钥，后续校验并复用同一路径的文件，环境密钥优先。文件发布采用同目录临时文件与硬链接，Windows 访问权限由目录 ACL 管理。真实文件、并发初始化及新进程复用的验证见 [第 032 步学习记录](docs/开发学习记录.md#第-032-步初始化并复用本地-jwt-签名密钥)。

已提供 AccessTokenSigner，使用固定 HS256 为已提交会话签发 JWT，包含字符串用户/会话 ID、issuer/audience 和整秒时间声明。到期时间取配置有效期与会话截止时间的较早值。签发、真实签名核对和边界测试见 [第 033 步学习记录](docs/开发学习记录.md#第-033-步使用登录会话签发-access-token)。

已提供 AccessTokenVerifier，固定校验 HS256、签名、issuer/audience 和时间声明，按规范十进制解析用户与会话 ID，返回供后续数据库授权查询使用的标识。声明名精确匹配，校验失败返回固定错误，单纯过期单独区分。流程与测试见 [第 034 步学习记录](docs/开发学习记录.md#第-034-步校验-access-token-并取得身份标识)。

已提供 FindSessionAuthorization，通过一条关联查询按用户和会话 ID 读取账号状态、会话到期/撤销及设备撤销时间，供服务层进行请求授权判断。字段、普通一致性读取与隔离库验证见 [第 035 步学习记录](docs/开发学习记录.md#第-035-步读取数据库会话授权信息)。

已提供 AuthorizationService，组合令牌校验和数据库状态查询，按账号、撤销及有效期规则完成普通请求授权，成功返回用户/会话/设备 Actor。查询期间到期和取消均会重新检查。服务编排及真实令牌配合内存状态的验证见 [第 036 步学习记录](docs/开发学习记录.md#第-036-步组合令牌与数据库状态完成请求授权)。

已提供 security.BearerToken，从单个 Authorization 头提取原始凭证，兼容 Bearer 方案大小写、连续分隔空格及头值两端的 HTTP 可选空白，拒绝重复头和非法字符。与真实令牌授权服务的 TCP 联测及学习说明见 [第 037 步学习记录](docs/开发学习记录.md#第-037-步从-http-请求头提取-bearer-令牌)。

已提供 requestcontext.WithActor 与 Actor，按值保存并读取授权后的用户、会话及设备标识，沿用父上下文的请求 ID、取消信号和期限。身份副本、并发隔离与 HTTP 传递验证见 [第 038 步学习记录](docs/开发学习记录.md#第-038-步保存与读取请求上下文中的-actor)。

已提供 RequireAuthorization 认证中间件，组合 Bearer 提取、授权检查与 Actor 传递，认证失败返回带请求 ID 的统一 401/403，未分类内部故障返回 500。中间件通过测试专用路由验证，装配、错误契约和运行方法见 [第 039 步学习记录](docs/开发学习记录.md#第-039-步组合受保护路由的认证中间件)。

已提供 LoginSessionService，接收已验证账号与登录设备字段，生成刷新凭证，在同一用户事务中准备设备并创建会话，提交成功后返回设备、会话和刷新原文。事务故障返回空结果，设备复用、恢复、回滚与真实 MySQL 并发验证见 [第 040 步学习记录](docs/开发学习记录.md#第-040-步组合登录设备与会话创建服务)。

已提供 LoginTokenService，组合已提交会话与 Access Token 签发，成功返回设备、会话和两类凭证。签发失败或提交后请求取消时返回空凭证结果及错误，数据库保留已提交会话。真实 JWT、刷新摘要和数据库授权联测见 [第 041 步学习记录](docs/开发学习记录.md#第-041-步组合登录会话与访问令牌签发)。

已提供 FindUserProfile，只读查询用户、真实活动根目录及配额快照，返回账号状态供后续服务层判断。未找到账号与缺少根目录分别处理，查询最多 5 秒且服从请求取消。字段映射、配额边界和真实 MySQL 验证见 [第 042 步学习记录](docs/开发学习记录.md#第-042-步只读查询用户根目录与配额资料)。

已提供 UserProfileService，复用 FindUserProfile 读取资料并复查账号状态，成功返回 ACTIVE 用户的资料副本。账号禁用、未知状态及查询故障分别保留对应错误，调用过程与真实 MySQL 验证见 [第 043 步学习记录](docs/开发学习记录.md#第-043-步读取用户资料并复查账号状态)。

已提供 UserSummary、UserProfileData 与转换方法，将用户和根目录 ID 映射为十进制字符串，三项字节计数映射为安全整数范围内的 JSON 数字，零值明确返回。字段示例、越界处理和测试入口见 [第 044 步学习记录](docs/开发学习记录.md#第-044-步映射用户资料响应-dto)。

已提供 CurrentUser HTTP 处理函数，从 Actor 取得用户 ID，调用资料服务与 DTO，返回带 request_id 的 200 资料响应并设置 no-store。账号未找到使用 AUTH_REQUIRED 401，禁用使用 403，缺少 Actor 等内部异常使用 500。测试路由、真实 JWT 联测与装配说明见 [第 045 步学习记录](docs/开发学习记录.md#第-045-步实现当前用户-http-处理函数)。

GET /api/v1/me 已接入生产启动流程，校验 Bearer JWT 并查询数据库状态，成功返回真实用户、根目录和配额。环境变量 CLOUDNEST_JWT_SECRET 优先，本地密钥默认位于 os.UserConfigDir()/CloudNest/jwt-secret，Windows 对应 %APPDATA%\CloudNest\jwt-secret。启动及完整隔离库验证见 [第 046 步学习记录](docs/开发学习记录.md#第-046-步启动装配并注册当前用户接口)。

已提供 LoginData/LoginResponse 与转换方法，复用用户资料 DTO，组合访问令牌、剩余秒数和设备/会话摘要。Web JSON 省略刷新原文，原生 JSON 明确交付刷新原文。字段、取整边界与复验方法见 [第 047 步学习记录](docs/开发学习记录.md#第-047-步映射登录成功响应-dto)。

已提供 LoginSuccess HTTP 交付方法，完成 DTO 编码后返回 200、no-store 和 Pragma no-cache。Web 使用 cloudnest_refresh_token Cookie，路径为 /api/v1/auth，设置 HttpOnly、SameSite=Strict，并按调用配置选择 Secure；原生模式通过 JSON 交付刷新原文。属性与真实 HTTP/HTTPS Cookie Jar 验证见 [第 048 步学习记录](docs/开发学习记录.md#第-048-步交付登录成功响应与刷新-cookie)。

已提供 Login HTTP 处理函数，串联 JSON/字段及来源检查、共享限流凭证适配器、资料检查、会话令牌创建和成功交付。错误凭证统一返回 AUTH_REQUIRED 401，资料准备成功后再创建会话。测试入口与真实 bcrypt/JWT 组件验证见 [第 049 步学习记录](docs/开发学习记录.md#第-049-步组合登录-http-处理流程)。

POST /api/v1/auth/login 已接入真实启动流程，支持 Web Cookie 和原生 JSON 刷新凭证交付，登录后的 access_token 可用于 GET /api/v1/me。CLOUDNEST_COOKIE_SECURE 默认 false，HTTPS 部署配置为 true。完整隔离库验证与可复制的登录示例见 [第 050 步学习记录](docs/开发学习记录.md#第-050-步启动装配并注册真实登录接口)。

已提供 FindRefreshSession，按固定 32 字节 SHA256 摘要读取会话标识、刷新代次和授权状态快照。查询保留禁用、撤销及到期事实，供后续刷新服务判断，轮换写入会在事务内复查。字段、参数绑定和真实 MySQL 验证见 [第 051 步学习记录](docs/开发学习记录.md#第-051-步按刷新摘要只读定位会话状态)。

已提供 RefreshCredentialService，严格校验刷新原文的规范无填充 Base64URL 格式，计算文本摘要并读取会话状态，成功返回标识、期限、代次及隐藏于普通 JSON 的摘要。格式错误和摘要未命中统一返回刷新凭证无效，具体流程及真实 MySQL 验证见 [第 052 步学习记录](docs/开发学习记录.md#第-052-步校验刷新原文与会话授权状态)。

已提供 WithRefreshUserTransaction，复用已有用户事务，按用户、设备、会话顺序锁定当前记录，再核对刷新摘要、代次及授权状态。通过后把最新资料与同一事务 Store 交给回调，提交和回滚沿用已有规则。锁等待、旧校验结果失效和真实 MySQL 验证见 [第 053 步学习记录](docs/开发学习记录.md#第-053-步建立刷新会话的用户事务入口)。

已提供事务内 RotateRefreshToken，通过 Store 绑定的锁内会话资料，按旧摘要与旧代次条件写入新摘要、下一代次及更新时间，成功更新一行后返回待提交结果。固定会话期限、摘要冲突、回滚和双请求单次成功的验证见 [第 054 步学习记录](docs/开发学习记录.md#第-054-步在刷新事务内轮换摘要与代次)。

已提供 RefreshSessionService，组合旧原文校验、新凭证生成和刷新用户事务，提交成功后返回会话、新代次及隐藏于普通 JSON 的新原文。错误返回空结果，调用顺序、事务适配及真实 MySQL 并发验证见 [第 055 步学习记录](docs/开发学习记录.md#第-055-步组合刷新会话服务)。

已提供 RefreshTokenService，将刷新会话提交与 Access Token 签发串联，成功返回会话、代次及两类凭证的内部结果。签发失败或提交后取消时返回空结果，已提交轮换继续生效。代码职责、真实 JWT/数据库授权联测和失败边界见 [第 056 步学习记录](docs/开发学习记录.md#第-056-步组合刷新会话与访问令牌签发)。

已提供 NewRefreshData，将刷新结果转换为 data/request_id 响应结构。Web 数据包含 access_token 与 expires_in，原生数据增加 refresh_token；剩余完整秒数按响应计算时刻向下取整。字段、期限边界、真实 JWT 转换和 OpenAPI 共享结构见 [第 057 步学习记录](docs/开发学习记录.md#第-057-步定义刷新成功响应-dto)。

已提供 RefreshSuccess，先转换并编码刷新响应，再按 Web Cookie 或原生 JSON 交付凭证，使用 no-store 与 Pragma no-cache。登录和刷新共用 Cookie 构造方法，HTTP/HTTPS Cookie Jar、新值替换、取消及已提交响应验证见 [第 058 步学习记录](docs/开发学习记录.md#第-058-步交付刷新成功的-http-响应与-cookie)。

已提供 RefreshRequest，校验 client_type 与 refresh_token 的组合。Web 正文字段可省略或为空，后续从 Cookie 取得凭证；原生须提供非空原文，交给凭证服务继续校验。字段错误复用统一 400，读取联测和 OpenAPI 输入分支见 [第 059 步学习记录](docs/开发学习记录.md#第-059-步定义刷新请求-dto-与字段校验)。

已提供 security.RefreshToken，Web 按单个非空刷新 Cookie 提取原文，原生直接取得正文值。同名 Cookie 在解析前计数，提取失败使用固定刷新错误并映射为 AUTH_REQUIRED 401。共用 Cookie 名称、来源链路和凭证校验联测见 [第 060 步学习记录](docs/开发学习记录.md#第-060-步按客户端模式提取刷新凭证)。

已提供 handler.Refresh，组合 JSON/字段校验、来源检查、原文提取、刷新服务与成功交付，并在服务调用前后检查取消。测试路由的 Web/原生交付、真实 MySQL 轮换与 JWT 授权联测见 [第 061 步学习记录](docs/开发学习记录.md#第-061-步组合刷新-http-处理函数)。

POST /api/v1/auth/refresh 已接入生产启动装配，复用已有连接池、JWT 签发器、来源白名单及 Cookie 配置。Web 通过刷新 Cookie 提交原文，原生通过 JSON 提交，每次成功刷新返回访问令牌并轮换刷新凭证。真实启动、重启复用、禁用/撤销与体验示例见 [第 062 步学习记录](docs/开发学习记录.md#第-062-步接入正式刷新接口与启动装配)。

已提供 WithUserTransaction，接收已认证 Actor，按用户、设备、会话顺序锁定并复查归属、账号状态、撤销和会话期限，再执行同一事务回调。提交、回滚及并发等待验证见 [第 063 步学习记录](docs/开发学习记录.md#第-063-步建立已认证用户的事务入口)。

已提供 RevokeCurrentSession，使用已认证事务 Store 绑定的当前身份，按完整归属更新会话撤销时间与更新时间。外层提交成功后撤销生效，访问/刷新失效、回滚和并发等待验证见 [第 064 步学习记录](docs/开发学习记录.md#第-064-步在事务内撤销当前会话)。

已提供 LogoutService，接收已认证 Actor，在用户事务中撤销当前会话，确认提交后返回 Revoked=true。错误、取消、提交边界和真实 MySQL 联测见 [第 065 步学习记录](docs/开发学习记录.md#第-065-步组合退出登录服务)。

已提供 NewLogoutData，将确认提交后的退出结果转换为 revoked=true，LogoutResponse 使用统一 data/request_id 结构。Web 与原生客户端共用该 JSON 契约，转换规则、服务结果衔接及验证见 [第 066 步学习记录](docs/开发学习记录.md#第-066-步定义退出成功响应-dto)。

已提供 LogoutSuccess，先校验并编码退出成功数据，再交付统一 200 JSON 和禁止缓存头。Web 复用原名称与认证路径，以空值、过去期限及 Max-Age=0 清理刷新 Cookie。测试路由、HTTP/HTTPS Cookie Jar、取消边界与调用方式见 [第 067 步学习记录](docs/开发学习记录.md#第-067-步交付退出成功的-http-响应与-cookie-清理)。

已提供 LogoutRequest，正文包含精确取值为 web/native 的 client_type。字段校验复用统一 400，JSON 读取继续使用已有正文上限和类型检查。请求示例、字段规则及测试入口验证见 [第 068 步学习记录](docs/开发学习记录.md#第-068-步定义退出请求-dto-与字段校验)。

已提供 handler.Logout，读取认证 Actor，组合退出输入、来源检查、会话撤销及成功交付。带认证中间件的测试路由已验证真实 MySQL/JWT 退出、凭证失效、其他会话范围及回滚。调用方式、取消边界和验证命令见 [第 069 步学习记录](docs/开发学习记录.md#第-069-步组合退出-http-处理函数)。

POST /api/v1/auth/logout 已接入正式启动流程，使用 Bearer 认证和 client_type 请求正文撤销当前会话。Web 成功响应清理刷新 Cookie，原生客户端根据成功 JSON 清理本地凭证。正式接口、重启后撤销状态和本地登录/退出示例见 [第 070 步学习记录](docs/开发学习记录.md#第-070-步接入正式退出接口与启动装配)。

已提供 ListUserSessions，按用户与会话 ID 上界倒序读取列表，每页 1～100 条，通过额外一条记录判断 HasMore。查询保留会话及设备撤销、到期和可空时间等事实，供后续会话管理服务使用。参数、十列投影、分页和真实数据库验证见 [第 071 步学习记录](docs/开发学习记录.md#第-071-步按用户分页读取会话列表)。

已提供 UserSessionService.List，接收已认证 Actor，校验分页、归属和顺序，并按会话 ID 标记 Current。服务返回独立的列表与可空时间副本，当前会话位于其他页时，本页各项 Current=false。调用关系及真实 MySQL/JWT 验证见 [第 072 步学习记录](docs/开发学习记录.md#第-072-步组合会话列表服务并标记当前会话)。

已提供 NewUserSessionListData，将列表服务结果映射为 data.items/has_more 和 request_id。会话及设备 ID 使用字符串，时间使用 UTC 微秒值，可空时间明确返回 null，空列表使用 []。字段、JSON 示例、服务衔接及边界验证见 [第 073 步学习记录](docs/开发学习记录.md#第-073-步定义会话列表响应-dto)。

已提供 ParseUserSessionListQuery，解析 before_id/limit 为列表服务参数。省略参数时读取首页、每页 50 条，显式 before_id=0 也表示首页，页大小为 1～100；额外查询字段采用忽略策略。标准十进制写法、重复值和整体解析错误的处理见 [第 074 步学习记录](docs/开发学习记录.md#第-074-步定义会话列表查询参数-dto)。

已提供 handler.UserSessions，读取认证 Actor，串联分页解析、列表服务与响应 DTO，交付带请求 ID 的 JSON 和禁止缓存头。测试专用路由已通过真实 MySQL/JWT/HTTP 验证分页、用户隔离、历史状态及认证边界。代码和运行方法见 [第 075 步学习记录](docs/开发学习记录.md#第-075-步组合会话列表-http-处理函数)。

GET /api/v1/sessions 已接入正式启动流程，使用 Bearer 认证和 before_id/limit 查询当前用户的会话。响应包含 items、has_more、当前标记及历史状态，重启、账号隔离和退出后的列表状态已通过真实启动联测。完整登录、查询及后续页示例见 [第 076 步学习记录](docs/开发学习记录.md#第-076-步接入正式会话列表与启动装配)。

第 077 步已提供撤销指定会话的路径参数 DTO，将路由中的 id 转为非零 uint64，校验标准十进制格式与范围，并复用统一字段错误。当前通过测试专用 DELETE 入口验证认证、路径解码与参数传递，实际撤销操作按后续小步接入。代码说明与验证命令见 [第 077 步学习记录](docs/开发学习记录.md#第-077-步撤销指定会话的路径参数-dto)。

已提供 RevokeUserSession，在已认证用户事务中按当前用户归属撤销指定会话。先只读定位归属，再锁定复查目标，重复撤销成功并保留首次时间，外层提交后确认生效。同/异设备、账号隔离、并发与回滚已通过真实 MySQL 验证，服务及正式 HTTP 接入按后续小步实施。方法步骤与复验命令见 [第 078 步学习记录](docs/开发学习记录.md#第-078-步在用户事务中撤销指定会话)。

已提供 RevokeSessionService，组合已认证用户事务与指定会话撤销，提交确认后返回 Revoked=true。事务入口的调用者状态错误保留原分类，目标不存在或属于其他账号时转换为 ErrTargetSessionNotFound。重复调用、并发、回滚和取消边界已通过真实 MySQL/JWT 验证，响应 DTO 与正式 HTTP 接入按后续小步实施。接口职责与验证方法见 [第 079 步学习记录](docs/开发学习记录.md#第-079-步组合指定会话撤销服务)。

已提供 NewRevokeSessionData，将指定会话撤销的已提交结果映射为 revoked=true，RevokeSessionResponse 使用统一 data/request_id 外层。空结果与未确认标记返回既有数据错误，JSON 字段、副本独立性及服务衔接已通过单元验证。数据格式与运行方法见 [第 080 步学习记录](docs/开发学习记录.md#第-080-步定义指定会话撤销成功响应-dto)。

已提供 RevokeSessionSuccess，将已提交撤销结果交付为 HTTP 200 JSON，关联本次请求 ID，附带 no-store/Pragma no-cache。测试专用 DELETE 入口已验证正常交付、异常结果、取消、已提交响应及真实 HTTP 传输，完整处理函数与正式路由按后续小步接入。执行顺序与验证命令见 [第 081 步学习记录](docs/开发学习记录.md#第-081-步交付指定会话撤销成功的-http-响应)。

已提供 handler.RevokeSession，组合认证 Actor、路径 ID 校验、撤销服务与成功交付。有效调用者访问缺失或其他账号目标时返回 SESSION_NOT_FOUND 404，调用者失效沿用认证错误。测试专用路由已通过真实 MySQL/JWT/HTTP 验证归属、重复撤销、回滚及事务状态复查，正式路由和启动接入按下一小步实施。代码与验证方法见 [第 082 步学习记录](docs/开发学习记录.md#第-082-步组合指定会话撤销-http-处理函数)。

DELETE /api/v1/sessions/{id} 已接入正式路由及启动流程，使用共享授权和用户事务撤销自己的指定会话。重复撤销保留首次时间，目标缺失或属于其他账号返回 SESSION_NOT_FOUND 404，调用者失效使用认证错误。实际启动及重启验证已通过，登录两个学习会话并撤销指定目标的示例见 [第 083 步学习记录](docs/开发学习记录.md#第-083-步接入正式指定会话撤销接口与启动装配)。

已提供 ListUserDevices，按用户及设备 ID 上界倒序读取设备列表，每页 1～100 条并返回 HasMore。查询保留设备名称、平台、创建时间、最近认证活动及撤销时间，已撤销设备和 NULL 时间按数据库事实返回。参数、七列投影与独占 MySQL 验证见 [第 084 步学习记录](docs/开发学习记录.md#第-084-步按用户分页读取设备列表)。

已提供 UserDeviceService.List，使用已认证 Actor 的用户归属查询设备，核对分页与 ID 顺序，并按 Actor.DeviceID 标记 Current。返回独立记录及两项可空时间副本，同设备不同会话共享当前设备标记。服务调用、分页与真实 MySQL/JWT 验证见 [第 085 步学习记录](docs/开发学习记录.md#第-085-步组合设备列表服务并标记当前设备)。

已提供 NewUserDeviceListData，将设备服务结果转换为 items/has_more，完整响应采用 data/request_id。设备 ID 使用字符串，时间使用 UTC 微秒值，current=false、NULL 时间及空数组明确交付。转换复用设备摘要与共用时间方法，字段、边界和服务衔接验证见 [第 086 步学习记录](docs/开发学习记录.md#第-086-步定义设备列表响应-dto)。

已提供 ParseUserDeviceListQuery，将 before_id/limit 转为设备服务的 BeforeDeviceID/PageSize，默认首页和每页 50 条，显式 0 表示首页。设备与会话共用分页校验，各自保留固定字段提示；查询格式、数字边界、HTTP 错误及服务衔接验证见 [第 087 步学习记录](docs/开发学习记录.md#第-087-步定义设备列表查询参数-dto)。

已提供 handler.UserDevices，组合认证 Actor、分页参数、设备服务与响应 DTO，交付统一 JSON、请求 ID 和禁止缓存头。测试专用 GET 入口已通过真实 MySQL/JWT/HTTP 验证分页、用户隔离、同设备多会话、历史状态及撤销边界。执行顺序与复验命令见 [第 088 步学习记录](docs/开发学习记录.md#第-088-步组合设备列表-http-处理函数)。

GET /api/v1/devices 已接入正式路由和启动装配，使用共享 Bearer 授权及设备列表服务，支持 before_id/limit 分页和当前设备标记。正式启动、重启、同设备多会话及禁用/撤销边界验证已通过，本地登录和查询示例见 [第 089 步学习记录](docs/开发学习记录.md#第-089-步接入正式设备列表与启动装配)。

已提供 RenameDeviceRequest，使用 name 提交修改后的设备名称，复用登录设备名称的 1～100 个 Unicode 码点、非全空白及无控制字符规则，并保留原文。字段校验、JSON 读取及统一错误的测试入口见 [第 090 步学习记录](docs/开发学习记录.md#第-090-步定义修改设备名称的请求-dto)。

已提供 ParseUserDevicePath，将路由 id 转为非零 uint64，校验标准十进制格式及完整范围。设备与会话路径共用数字检查，各自使用固定提示；测试专用 PATCH 入口已串联认证、目标 ID 与名称正文。参数规则、调用顺序及复验命令见 [第 091 步学习记录](docs/开发学习记录.md#第-091-步定义设备-id-路径参数-dto)。

已提供 Store.RenameUserDevice，在已认证用户事务中检查归属并更新设备名称，外层提交后确认生效。相同名称保留原更新时间，已撤销目标可改展示名称并保留撤销状态。名称规则复用领域层 ValidDeviceName，事务、回滚、并发及独占 MySQL 验证见 [第 092 步学习记录](docs/开发学习记录.md#第-092-步在用户事务中修改指定设备名称)。

已提供 RenameDeviceService.Rename，组合已认证用户事务与设备改名，核对并复制设备结果，确认提交后返回。目标缺失或跨账号转换为 ErrTargetDeviceNotFound，调用者失效保留原分类；提交、取消、副本与真实 MySQL/JWT 验证见 [第 093 步学习记录](docs/开发学习记录.md#第-093-步组合设备名称更新服务)。

已提供 NewRenameDeviceData，将改名服务已提交的设备结果转换为 data.device，完整响应使用 data/request_id。设备展示包含字符串 ID、名称、平台及三项 UTC 微秒时间，可空时间明确返回 null。字段、时间边界、副本与服务衔接验证见 [第 094 步学习记录](docs/开发学习记录.md#第-094-步定义设备改名成功响应-dto)。

已提供 RenameDeviceSuccess，将已确认提交的设备改名结果交付为 HTTP 200 JSON，使用 data.device/request_id、no-store 和 Pragma no-cache。测试专用 PATCH 入口已验证请求 ID、设备字段、异常、取消与真实 HTTP 传输，执行顺序和复验命令见 [第 095 步学习记录](docs/开发学习记录.md#第-095-步交付设备改名成功的-http-响应)。

已提供 handler.RenameDevice，组合认证 Actor、设备路径、名称正文、改名服务及成功交付。缺失或其他账号目标统一返回 DEVICE_NOT_FOUND 404，调用者失效沿用认证错误。测试专用路由已通过真实 MySQL/JWT/HTTP 验证改名、同名重试、归属、回滚及事务复查，执行顺序与复验命令见 [第 096 步学习记录](docs/开发学习记录.md#第-096-步组合设备改名-http-处理函数)。

PATCH /api/v1/devices/{id} 已接入正式路由和启动装配，使用共享 Bearer 授权与用户事务修改设备名称，成功返回 data.device/request_id。同名重试、重启持久化、已撤销设备及重新登录复用已通过真实启动验证，本地登录和改名示例见 [第 097 步学习记录](docs/开发学习记录.md#第-097-步接入正式设备改名接口与启动装配)。

已提供 Store.RevokeUserDevice，在已认证用户事务中撤销指定设备及其尚未撤销的全部会话，外层提交后共同生效。重复调用保留已有撤销时间，支持当前设备、已撤销目标补齐会话及重新登录复用。参数、两表回滚、并发与独占 MySQL/JWT 验证见 [第 098 步学习记录](docs/开发学习记录.md#第-098-步在事务内撤销指定设备及关联会话)。

已提供 RevokeDeviceService.Revoke，组合已认证用户事务与设备/关联会话撤销，确认提交后返回 Revoked=true。目标缺失或跨账号使用 ErrTargetDeviceNotFound，调用者失效保留原分类。提交、取消、两表回滚、并发及真实 MySQL/JWT 验证见 [第 099 步学习记录](docs/开发学习记录.md#第-099-步组合设备撤销服务)。

已提供 NewRevokeDeviceData，将设备撤销服务确认提交后的结果转换为 revoked=true，RevokeDeviceResponse 使用统一 data/request_id。空结果和未确认标记返回设备数据错误，JSON 字段、独立副本及服务提交边界验证见 [第 100 步学习记录](docs/开发学习记录.md#第-100-步定义设备撤销成功响应-dto)。

已提供 RevokeDeviceSuccess，将设备及关联会话确认提交后的结果交付为 HTTP 200 JSON，关联请求 ID 并设置 no-store/Pragma no-cache。测试专用 DELETE 入口已验证成功、异常、取消、已提交响应及真实 HTTP 传输，执行顺序与复验命令见 [第 101 步学习记录](docs/开发学习记录.md#第-101-步交付设备撤销成功的-http-响应)。

已提供 handler.RevokeDevice，组合认证 Actor、设备 ID 路径解析、撤销服务及成功交付，目标缺失或跨账号复用 DEVICE_NOT_FOUND 404。测试专用 DELETE 入口已通过真实 MySQL/JWT/HTTP 验证设备与全部会话撤销、重复请求、两表回滚、调用者状态复查及重新登录复用，代码与复验命令见 [第 102 步学习记录](docs/开发学习记录.md#第-102-步组合设备撤销-http-处理函数)。

DELETE /api/v1/devices/{id} 已接入正式路由和启动装配，使用共享授权与用户事务撤销设备及全部关联会话。真实启动和重启已验证重复撤销、历史时间、访问/刷新失效、当前设备及重新登录复用，本地登录和撤销示例见 [第 103 步学习记录](docs/开发学习记录.md#第-103-步接入正式设备撤销接口与启动装配)。

已进入 B2 文件元数据阶段，提供 FindUserFileEntry，按已认证用户归属读取指定文件或目录的十一项基础字段。真实 MySQL 已验证根目录、普通文件/目录、历史状态、用户隔离、只读快照及完整 uint64，执行顺序与复验命令见 [第 104 步学习记录](docs/开发学习记录.md#第-104-步按用户归属读取指定目录项基础信息)。

已提供 WithFileEntrySnapshot，以显式只读、可重复读事务组合目录项查询，首次普通读取建立快照，后续查询复用同一快照。真实 MySQL 已验证并发移动/回收期间的一致性、只读写入拒绝、错误/取消/panic 后释放及新快照读取，执行顺序与复验命令见 [第 105 步学习记录](docs/开发学习记录.md#第-105-步增加目录项只读快照事务入口)。

已提供 FileEntryPathService.Get，在同一只读快照内复用用户资料与目录项查询，返回真实根目录到目标的可见路径。目标和祖先状态、用户归属、父目录类型、循环与 32 层深度边界已通过单元及真实 MySQL 验证，执行顺序与复验命令见 [第 106 步学习记录](docs/开发学习记录.md#第-106-步检查目录项及祖先链可见性)。

已提供 ListUserDirectoryEntries，按用户、父目录及 ACTIVE 状态分页读取子项，采用修改时间/ID 双降序和复合翻页位置，返回独立基础记录及 HasMore。相同时间翻页、百条页、范围隔离、快照并发和整数/时间边界已通过真实 MySQL 验证，执行顺序与复验命令见 [第 107 步学习记录](docs/开发学习记录.md#第-107-步分页读取当前目录的活动子项)。

已提供 DirectoryListService.List，在同一快照内复用目录路径检查和活动子项分页，返回路径、独立子项及 HasMore。真实 MySQL 已验证目录可见性、文件父项拒绝、分页、并发移动/回收、深度边界与快照结束失败，执行顺序与复验命令见 [第 108 步学习记录](docs/开发学习记录.md#第-108-步组合普通目录列表服务)。

已提供 DirectoryCursorSigner.Sign，使用专用派生密钥和 HMAC-SHA256，将版本、用户、父目录、directory/modified_desc、空筛选摘要及微秒时间/ID 编码为签名游标。固定向量、字段改动、并发及真实目录页衔接已通过验证，执行顺序与复验命令见 [第 109 步学习记录](docs/开发学习记录.md#第-109-步生成带签名的目录分页游标)。

已提供 DirectoryCursorVerifier.Verify，核对规范编码、HMAC、版本及用户/父目录/默认查询绑定，返回独立的 FileEntryPosition，版本 1 输入上限为 1024 字节。异常协议、并发及真实 MySQL 后续页衔接已通过验证，执行顺序与复验命令见 [第 110 步学习记录](docs/开发学习记录.md#第-110-步校验并解码目录分页游标)。

已提供 DirectoryPageService.List，接收客户端 cursor 文本，复用游标校验、目录列表和下一页签名，返回路径、独立子项及可空 NextCursor。首页、后续页、末页、错误短路和完整整数流程已通过真实 MySQL 验证，执行顺序与复验命令见 [第 111 步学习记录](docs/开发学习记录.md#第-111-步组合带游标的目录分页服务)。

已提供 NewDirectoryPageData，将完整目录页映射为 items、breadcrumbs 和可空 next_cursor，DirectoryPageResponse 使用 data/request_id 外层。当前十项基础子项字段使用 entry_type、字符串 ID/版本、JSON 安全字节数和 UTC 微秒时间，转换及真实游标衔接见 [第 112 步学习记录](docs/开发学习记录.md#第-112-步定义目录列表响应-dto)。

已提供 DirectoryPageSuccess，将成功目录页交付为 HTTP 200 JSON，关联请求 ID，设置 no-store/Pragma no-cache，并处理异常数据和取消。测试专用 GET 入口已验证字段、空页/末页、正文头、已提交响应及真实网络传输，执行顺序与复验命令见 [第 113 步学习记录](docs/开发学习记录.md#第-113-步交付目录列表成功的-http-响应)。

已提供 ParseDirectoryPageQuery，解析必填 parent_id、单个原始 cursor 和默认 50 条的 limit，并核对当前 directory/modified_desc/空筛选条件。参数、HTTP 字段错误及分页服务衔接已验证，执行顺序与复验命令见 [第 114 步学习记录](docs/开发学习记录.md#第-114-步定义目录列表查询参数-dto)。

已提供 handler.DirectoryPage，组合认证 Actor、查询 DTO、带游标分页服务与成功交付。无效游标、不可见目录、文件父项和认证错误按契约映射，测试专用 GET 入口已通过真实 MySQL/JWT/HTTP 验证，执行顺序与复验命令见 [第 115 步学习记录](docs/开发学习记录.md#第-115-步组合目录列表-http-处理函数)。

GET /api/v1/entries 已接入正式路由和启动装配，支持指定可见目录的活动子项、面包屑及签名游标分页。当前查询条件为 directory、modified_desc 和空 q/kind；parent_id 从当前用户资料取得，limit 默认 50、上限 100。游标使用持久化主密钥派生的目录专用签名配置，实际启动和重启已验证原游标续页、账号隔离及失效凭证拒绝，体验与复验方法见 [第 116 步学习记录](docs/开发学习记录.md#第-116-步接入正式目录列表接口)。

已提供 EntryPath 和 ParseEntryPath，复用资源 ID 校验，将路由解码后的目录项 id 转换为非零 uint64，错误使用固定提示和 id 字段。完整整数、非法格式、结果独立性及认证先行的 HTTP 测试入口已验证，供后续指定目录项详情流程调用，执行顺序与复验方法见 [第 117 步学习记录](docs/开发学习记录.md#第-117-步定义目录项-id-路径参数-dto)。

已提供 FileEntryDisplay 和 Store.FindUserFileEntryDisplay，按用户归属读取分类、扩展名、MIME、来源设备 ID/名称快照、文件夹颜色和内容发布时间，保留字段原文及 NULL。真实 MySQL 已验证空值、历史记录、完整整数、日期边界、设备改名/撤销后的名称快照，以及基础/展示字段共用只读快照，执行顺序与复验方法见 [第 118 步学习记录](docs/开发学习记录.md#第-118-步按用户归属读取指定目录项扩展展示信息)。

已提供 EntryPreference 和 Store.FindUserEntryPreference，以目录项左连接偏好表，按用户与目标读取收藏时间及最近成功授权访问时间。已有目录项缺少偏好时返回两项空时间，缺失或跨账号目标使用目录项不存在错误。真实 MySQL 已验证默认值、时间组合、只读行为、隔离与同快照更新/新增偏好，执行顺序与复验方法见 [第 119 步学习记录](docs/开发学习记录.md#第-119-步按用户归属读取指定目录项偏好)。

已提供 EntryDetailService.Get，在同一次只读快照中复用路径可见性检查，再读取扩展展示与偏好，返回 EntryDetail。路径末项包含目标基础信息，可空字段返回独立副本，异常或取消返回空结果。单元和真实 MySQL 已验证文件/目录/根目录、账号隔离、隐藏路径短路、并发移动/回收/展示/偏好更新下的一致快照及完整整数边界，执行顺序与复验方法见 [第 120 步学习记录](docs/开发学习记录.md#第-120-步组合指定目录项详情服务)。

已提供 NewEntryDetailData 和 EntryDetailResponse，将内部详情映射为十八项公开字段及 data/request_id 外层，文件类别生成 kind，收藏时间生成 starred。根目录 parent_id 为 null，来源设备历史未知字段保留 null，标识/版本使用字符串，时间复用 UTC 微秒转换。单元及真实 MySQL 已验证完整字段、来源空值、类别、整数/日期边界与独立副本，执行顺序与复验方法见 [第 121 步学习记录](docs/开发学习记录.md#第-121-步定义目录项详情响应-dto)。

已提供 handler.EntryDetailSuccess，将成功详情转换为 DTO、附加请求 ID、预先编码并交付 200 UTF-8 JSON，设置 no-store/Pragma no-cache，异常数据及取消沿用统一内部错误。测试专用 GET 入口已验证完整字段、根目录、历史空值、请求关联、正文头和实际网络传输，执行顺序与复验方法见 [第 122 步学习记录](docs/开发学习记录.md#第-122-步交付目录项详情成功的-http-响应)。

已提供 handler.EntryDetail，组合认证 Actor、路径 DTO、详情服务及成功交付。路径错误定位 id，不可见目标统一映射 ENTRY_NOT_FOUND，账号/会话错误沿用认证规则，服务结果再次核对请求目标及用户。测试专用 GET 入口已通过真实 MySQL/JWT/HTTP 联测，执行顺序与复验方法见 [第 123 步学习记录](docs/开发学习记录.md#第-123-步组合目录项详情-http-处理函数)。

GET /api/v1/entries/{id} 已接入正式路由和生产启动装配，读取当前用户可见文件、目录及根目录的完整详情。路由绑定共享授权与真实只读快照服务，交付类别、来源名称快照和收藏/访问偏好等十八项公开字段。实际启动及重启验证已覆盖字段、账号隔离、生命周期、失效凭证和目录列表共存，体验与复验方法见 [第 124 步学习记录](docs/开发学习记录.md#第-124-步接入正式目录项详情接口)。

已提供 service.ValidateEntryName，校验普通文件/文件夹名称并返回去除首尾空白的展示名，覆盖 UTF-8、控制/保留字符、120 个 UTF-16 代码单元、设备保留名和末尾句点。跨客户端共享样例位于 api/testdata/entry_name_cases.json，后端测试实际读取并验证这些样例，规则及复验方法见 [第 125 步学习记录](docs/开发学习记录.md#第-125-步实现目录项名称校验)。

已提供 service.BuildEntryNameKey，复用名称校验并按 NFC → Unicode full case folding → NFC 生成独立 UTF-8 字节键，上限为 768 字节。大小写、规范等价文本、长串组合附加符、字节膨胀和并发检查通过，四十项跨客户端共享样例包含展示名与名称键，执行顺序与复验方法见 [第 126 步学习记录](docs/开发学习记录.md#第-126-步生成目录项名称判重键)。

已提供 Store.FindUserActiveFileEntryByName，按已认证用户、真实父目录和二进制活动名称键读取十一项基础字段，缺失沿用 ErrFileEntryNotFound。真实 MySQL 已验证 Unicode/大小写等价名称、用户及目录隔离、历史同名记录、唯一索引、并发改名/移动快照及完整整数边界，执行顺序与复验方法见 [第 127 步学习记录](docs/开发学习记录.md#第-127-步按用户和父目录查询活动同名目录项)。

已提供 Store.CreateUserDirectory，在已认证用户事务中检查父目录归属和自身状态，插入普通文件夹并通过活动名称唯一键读回数据库默认字段，重名返回 ErrEntryNameConflict。真实 MySQL 已验证默认值、空颜色、回滚、历史重名复用、失效身份、同名并发和高位无符号自增 ID，执行顺序与复验方法见 [第 128 步学习记录](docs/开发学习记录.md#第-128-步在已认证用户事务内创建文件夹记录)。

已提供 CreateDirectoryRequest、CreateDirectoryInput 和 ParseCreateDirectoryRequest，将 JSON 字符串父目录 ID 转为非零 uint64，检查名称基础文本和可空颜色键，并返回独立输入。测试入口已验证 JSON 绑定、字段错误、认证先行和统一名称服务衔接，执行顺序与复验方法见 [第 129 步学习记录](docs/开发学习记录.md#第-129-步定义创建文件夹请求-dto)。

已提供 CreateDirectoryService.Create，复用名称规则，在同一已认证用户事务中检查父目录及祖先可见性、目录类型和创建后的 32 层深度，再写入并确认提交后返回独立记录。颜色规则由 DTO 和服务共用 Domain 实现，真实 MySQL 已验证路径、层级、回滚、同名并发及高位自增，执行顺序与复验方法见 [第 130 步学习记录](docs/开发学习记录.md#第-130-步组合创建文件夹核心服务)。

已提供 dto.ParseIdempotencyKey，读取单个 Idempotency-Key 请求头，去除首尾 HTTP 空白后按 1～128 字节可见 ASCII 规则校验，保留键的大小写，错误定位同名请求头。定向验证覆盖重复/合并值、边界、解析顺序、CORS 和真实 HTTP 传输，执行顺序与复验方法见 [第 131 步学习记录](docs/开发学习记录.md#第-131-步解析业务写请求的幂等键)。

已提供 service.BuildCreateDirectoryRequestHash，复用创建规则校验参数，按固定顺序编码版本化操作、字符串父目录 ID、展示名与可空颜色，返回 32 字节 SHA-256 摘要。固定向量、参数差异、完整整数、并发和 HTTP 绑定验证通过，编码及复验方法见 [第 132 步学习记录](docs/开发学习记录.md#第-132-步生成创建文件夹请求摘要)。

已提供 Store.FindUserIdempotencyRecord，按用户、版本化操作及幂等键读取十一项事实，保留摘要、资源、响应 JSON 和到期时间，缺失返回专用领域错误。真实 MySQL 已验证范围及大小写、SQL NULL/JSON null、历史记录、索引、事务视图及完整整数，执行顺序与复验方法见 [第 133 步学习记录](docs/开发学习记录.md#第-133-步按用户范围读取指定幂等记录)。

已提供 Store.CreateUserIdempotencyRecord，在已认证用户事务中保存九项请求/结果字段，归属取自绑定身份，重复范围返回记录已存在错误，插入后读回实际标识与时间。真实 MySQL 已验证文件夹与幂等结果同事务提交/回滚、JSON 空值、占用范围、当前身份复查、并发和高位整数，执行顺序与复验方法见 [第 134 步学习记录](docs/开发学习记录.md#第-134-步在已认证用户事务内保存幂等结果)。

## 本地连接信息

| 组件 | 本机地址 | 账号 | 密码 |
|---|---|---|---|
| MySQL 8.4.13 | 127.0.0.1:13309，数据库 cloudnest | root | root |
| Redis 7.4.12 | 127.0.0.1:16380，数据库 0 | root（ACL 用户） | root |
| MinIO S3 API | http://127.0.0.1:19000，私有桶 cloudnest | root | rootroot |
| MinIO 控制台 | [打开控制台](http://127.0.0.1:19001) | root | rootroot |
| Go API | [检查 /ping](http://127.0.0.1:18081/ping)、[存活检查](http://127.0.0.1:18081/health/live)、[就绪检查](http://127.0.0.1:18081/health/ready) | — | — |

按本项目约定，后续本地开发组件的账号、密码默认使用 root。MinIO 密码存在至少 8 位的硬性限制，因此使用 rootroot；此例外同时适用于控制台和 S3 secret key。

所有依赖端口只绑定 127.0.0.1。后端默认使用 18081，避开用户保留给其他项目的 8080–8090；18080 已被另一已停止容器预留，也不使用。

## 常用命令

~~~powershell
# 检查端口和子网；识别 CloudNest 自己已经运行的容器
python scripts/check_infra_ports.py

# 验证认证和真实 SQL、Redis、S3 分片读写，并创建私有 cloudnest 桶
python scripts/verify_infra.py

# 查看或停止本项目依赖，保留数据卷
docker compose -f scripts/docker-compose.yml ps
docker compose -f scripts/docker-compose.yml stop

# Go 代码检查
go test ./...
go vet ./...
~~~

私有桶由 verify_infra.py 在不存在时创建；验证临时对象会自动清理。三个组件分别使用 cloudnest_mysql_data、cloudnest_redis_data、cloudnest_minio_data 命名卷，独立网络为 cloudnest-dev（10.253.81.0/24）。

本地配置来自 scripts/.env，示例为 scripts/.env.example。修改已初始化数据库的密码需要同步修改数据库账号，不能只修改环境变量。

## 项目文档

- [教学开发约定](AGENTS.md)
- [开发学习记录](docs/开发学习记录.md)
- [当前 HTTP 接口契约](api/openapi.yaml)

- [技术方案书](<云盘 SaaS 项目技术方案书（混合架构版）.md>)
- [数据库设计方案书（已批准）](docs/数据库设计方案书.md)
- [后端设计方案书（已批准）](docs/后端设计方案书.md)
- [环境检查、版本与验证记录](docs/development_environment.md)
- [Bug 修复记录](bugfix_log.md)
