# CloudNest

学习导向的自用文件云盘，采用 Go 后端、React Web、Wails 桌面端和 Flutter 安卓端。

当前已完成本机开发环境、基础容器及首期八张业务表初始化。后端提供 /ping、/health/live、/health/ready、受保护的 POST /api/v1/uploads、PUT /api/v1/uploads/{id}/content、GET /api/v1/uploads/{id}、GET /api/v1/entries、GET /api/v1/entries/{id}、PATCH /api/v1/entries/{id}、POST /api/v1/entries/{id}/move、PUT /api/v1/entries/{id}/star、POST /api/v1/entries/{id}/access、POST /api/v1/directories、GET /api/v1/me、GET /api/v1/sessions、GET /api/v1/devices、PATCH /api/v1/devices/{id}、DELETE /api/v1/devices/{id} 和 DELETE /api/v1/sessions/{id}，以及 POST /api/v1/auth/login、POST /api/v1/auth/refresh 和 POST /api/v1/auth/logout，支持配置及 CORS 白名单放行/预检、JSON 日志、依赖客户端、退出信号处理、请求 ID 和统一错误响应。启动时检查上传临时目录、加载 JWT 配置并检查数据库连接、迁移记录及已有存储桶访问，文件业务按技术方案分阶段实现。

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

已提供 CreatedDirectorySummary、CreateDirectoryData、CreateDirectoryResponse 和 NewCreateDirectoryData，创建成功数据使用 data.entry，复用十项基础字段并附加可空颜色，外层关联当前 request_id。真实 MySQL 已验证稳定数据同事务保存、JSON 读回、后续目录变化下的创建快照和转换失败回滚，字段及复验方法见 [第 135 步学习记录](docs/开发学习记录.md#第-135-步定义创建文件夹成功响应-dto)。

已提供 dto.RestoreCreateDirectoryData，从已保存 JSON 恢复创建快照，逐项检查十一项必需字段、精确字段名、类型、空值、字符串标识和新建目录规则，时间沿用 UTC 微秒转换，异常返回幂等记录数据错误。真实 MySQL 已验证 JSON 读回、颜色空值、历史创建快照和异常结果拒绝，执行顺序与复验方法见 [第 136 步学习记录](docs/开发学习记录.md#第-136-步恢复并校验已保存的创建文件夹响应)。

已提供 handler.CreateDirectorySuccess，接收已确认提交的稳定 JSON，复用恢复校验后关联当前 request_id，预先编码并按 201 交付 data.entry，沿用 no-store、Pragma no-cache、取消检查和统一内部错误。测试 POST 入口、真实 HTTP 传输及同快照关联不同请求 ID 验证通过，执行顺序与复验方法见 [第 137 步学习记录](docs/开发学习记录.md#第-137-步交付创建文件夹成功-http-响应)。

已提供 service.ValidateCreateDirectoryReplay，按已认证用户、版本化操作和幂等键核对记录范围，检查保留时间、请求摘要、ENTRY/201 元数据及正文资源标识。同键参数差异和到期分别返回专用领域错误，截止时刻按已到期处理，完整快照继续由既有恢复方法核对。真实 MySQL 已验证单连接事务内复用判断、参数冲突、期限边界和资源标识不一致，执行顺序与复验方法见 [第 138 步学习记录](docs/开发学习记录.md#第-138-步校验创建文件夹幂等结果复用条件)。

已提供 IdempotentCreateDirectoryService.Create，在同一已认证用户事务内先查询幂等记录，缺失时复用核心创建并保存八天稳定结果，已有记录按范围、期限、摘要和完整快照校验后复用。编码与校验通过函数绑定既有 DTO，提交确认后返回独立 JSON。真实 MySQL 已验证单连接、历史快照、双表回滚、当前身份复查、同键并发与高位 ID，执行顺序与复验方法见 [第 139 步学习记录](docs/开发学习记录.md#第-139-步组合创建文件夹幂等服务)。

已提供 handler.CreateDirectory，按认证 Actor、幂等头、受限 JSON、字段解析、幂等服务和结果核对的顺序处理创建请求，成功复用 201 交付，名称/幂等冲突及到期使用固定 409，参数和不可见父项分别使用既有 400/404 规则。测试 HTTP 入口及真实 JWT/MySQL/TCP 已验证创建、重试、账号范围、身份复查、回滚和高位 ID，执行顺序与复验方法见 [第 140 步学习记录](docs/开发学习记录.md#第-140-步实现创建文件夹-http-处理函数)。

POST /api/v1/directories 已接入正式路由与生产启动装配，复用共享授权、已认证用户事务、幂等创建服务和 DTO 编解码。首次创建及有效期内同键同参数重试返回 201 创建快照，结果保留八天，交付关联当前 request_id。真实启动已验证创建、列表/详情衔接、参数/名称冲突、账号范围及重启后复用，调用与复验方法见 [第 141 步学习记录](docs/开发学习记录.md#第-141-步接入正式创建文件夹接口与启动装配)。

已提供 RenameEntryRequest、RenameEntryInput 和 ParseRenameEntryRequest，按 name、expected_revision 顺序检查基础名称文本及非零 uint64 标准十进制版本，保留名称原文供统一服务规则处理。测试 PATCH 入口已验证认证、路径/幂等头、JSON 绑定、字段错误、完整整数和命名服务衔接，字段及复验方法见 [第 142 步学习记录](docs/开发学习记录.md#第-142-步定义目录项重命名请求-dto)。

已提供 service.ValidateEntryRevision、ErrEntryRevisionConflict 和 EntryRevisionConflictError，相同非零版本通过，不同版本返回附带当前版本的固定冲突错误，零当前版本沿用目录项数据错误。测试 HTTP 入口已验证重命名 DTO、版本比对、包装错误识别和字符串 current_revision 交付，规则及复验方法见 [第 143 步学习记录](docs/开发学习记录.md#第-143-步实现目录项预期版本校验)。

已提供 EntryNameMetadata 和 service.BuildEntryNameMetadata，复用统一名称校验，按最后一个非首位英文句点提取小写后缀，沿用 Demo 的三十二项扩展名分类清单。文件夹字段为空，无后缀及超三十二字符后缀采用空提示和 OTHER。真实 MySQL 已验证可空字段、Unicode 字符容量及详情 kind 映射，规则及复验方法见 [第 144 步学习记录](docs/开发学习记录.md#第-144-步实现目录项扩展名与文件分类计算)。

已提供 Store.RenameUserFileEntry，在已认证用户事务内按归属定位并锁定当前记录，核对状态和预期版本，按条件更新名称、名称键、扩展名、分类、版本及修改时间，写后读回基础事实。同名且版本匹配返回当前记录，根目录和版本上限返回专用错误。真实 MySQL 已验证派生字段、当前锁定读、回滚、身份复查、并发和完整整数，执行顺序与复验方法见 [第 145 步学习记录](docs/开发学习记录.md#第-145-步在已认证用户事务内按版本重命名目录项)。

已提供 RenameEntryService.Rename，在同一已认证用户事务内检查目标及完整祖先路径、根目录限制和预期版本，复用名称元数据与名称键计算，调用条件写入并核对基础结果，确认提交后交付独立副本。真实 MySQL 已验证单连接、三类隐藏状态、回滚、身份复查、并发和最大深度，调用关系及复验方法见 [第 146 步学习记录](docs/开发学习记录.md#第-146-步组合目录项重命名核心服务)。

已提供 service.BuildRenameEntryRequestHash，将版本化操作名、目标 ID、预期版本及校验后的展示名按固定顺序编码并生成 32 字节 SHA-256 摘要。完整整数以十进制字符串参与编码，首尾空白统一处理，展示名大小写和 Unicode 写法参与请求比较。固定向量、HTTP 参数衔接及并发验证通过，编码约定和复验方法见 [第 147 步学习记录](docs/开发学习记录.md#第-147-步生成目录项重命名请求摘要)。

已提供 RenameEntryData、RenameEntryResponse 和 NewRenameEntryData，复用现有详情 DTO 的十八项字段，生成 data.entry 稳定快照，响应外层关联当前 request_id。真实 MySQL 已验证文件/目录、六类文件、空值、同事务保存、后续变更后的快照及失败回滚，结构与复验方法见 [第 148 步学习记录](docs/开发学习记录.md#第-148-步定义目录项重命名成功响应-dto)。

已提供 dto.RestoreRenameEntryData，逐项核对已保存重命名快照的十八项字段、来源设备结构、完整标识、版本、类别、大小、收藏一致性与时间，返回独立 DTO，异常统一为幂等记录数据错误。真实 MySQL 已验证稳定快照恢复、后续改名及八类合法 JSON 异常，规则与复验方法见 [第 149 步学习记录](docs/开发学习记录.md#第-149-步恢复并校验已保存的重命名响应快照)。

已提供 handler.RenameEntrySuccess，恢复并校验已确认的稳定快照，附加当前请求 ID 后预先编码，复查取消并交付 200 JSON、no-store 和 Pragma no-cache。测试 PATCH 入口已验证完整快照、异常、已提交响应、跨域及真实 HTTP 传输，执行顺序与复验方法见 [第 150 步学习记录](docs/开发学习记录.md#第-150-步实现目录项重命名成功-http-交付)。

已提供 service.ValidateRenameEntryReplay，核对当前用户、重命名操作、幂等键、记录期限及请求摘要，再确认 ENTRY 资源、200 状态和正文目标标识。到期与参数冲突复用既有领域错误，校验后连接完整快照恢复。真实 MySQL 已验证原请求复用、目标/版本/名称变化和到期边界，判断顺序与复验方法见 [第 151 步学习记录](docs/开发学习记录.md#第-151-步校验重命名幂等结果复用条件)。

已提供 IdempotentRenameEntryService，在同一已认证用户事务中查询幂等记录，已有记录经复用及完整快照校验后返回，首次请求则连接核心改名、完整详情读取、DTO 编码及八天结果保存。真实 MySQL 已验证单连接、同名、后续变更、五类回滚、身份失效、三类并发及完整无符号整数，调用与复验方法见 [第 152 步学习记录](docs/开发学习记录.md#第-152-步组合目录项重命名幂等服务)。

已提供 handler.RenameEntry，连接已认证身份、目标路径、幂等键、重命名正文、幂等服务及 200 交付。错误按既有契约映射，版本冲突携带完整当前版本字符串，根目录改名返回 403，成功快照核对目标、名称和合法版本关系。测试 PATCH 入口已通过真实 JWT/MySQL 联测，执行顺序与复验方法见 [第 153 步学习记录](docs/开发学习记录.md#第-153-步实现目录项重命名-http-处理函数)。

PATCH /api/v1/entries/{id} 已接入正式路由与启动装配，绑定共享授权、已认证用户事务、幂等重命名服务及 DTO 编解码。真实 run 启动已验证文件/目录、同名、名称/版本/幂等冲突、列表/详情衔接，以及服务重启后的原始快照复用。正式参数与响应见 [OpenAPI](api/openapi.yaml)，执行顺序与复验方法见 [第 154 步学习记录](docs/开发学习记录.md#第-154-步接入正式目录项重命名接口与启动装配)。

已提供 MoveEntryRequest、MoveEntryInput 和 ParseMoveEntryRequest，按 target_parent_id、expected_revision 顺序解析非零 uint64 标准十进制字符串，完整保留高位整数。测试 POST 入口已验证认证、源路径、幂等头、JSON 绑定和字段错误，参数边界及复验方法见 [第 155 步学习记录](docs/开发学习记录.md#第-155-步定义目录项移动请求-dto)。

已提供 EntryMoveTargetService.Check，复用同一读取范围内的源路径与目标父目录路径，检查根目录、目标类型、共同根目录及自身/子树关系，返回独立 EntryMovePaths。真实 MySQL 已验证单连接、隐藏链、账号范围、快照一致性、取消和完整标识，规则与复验方法见 [第 156 步学习记录](docs/开发学习记录.md#第-156-步校验移动源与目标父目录关系)。

已提供 Store.FindUserEntrySubtreeStats，使用单条递归查询统计源自身及各生命周期保留后代的数量与最大相对深度，源自身深度为零。查询复用当前连接或事务、绑定用户范围并检查超深/循环及异常父项，真实 MySQL 已验证快照、10000/10001 节点和完整标识，规则与复验方法见 [第 157 步学习记录](docs/开发学习记录.md#第-157-步读取源子树数量与最大相对深度)。

已提供 ValidateEntryMoveLimits，将已校验路径与同源子树统计绑定，核对 10000 项规模及目标父目录深度加一再加子树相对深度的最终层级。真实 MySQL 已验证单连接组合、10000/10001 项和最终 32/33 层边界，规则与复验方法见 [第 158 步学习记录](docs/开发学习记录.md#第-158-步校验移动规模与最终深度)。

已提供 Store.MoveUserFileEntry，在已认证用户事务内按范围定位源和目的地，按主键升序锁定后核对状态、类型与预期版本，实际移动条件更新父目录、版本和两个修改时间，同位置匹配返回当前事实。真实 MySQL 已验证单连接、名称/版本冲突、回滚、当前读和并发，调用前置条件及复验方法见 [第 159 步学习记录](docs/开发学习记录.md#第-159-步在已认证用户事务内按版本移动目录项)。

已提供 MoveEntryService.Move，在同一已认证用户事务中串起移动关系、版本、子树统计、规模/最终深度及条件写入，确认提交后返回独立基础记录。同位置仍校验完整规则，真实 MySQL 已验证文件/目录、实际 10000 项及 32 层边界、回滚、身份复查和并发，调用顺序与复验方法见 [第 160 步学习记录](docs/开发学习记录.md#第-160-步组合目录项移动核心服务)。

已提供 service.BuildMoveEntryRequestHash，将移动操作名、源 ID、预期版本和目标父目录 ID 按固定顺序编码为紧凑 JSON，生成 32 字节 SHA-256 摘要。整数采用完整十进制字符串，六组固定向量、参数差异、整数边界及 HTTP 参数衔接验证通过，编码约定与复验方法见 [第 161 步学习记录](docs/开发学习记录.md#第-161-步生成目录项移动请求摘要)。

已提供 MoveEntryData、MoveEntryResponse 和 NewMoveEntryData，将同一事务内移动后读取的完整详情映射为 entry 稳定快照，HTTP 外层关联当前请求 ID。复用十八项详情字段及可空值、完整标识和 UTC 微秒时间规则，真实 MySQL 已验证单连接保存、后续移动后的历史快照和回滚，转换与复验方法见 [第 162 步学习记录](docs/开发学习记录.md#第-162-步定义目录项移动成功响应-dto)。

已提供 RestoreMoveEntryData，从保存的 JSON 恢复并校验完整移动快照，核对十八项字段、标识、类别、来源设备、收藏关系及时间，异常返回统一幂等数据错误。移动和重命名恢复共用内部校验方法，真实 MySQL 已验证历史快照及十二类异常正文，规则与复验方法见 [第 163 步学习记录](docs/开发学习记录.md#第-163-步恢复并校验已保存的移动响应快照)。

已提供 handler.MoveEntrySuccess，恢复并校验已确认的移动快照，关联当前请求 ID，预先编码后交付 HTTP 200 与 UTF-8 JSON、no-store 和 Pragma no-cache。测试 POST 入口已验证完整详情、取消及异常、响应状态和真实传输，调用约定与复验方法见 [第 164 步学习记录](docs/开发学习记录.md#第-164-步实现目录项移动成功-http-交付)。

已提供 ValidateMoveEntryReplay，按用户、移动操作和幂等键核对记录范围，再检查期限、请求摘要及 ENTRY/200 结果，正文中的源和父目录须与本次请求一致。真实 MySQL 已验证原请求复用、参数冲突、期限边界及错误目的地，完整快照继续由恢复入口校验，规则与复验方法见 [第 165 步学习记录](docs/开发学习记录.md#第-165-步校验移动幂等结果复用条件)。

已提供 IdempotentMoveEntryService.Move，在同一已认证用户事务中先查幂等记录，缺失时组合核心移动、完整详情转换和八天稳定结果保存，已有记录通过复用及快照校验后交付。确认提交后返回独立 JSON，真实 MySQL 已验证单连接、历史结果、双表回滚、身份复查及三类并发，调用过程与复验方法见 [第 166 步学习记录](docs/开发学习记录.md#第-166-步组合目录项移动幂等服务)。

已提供 handler.MoveEntry，按认证身份、源路径、幂等头、移动正文、服务调用及结果核对的顺序处理请求，成功复用 MoveEntrySuccess，领域错误映射统一 400/403/404/409 响应。真实 JWT/MySQL/HTTP 测试入口已验证移动与重试、目录关系、范围及认证后状态复查，参数与错误约定见 [第 167 步学习记录](docs/开发学习记录.md#第-167-步实现目录项移动-http-处理函数)。

已接入正式 POST /api/v1/entries/{id}/move，启动入口复用共享授权、用户事务、移动幂等服务和 DTO 编解码。真实 run 两轮启动已验证文件/目录移动、同位置、冲突、范围、列表/详情及重启后的原始快照复用，接口契约见 [OpenAPI](api/openapi.yaml)，调用与复验方法见 [第 168 步学习记录](docs/开发学习记录.md#第-168-步接入正式目录项移动接口与启动装配)。

已提供 SetStarredRequest、SetStarredInput 和 ParseSetStarredRequest，用指针布尔值读取明确的收藏状态，true 表示收藏、false 表示取消收藏，缺失或 null 返回 starred 字段错误。独立结果、并发及三十五组真实 HTTP 输入验证通过，类型与调用过程见 [第 169 步学习记录](docs/开发学习记录.md#第-169-步定义目录项收藏请求-dto)。

已提供 Store.SetUserEntryStarred，在已认证用户事务中按归属定位并锁定活动目录项，设置收藏偏好并当前读回结果。首次收藏使用数据库 UTC 时间，重复收藏沿用原收藏时间，取消收藏返回空收藏时间，最近访问按原值返回。真实 MySQL 已验证单连接、文件/目录/根目录、旧快照、回滚、并发及完整整数，调用前置条件与复验方法见 [第 170 步学习记录](docs/开发学习记录.md#第-170-步在已认证用户事务内设置目录项收藏状态)。

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

已提供 SetStarredService，在同一已认证用户事务中复用完整路径检查及收藏写入，核对偏好归属、状态与时间，复制结果并确认提交后交付。文件、目录及根目录沿用既有收藏规则，隐藏祖先拒绝写入，异常结果回滚真实偏好及审计时间。服务专项及扩展后的正式 MySQL 联测连续三次通过，调用与复验方法见 [第 171 步学习记录](docs/开发学习记录.md#第-171-步组合目录项收藏核心服务)。

已提供 SetStarredData、SetStarredResponse 和 NewSetStarredData，将已提交收藏偏好转换为 starred、starred_at 及 data/request_id 外层。收藏时间复用 UTC 微秒转换，取消收藏明确返回 false 与 null。精确 JSON、日期边界、副本及真实 MySQL 组合验证通过，字段与复验方法见 [第 172 步学习记录](docs/开发学习记录.md#第-172-步定义目录项收藏成功响应-dto)。

已提供 handler.SetStarredSuccess，将已提交收藏偏好转换为公开响应，关联当前请求 ID，以 HTTP 200、UTF-8 JSON 和禁止缓存头交付。取消收藏明确返回 false/null，异常资料或取消沿用统一内部错误，已发送状态按原响应结束。测试专用 PUT 入口及三组真实 HTTP 传输连续三次通过，执行顺序与复验方法见 [第 173 步学习记录](docs/开发学习记录.md#第-173-步实现目录项收藏成功-http-交付)。

已提供 handler.SetStarred，按认证身份、路径、JSON 正文及明确的收藏状态校验，调用收藏核心服务，核对偏好归属与状态并交付成功响应。不可见目标使用统一 404，认证变化和内部故障沿用既有错误映射。测试入口、真实 JWT 与 MySQL 的十四次 PUT 请求验证通过，调用与复验方法见 [第 174 步学习记录](docs/开发学习记录.md#第-174-步实现目录项收藏-http-处理函数)。

PUT /api/v1/entries/{id}/star 已接入正式路由和生产启动装配，复用共享授权及用户事务设置收藏。文件、目录和根目录支持明确 true/false，重复收藏取原时间，取消收藏返回 false/null，详情同步读取偏好。正式启动及重启验证连续三次通过，规则与复验方法见 [第 175 步学习记录](docs/开发学习记录.md#第-175-步接入正式目录项收藏接口与启动装配)。

已提供 RecordAccessRequest、RecordAccessInput 和 ParseRecordAccessRequest，明确接受 preview/open 两种访问用途，缺失、null 或其他文本定位 purpose 字段错误。解析后取得独立字符串参数，认证、路径及正文顺序通过三十九组真实 POST 输入验证，字段与复验方法见 [第 176 步学习记录](docs/开发学习记录.md#第-176-步定义目录项访问记录请求-dto)。

已提供 Store.RecordUserEntryAccess，在已认证用户事务内记录活动文件最近访问时间，首次填充数据库 UTC 时间，满六十秒更新，窗口内复用原值并保留收藏事实。精确微秒边界、回滚、身份复查、旧快照与并发经过真实 MySQL 验证，调用前置条件与复验方法见 [第 177 步学习记录](docs/开发学习记录.md#第-177-步在已认证用户事务内记录目录项最近访问)。

已提供 RecordAccessService，在同一已认证用户事务中校验用途、完整路径和文件类型，调用最近访问写入，核对偏好并确认提交后交付独立结果。用途规则由 Domain 供 DTO/服务共用，服务专项和扩展后的真实 MySQL 联测连续三次通过，调用与复验方法见 [第 178 步学习记录](docs/开发学习记录.md#第-178-步组合目录项访问记录核心服务)。

已提供 RecordAccessData、RecordAccessResponse 和 NewRecordAccessData，将已提交访问偏好映射为必填 last_opened_at 及 data/request_id 外层。时间复用 UTC 微秒转换，缺失或异常访问时间使用统一目录项数据错误。精确 JSON、日期、副本和真实 MySQL 组合验证通过，字段与复验方法见 [第 179 步学习记录](docs/开发学习记录.md#第-179-步定义目录项访问记录成功响应-dto)。

已提供 handler.RecordAccessSuccess，将已提交访问偏好转换为公开响应，关联当前请求 ID，以 HTTP 200、UTF-8 JSON 和禁止缓存头交付。异常偏好或取消沿用统一内部错误，测试专用 POST 入口及四组真实 HTTP 传输连续三次通过，执行顺序与复验方法见 [第 180 步学习记录](docs/开发学习记录.md#第-180-步实现目录项访问记录成功-http-交付)。

已提供 handler.RecordAccess，按 Actor、路径、JSON 正文及用途校验，调用访问核心服务并核对偏好后交付。目录目标定位 id，用途错误定位 purpose，不可见目标使用统一 404。测试入口及真实 JWT/MySQL 的十四次 POST 请求验证通过，调用与复验方法见 [第 181 步学习记录](docs/开发学习记录.md#第-181-步实现目录项访问记录-http-处理函数)。

POST /api/v1/entries/{id}/access 已接入正式路由与启动装配，按 preview/open 记录当前用户可见文件的访问时间。用户事务复查账号、设备、会话和完整路径，六十秒内返回原访问时间，成功响应包含 last_opened_at 与当前 request_id。正式启动与重启联测连续三次通过，详情、原收藏、目录项版本、归属及隐藏路径经过对照。调用关系与复验方法见 [第 182 步学习记录](docs/开发学习记录.md#第-182-步接入正式目录项访问记录路由与启动装配)。

已提供 GlobalEntryQuery 与 IsValid，统一表达 all、starred、recent 的内部查询条件，检查范围与排序组合、UTF-8 搜索文本、小写分类及 1～100 的页大小。recent 使用 last_opened_desc 表达访问时间降序，单次全局请求候选扫描预算为 1000。组合、边界、输入保护与并发校验连续三次通过，模型及后续衔接见 [第 183 步学习记录](docs/开发学习记录.md#第-183-步定义全局目录项列表查询模型)。

已提供 Store.ListUserGlobalEntryCandidates，接收已认证用户 ID、GlobalEntryQuery 和可空 FileEntryPosition，读取 all/modified_desc、空筛选的活动普通目录项候选。复用十一列投影及分页结果，按修改时间与 ID 降序翻页，多取一条判断 HasMore，候选完整祖先链可见性由后续服务组合。正式 MySQL 跨目录、隔离、快照与整数/日期边界联测连续三次通过，调用与复验方法见 [第 184 步学习记录](docs/开发学习记录.md#第-184-步按用户读取全局活动目录项候选)。

全局候选读取已支持 kind 分类筛选，空值查询全部类型，folder 绑定 DIR，document/image/video/audio/archive/other 绑定 FILE 及对应大写 category。筛选条件在排序和分页前进入 SQL，复用已有用户范围、位置和 HasMore，返回记录核对文件/目录类型。分类、续页、隔离及分类变更快照经过正式 MySQL 三次验证，调用与复验方法见 [第 185 步学习记录](docs/开发学习记录.md#第-185-步为全局目录项候选增加分类筛选)。

全局候选读取已支持 Search 名称关键词，按名称包含匹配，并与 kind 条件共同在数据库排序和分页前执行。搜索原文保留，使用绑定参数及显式 LIKE ESCAPE 处理百分号、下划线和转义符，匹配沿用 name 字段的排序规则。特殊字符、分类组合、续页、名称变更快照及 NO_BACKSLASH_ESCAPES 模式经过正式 MySQL 三次验证，调用与复验方法见 [第 186 步学习记录](docs/开发学习记录.md#第-186-步为全局目录项候选增加名称搜索)。

已提供 Store.FindUserFileEntries，按认证用户及一批目录项 ID 读取十一列基础事实，返回以 ID 为键的映射。单批输入最多 1000 个 ID，重复值合并，空批次返回空映射，缺失或跨账号 ID 在结果中缺省，根目录及生命周期值保留供上层服务判断。正式表批次、隔离、独立结果、事务连接、并发与快照联测连续三次通过，调用与复验方法见 [第 187 步学习记录](docs/开发学习记录.md#第-187-步按用户批量读取目录项基础记录)。

已提供 service.FilterVisibleFileEntries，在调用方的同一只读快照内逐层批量补齐祖先，再通过缓存复用 FileEntryPathService 检查完整路径。隐藏路径候选被过滤，可见项保持原顺序，异常父链与读取故障交付错误。共享祖先、千项候选、最大深度、独立结果及正式 MySQL 的单连接/父链变更快照验证通过，执行顺序和复验方法见 [第 188 步学习记录](docs/开发学习记录.md#第-188-步组合批量祖先可见性检查)。

已提供 GlobalEntryListService，在一次只读快照内组合候选扫描与完整路径可见性，每批最多一百项、单次最多检查一千个候选。页满保留当前消费位置，隐藏过滤后的短页或空页可携带续页位置，候选耗尽时结束翻页。补读、预算、批次余项、分类/搜索、账号、单连接及并发快照经过正式 MySQL 验证，调用与复验方法见 [第 189 步学习记录](docs/开发学习记录.md#第-189-步组合全局可见列表分页)。

已提供 GlobalEntryCursorSigner，使用全局游标用途的派生 HMAC 密钥，绑定用户、all/modified_desc、搜索/分类摘要和最后消费位置，生成两段无填充 Base64URL 游标。ID 使用完整十进制字符串，时间固定为 UTC 微秒，有效页大小调整沿用同一绑定。独立固定向量、密钥副本、并发及真实预算空页签名续读验证通过，格式与复验方法见 [第 190 步学习记录](docs/开发学习记录.md#第-190-步实现全局分页游标签名)。

已提供 GlobalEntryCursorVerifier，复用全局签名器的密钥派生及规范编码，校验 HMAC、版本、当前用户、范围/排序、筛选摘要及位置，返回独立 UTC 微秒位置。异常游标统一使用全局游标错误，合法页大小调整可继续使用原游标。协议异常、篡改、用途、并发及真实预算空页的签名/校验/续读闭环验证通过，规则与复验方法见 [第 191 步学习记录](docs/开发学习记录.md#第-191-步实现全局游标校验与位置还原)。

已提供 GlobalEntryPageService，统一首页空游标、非空游标校验、已确认全局列表读取与下一页签名，返回可见项及可空文本游标。预算空页保留续页文本，结束时 NextCursor 为 nil，续页位置核对向后推进。参数、异常、取消、副本及正式 MySQL 文本游标闭环验证通过，装配与复验方法见 [第 192 步学习记录](docs/开发学习记录.md#第-192-步组合带游标的全局分页服务)。

已提供 GlobalEntryPageData、GlobalEntryPageResponse 和 NewGlobalEntryPageData，复用十字段条目摘要及 UTC 微秒转换。全局 breadcrumbs 使用 []，预算空页保留 next_cursor，结束时输出 null，ID/版本采用完整字符串、大小采用安全 JSON 整数。目录与全局摘要共用转换方法，原目录回归及真实全局分页 JSON 验证通过，格式与复验方法见 [第 193 步学习记录](docs/开发学习记录.md#第-193-步定义全局分页响应-dto)。

已提供 handler.GlobalEntryPageSuccess，将全局分页结果转换为 DTO，关联当前 request_id 并以 HTTP 200、UTF-8 JSON、no-store 和 Pragma no-cache 交付。预算空页保留续页文本，取消及异常结果沿用统一内部错误，响应状态边界、实际 GET 传输及真实数据库结果交付验证通过，执行顺序与复验方法见 [第 194 步学习记录](docs/开发学习记录.md#第-194-步实现全局分页成功-http-交付)。

已提供 GlobalEntryPageQuery 与 ParseGlobalEntryPageQuery，完整解析全局列表 RawQuery，默认 all/modified_desc 和每页五十条，保留有效 UTF-8 搜索原文并复用分类规则及页大小校验。重复值、格式和能力边界按固定字段错误返回，游标原文进入分页服务完成签名及筛选绑定校验。参数、HTTP 测试入口与正式 MySQL 三页闭环验证通过，执行顺序和复验方法见 [第 195 步学习记录](docs/开发学习记录.md#第-195-步定义全局列表查询参数-dto)。

已提供 handler.GlobalEntryPage，组合认证 Actor、全局查询 DTO、文本分页服务及成功交付，检查请求取消、结果数量及当前用户归属。无效全局游标返回 INVALID_CURSOR 400 并定位 cursor，账号/会话状态复用已有认证响应，内部故障使用统一 500。处理函数专项及真实 JWT/MySQL/HTTP 二十四次 GET 闭环连续三次通过，调用与复验方法见 [第 196 步学习记录](docs/开发学习记录.md#第-196-步实现全局列表-http-处理函数)。

GET /api/v1/entries 已支持正式全局查询，单个 scope=all 使用当前用户的全局可见项，支持名称搜索、kind 分类及修改时间降序游标分页，breadcrumbs 为 []。省略 scope 时采用 directory，parent_id 必填；全局扫描预算耗尽时空页保留 next_cursor。生产启动已装配同请求只读快照和独立用途游标签名，正式启动/重启及原游标续读连续三次通过，示例及复验方法见 [第 197 步学习记录](docs/开发学习记录.md#第-197-步接入正式全局列表路由与启动装配)。

全局候选 Repository 已支持 starred/modified_desc，按当前用户与 entry_id 检查非空 starred_at，再组合名称搜索、kind 分类、排序和分页。收藏候选复用十一列基础记录，完整父链可见性由上层组合检查。正式 MySQL 的收藏状态、跨目录分页、百条页、偏好变更快照及整数/日期边界连续三次通过，说明与复验方法见 [第 198 步学习记录](docs/开发学习记录.md#第-198-步为全局目录项候选增加收藏范围筛选)。

GlobalEntryListService.List 已支持 starred/modified_desc，在同一只读快照中读取收藏候选、检查完整父链并补读可见项，每批最多一百项、单次最多一千个候选。预算耗尽时短页或空页携带最后消费位置。收藏/原全局专项与真实 MySQL 分页、未收藏父目录、收藏变化快照联测连续三次通过，说明与复验方法见 [第 199 步学习记录](docs/开发学习记录.md#第-199-步让全局可见列表服务支持收藏范围)。

全局游标签名器及校验器已支持 starred/modified_desc，沿用版本 1 七字段载荷，签名绑定当前用户、查询范围、排序、搜索/分类摘要和最后消费位置。两类范围的固定向量、协议异常、密钥/结果副本、并发及真实普通/预算空页签名续读验证连续三次通过，格式和复验方法见 [第 200 步学习记录](docs/开发学习记录.md#第-200-步让全局签名游标支持收藏范围)。

GlobalEntryPageService.List 已支持 starred/modified_desc，按空游标首页、非空游标校验、可见收藏列表读取和下一页签名返回 Entries/NextCursor。预算空页保留文本游标，结束页 NextCursor 为 nil。两类范围服务专项及真实 MySQL 普通/预算分页、筛选绑定和公开 DTO 空值验证连续三次通过，调用与复验方法见 [第 201 步学习记录](docs/开发学习记录.md#第-201-步让文本游标分页服务支持收藏范围)。

ParseGlobalEntryPageQuery 已接收单个 scope=all/starred，并保留实际范围，默认 all/modified_desc、每页五十条。搜索、分类及游标原文继续传给分页服务，重复/空/不支持范围使用固定 scope 字段提示。参数、测试 HTTP 入口及真实 MySQL 文本分页闭环连续三次通过，说明和复验方法见 [第 202 步学习记录](docs/开发学习记录.md#第-202-步让全局列表查询参数支持收藏范围)。

GET /api/v1/entries?scope=starred 已接入正式路由，按当前用户的收藏事实、搜索和分类读取完整父链可见项，面包屑为 []，预算空页保留 next_cursor。正式 PUT 收藏/取消后可通过列表核对，原普通/预算游标在服务重启后继续有效。真实启动/重启及每轮新增二十七次 GET、八次 PUT 连续三次通过，示例和复验方法见 [第 203 步学习记录](docs/开发学习记录.md#第-203-步接入正式收藏列表路由)。

已提供 RecentEntryPosition，以 LastOpenedAt 和完整无符号目录项 ID 表达最近使用列表的内部续页位置。IsValid 核对非零 ID、UTC 年份 1000～9999 及微秒精度，带时区输入按同一时刻检查并保留原值。二十组正常/边界专项及全局查询模型连续三次通过，模型和复验方法见 [第 204 步学习记录](docs/开发学习记录.md#第-204-步定义最近使用列表的分页位置模型)。

已提供 Store.ListUserRecentEntryCandidates，接收认证用户、recent/last_opened_desc 的空筛选查询及可空 RecentEntryPosition。关联当前用户偏好后读取最近访问过的活动普通文件，以访问时间/ID 降序多取一条，返回基础记录、LastOpenedAt 与 HasMore，完整祖先链由上层在同快照内检查。专项及正式 MySQL 的排序分页、百条页、隔离、快照和整数/日期边界连续三次通过，调用和复验方法见 [第 205 步学习记录](docs/开发学习记录.md#第-205-步按用户读取最近访问文件候选)。

最近访问候选已支持合法 kind 分类筛选，六类文件使用绑定的大写 category，空分类查询全部最近文件，folder 与文件范围求交后返回空候选页。分类条件在访问时间/ID 排序和 LIMIT 前进入 SQL，复用原访问偏好、用户范围及翻页位置。分类、续页、百条页和分类变更快照经过真实 MySQL 连续三次验证，调用与复验方法见 [第 206 步学习记录](docs/开发学习记录.md#第-206-步为最近访问候选增加分类筛选)。

最近访问候选已支持 Search 名称包含查询，保留 UTF-8 搜索原文并与 kind、用户访问范围及续页位置组合。%、_ 和显式转义符先转义后绑定，匹配沿用 name 字段的 MySQL LIKE 及排序规则。专项和正式表的特殊字符、搜索/分类续页、用户隔离、名称变更快照与 NO_BACKSLASH_ESCAPES 验证连续三次通过，匹配细节与复验方法见 [第 207 步学习记录](docs/开发学习记录.md#第-207-步为最近访问候选增加名称搜索)。

已提供 RecentEntryListService，在同一只读快照内分批读取最近候选、复用完整父链可见性并保留访问事实。每批最多一百项、单次最多检查一千项，页满记录最后消费位置，预算短页或空页可以继续访问时间/ID 翻页。服务专项及正式 MySQL 的过滤补读、搜索/分类、账号、父目录/访问变化快照和预算边界连续三次通过，调用与复验方法见 [第 208 步学习记录](docs/开发学习记录.md#第-208-步组合最近使用可见列表服务)。

已提供 RecentEntryCursorSigner，为 recent/last_opened_desc 查询生成版本 1 签名游标。采用最近使用专用派生密钥，绑定用户、范围、排序、搜索/分类摘要及 UTC 微秒访问时间和完整 ID，合法页大小调整可复用同一文本。固定向量、边界、原文绑定、用途隔离、副本/并发及正式 MySQL 的普通/预算位置签名续读连续三次通过，格式和复验方法见 [第 209 步学习记录](docs/开发学习记录.md#第-209-步实现最近使用分页游标签名)。

已提供 RecentEntryCursorVerifier，先核对最近使用专用 HMAC，再检查版本、当前用户、范围、排序、筛选及规范编码，成功还原独立的 UTC 微秒访问位置。格式/签名/绑定异常统一返回最近使用游标错误，合法页大小调整沿用原游标。协议专项、固定向量、用途隔离、副本/并发及真实 MySQL 普通/预算游标校验续读连续三次通过，顺序与复验方法见 [第 210 步学习记录](docs/开发学习记录.md#第-210-步校验最近使用游标并还原访问位置)。

已提供 RecentEntryPageService，组合空游标首页、非空游标校验、最近可见列表读取及下一页签名，返回独立文件/访问事实与可空 NextCursor。预算空页或短页保留文本游标，结束页使用 nil，异常结果和取消整体返回错误。服务专项及正式 MySQL 的首页/续页/结束、合法页大小调整、错误绑定拒绝和预算文本分页连续三次通过，调用与复验方法见 [第 211 步学习记录](docs/开发学习记录.md#第-211-步组合带文本游标的最近使用分页服务)。

已提供 RecentEntrySummary、RecentEntryPageData、RecentEntryPageResponse 和 NewRecentEntryPageData。复用十字段摘要并增加必填 last_opened_at，ID/版本使用字符串，访问时间转换为 UTC 微秒值，空 items/breadcrumbs 为 []，结束游标为 null。精确 JSON、边界、副本及真实 MySQL 文本分页转换连续三次通过，字段与复验方法见 [第 212 步学习记录](docs/开发学习记录.md#第-212-步定义最近使用分页响应-dto)。

已提供 handler.RecentEntryPageSuccess，将最近使用分页结果转换为公开 DTO，关联当前 request_id，以 HTTP 200、UTF-8 JSON、no-store 和 Pragma no-cache 交付。文件保留必填 last_opened_at，预算空页保留续页文本，结束为 null。十六组成功、十五组异常、两组已提交状态及五组真实 GET 专项连续三次通过，正式 MySQL 普通/预算/结束页的真实 HTTP 交付通过。执行顺序与复验方法见 [第 213 步学习记录](docs/开发学习记录.md#第-213-步实现最近使用分页成功-http-交付)。

已提供 RecentEntryPageQuery 与 ParseRecentEntryPageQuery，完整解析最近使用列表 RawQuery，默认 recent/last_opened_desc 和每页五十条。搜索原文保留，分类及页大小复用既有规则，游标检查单值与解码后的字节长度，再交由分页服务校验签名和绑定。参数/HTTP 专项及真实 MySQL 普通、预算、结束分页连续三次通过，执行顺序与复验方法见 [第 214 步学习记录](docs/开发学习记录.md#第-214-步定义最近使用列表查询参数-dto)。

已提供 handler.RecentEntryPage，组合认证 Actor、最近查询 DTO、文本分页服务和成功交付。参数错误定位字段，无效最近游标返回 INVALID_CURSOR 400，账号/会话状态复用既有响应，异常结果检查数量及当前用户归属。五十四组流程及真实 JWT/MySQL/HTTP 每轮二十四次 GET 连续三次通过，调用与复验方法见 [第 215 步学习记录](docs/开发学习记录.md#第-215-步实现最近使用列表-http-处理函数)。

GET /api/v1/entries 已支持 scope=recent，按当前用户的 last_opened_at 与 ID 降序查询可见活动文件，支持名称搜索、kind 分类和文本游标分页，返回必填 UTC 微秒访问时间。启动入口装配只读快照及最近使用独立用途游标，正式访问写入、预算空页、用户/用途隔离与真实启动重启续页连续三次通过。执行顺序与复验方法见 [第 216 步学习记录](docs/开发学习记录.md#第-216-步接入正式最近使用列表路由与启动装配)。

已提供 NameEntryPosition，以数据库 name_key 原始字节和完整目录项 ID 表达名称升序分页位置，IsValid 检查非零 ID 及 1～768 字节容量。位置可以保存最后扫描的隐藏候选，名称键生成继续复用 BuildEntryNameKey。边界、借用字节保护、生成键衔接及既有名称键正式表回归连续三次通过，模型与复验方法见 [第 217 步学习记录](docs/开发学习记录.md#第-217-步定义全局名称升序分页位置模型)。

已提供 Store.ListUserNameEntryCandidates，按认证用户读取 all/starred、name_asc 的活动普通目录项候选，保留独立 name_key 字节并按名称键/ID 双升序续页。复用搜索及分类规则，多取一条判断 HasMore，完整祖先可见性由后续服务组合。专项及正式 MySQL 的同键分页、二进制边界、收藏/筛选、百条页与改名快照连续三次通过，调用与复验方法见 [第 218 步学习记录](docs/开发学习记录.md#第-218-步按用户读取名称升序的全局目录项候选)。

已提供 NameEntryListService，在同一只读快照中组合 all/starred、name_asc 名称候选与完整祖先链可见性，每批最多一百项、一次最多扫描一千项。保留原始名称键，页满保存最后消费位置，预算空页或短页可以继续翻页，结束时 NextPosition 为 nil。专项及正式 MySQL 的同键分页、过滤补读、快照、单连接与预算边界连续三次通过，调用和复验方法见 [第 219 步学习记录](docs/开发学习记录.md#第-219-步组合名称升序可见列表服务)。

已提供 NameEntryCursorSigner，为 all/starred、name_asc 查询生成版本 1 签名游标。采用名称用途派生 HMAC 密钥，绑定用户、范围、排序、原文筛选摘要及名称键/ID，二进制键使用可逆 Base64URL，名称游标上限为 2048 字节。独立固定向量、最长载荷、绑定/用途、副本/并发及正式 MySQL 签名位置续读连续三次通过，协议与复验方法见 [第 220 步学习记录](docs/开发学习记录.md#第-220-步实现名称升序分页游标签名)。

已提供 NameEntryCursorVerifier，先核对名称用途 HMAC，再检查版本、当前用户、all/starred、name_asc、筛选及规范编码，返回独立的原始名称键/ID 位置。支持 2048 字节游标边界，合法页大小调整可继续使用原文本，异常文本及绑定统一返回名称游标错误。固定向量、二进制键、协议异常、副本/并发及正式 MySQL 校验续读连续三次通过，顺序与复验方法见 [第 221 步学习记录](docs/开发学习记录.md#第-221-步实现名称游标校验与位置还原)。

已提供 NameEntryPageService，组合 all/starred、name_asc 的首页、游标校验、可见列表读取及下一页签名，输出独立目录项与可空文本游标。名称键/ID 严格升序，预算空页及短页保留最后消费位置，名称文本容量沿用 2048 字节。服务组合与真实 MySQL 普通/收藏、长二进制键、页大小调整及预算续读连续三次通过，调用与复验方法见 [第 222 步学习记录](docs/开发学习记录.md#第-222-步组合带文本游标的名称升序分页服务)。

已提供 NameEntryPageData、NameEntryPageResponse 和 NewNameEntryPageData，将名称文本分页结果转换为十字段条目摘要、空 breadcrumbs 及可空 next_cursor。复用完整 ID/版本字符串、JSON 安全字节数和 UTC 微秒时间，名称游标按最多 2048 字节检查。精确 JSON、边界、错误及副本专项和真实 MySQL 普通/预算/长游标响应转换连续三次通过，字段与复验方法见 [第 223 步学习记录](docs/开发学习记录.md#第-223-步定义名称升序分页响应-dto)。

已提供 handler.NameEntryPageSuccess，接收已确认的名称分页结果，转换 DTO、关联当前 request_id 并完整编码 JSON，以 HTTP 200、UTF-8 JSON、no-store 和 Pragma no-cache 交付。异常及取消复用统一错误，交付过程核对响应提交状态。专项与真实 MySQL 普通/收藏/预算/长游标结果的实际 GET 交付连续三次通过，调用与复验方法见 [第 224 步学习记录](docs/开发学习记录.md#第-224-步实现名称升序分页成功-http-交付)。

已提供 NameEntryPageQuery 与 ParseNameEntryPageQuery，完整解析名称列表 URL，默认 all/name_asc、每页五十条，接收 all/starred、搜索、分类及最多 2048 字节的解码游标原文。复用领域分类和标准页大小校验，重复值或非法参数返回固定字段提示。专项、HTTP 输入及真实 MySQL 查询到交付闭环连续三次通过，规则与复验方法见 [第 225 步学习记录](docs/开发学习记录.md#第-225-步定义名称升序列表查询参数-dto)。

已提供 handler.NameEntryPage，组合可信 Actor、名称查询 DTO、分页服务和成功/错误交付。格式错误使用固定 INVALID_ARGUMENT，名称游标及绑定错误使用 INVALID_CURSOR，账号/会话/设备错误复用统一授权响应。处理函数专项及真实认证、普通/收藏/长游标、预算空页/短页和状态联测连续三次通过，调用与复验方法见 [第 226 步学习记录](docs/开发学习记录.md#第-226-步实现名称升序列表-http-处理函数)。

GET /api/v1/entries 已接入 all/starred 的 name_asc 名称排序，例如 scope=all&sort=name_asc 或 scope=starred&sort=name_asc。正式启动装配同快照名称列表及持久化主密钥派生的名称游标，支持搜索、分类、2048 字节游标和预算空页续读。路由及真实启动/重启联测连续三次通过，普通、收藏、预算和长键原游标可跨重启使用，示例与复验方法见 [第 227 步学习记录](docs/开发学习记录.md#第-227-步接入正式名称升序列表路由与启动装配)。

已提供 SizeEntryPosition，以完整无符号 FileSize 和目录项 ID 表达大小/ID 双降序的内部续页位置。IsValid 检查非零 ID，大小接受零字节至 uint64 最大值，可保存最后扫描候选。四十五组边界、四组基础记录/副本及既有大小读取正式表回归连续三次通过，模型与复验方法见 [第 228 步学习记录](docs/开发学习记录.md#第-228-步定义全局大小降序分页位置模型)。

已提供 Store.ListUserSizeEntryCandidates，读取 all/starred、size_desc 的活动普通目录项候选，组合搜索、分类及大小/ID 双降序续页条件，多取一条判断 HasMore，返回复用的 FileEntryPage。候选及额外项核对归属、结构、严格顺序和重复 ID，结果父目录独立复制。专项与真实 MySQL 的同大小/零字节、收藏/筛选、快照、百条页及完整无符号边界连续三次通过，调用与复验方法见 [第 229 步学习记录](docs/开发学习记录.md#第-229-步按用户读取大小降序的全局目录项候选)。

已提供 SizeEntryListService，在同一只读快照中组合 all/starred、size_desc 候选与完整祖先链可见性，每批最多一百项、一次最多扫描一千项。返回独立可见记录，页满和预算空页/短页保留最后消费的大小/ID 位置，候选耗尽时结束。专项与真实 MySQL 的同大小/零字节、筛选/账号、单连接、快照及预算边界连续三次通过，调用与复验方法见 [第 230 步学习记录](docs/开发学习记录.md#第-230-步组合大小降序可见列表服务)。

已提供 SizeEntryCursorSigner，为 all/starred、size_desc 查询生成版本 1 签名游标。采用大小用途派生 HMAC，绑定用户、范围、排序、原文搜索/分类摘要及完整十进制大小/ID，零字节有效，合法页大小调整复用原文本。独立固定向量、355 字节最长载荷、绑定/用途、副本/并发及正式 MySQL 普通/预算位置签名续读连续三次通过，协议与复验方法见 [第 231 步学习记录](docs/开发学习记录.md#第-231-步实现大小降序分页游标签名)。

已提供 SizeEntryCursorVerifier，先核对大小用途 HMAC，再检查版本、当前用户、all/starred、size_desc、筛选及规范编码，返回独立完整大小/ID 位置。零字节和完整 uint64 可还原，合法页大小调整复用原文本，异常统一返回大小游标错误。协议边界、固定向量、副本/并发及真实 MySQL 普通/预算校验续读连续三次通过，顺序与复验方法见 [第 232 步学习记录](docs/开发学习记录.md#第-232-步实现大小游标校验与位置还原)。

已提供 SizeEntryPageService，组合 all/starred、size_desc 的首页、游标校验、可见列表和下一页签名，返回独立目录项及可空 NextCursor。大小/ID 严格降序，预算空页和短页保留最后消费位置，结束时游标为 nil。服务专项与真实 MySQL 普通/收藏、合法页大小调整、零字节、预算及完整无符号文本分页连续三次通过，调用与复验方法见 [第 233 步学习记录](docs/开发学习记录.md#第-233-步组合带文本游标的大小降序分页服务)。

已提供 SizeEntryPageData、SizeEntryPageResponse 和 NewSizeEntryPageData，将大小文本分页结果转换为十字段条目摘要、空 breadcrumbs 及可空 next_cursor。复用完整 ID/版本字符串、JSON 安全字节数和 UTC 微秒时间，大小游标按最多 1024 字节检查。精确 JSON、边界、错误与副本专项及真实 MySQL 普通/收藏/预算分页转换连续三次通过，字段与复验方法见 [第 234 步学习记录](docs/开发学习记录.md#第-234-步定义大小降序分页响应-dto)。

已提供 handler.SizeEntryPageSuccess，将已确认的大小分页结果转换为 DTO、关联当前 request_id 并预先编码 JSON，以 HTTP 200、UTF-8 JSON、no-store 和 Pragma no-cache 交付。预算空页保留 next_cursor，结束为 null，异常及取消复用统一错误。专项与真实 MySQL 普通/收藏/预算结果的测试 GET 交付连续三次通过，调用与复验方法见 [第 235 步学习记录](docs/开发学习记录.md#第-235-步实现大小降序分页成功-http-交付)。

已提供 SizeEntryPageQuery 与 ParseSizeEntryPageQuery，完整解析大小列表 RawQuery，默认 all/size_desc、每页五十条，接收 all/starred、原文搜索、分类和最多 1024 字节的解码游标。复用领域分类及标准页大小校验，重复或非法参数返回固定字段提示。查询专项、HTTP 输入及真实 MySQL 分页到交付组合连续三次通过，规则与复验方法见 [第 236 步学习记录](docs/开发学习记录.md#第-236-步定义大小降序列表查询参数-dto)。

已提供 handler.SizeEntryPage，组合可信 Actor、大小查询 DTO、分页服务及成功/错误交付。参数错误定位字段，大小游标及绑定错误返回 INVALID_CURSOR，账号/会话/设备状态复用统一授权响应，交付前核对页大小和用户归属。处理函数专项及真实 JWT/MySQL/HTTP 普通/收藏/同大小/零字节、预算续页和状态联测连续三次通过，调用与复验方法见 [第 237 步学习记录](docs/开发学习记录.md#第-237-步实现大小降序列表-http-处理函数)。

GET /api/v1/entries 已接入 all/starred 的 size_desc 大小降序查询，例如 scope=all&sort=size_desc 或 scope=starred&sort=size_desc。正式启动通过 RegisterSizeEntryPageRoute 装配五类分页能力，大小候选及祖先读取共享快照，持久化主密钥派生大小用途游标。正式路由及真实启动/重启连续三次通过，普通/收藏/预算/同大小的原游标可跨重启续读，调用与复验方法见 [第 238 步学习记录](docs/开发学习记录.md#第-238-步接入正式大小降序列表路由与启动装配)。

已提供 domain.CreateUploadInput，保存目标目录、原始名称、声明大小及可空的固定 32 字节预期 SHA-256。基础校验接收完整非零目录 ID、非空 UTF-8 名称和 0～16 MiB 声明，完整命名继续复用 ValidateEntryName。十八组声明、四十组大小/目录边界、六组哈希原文保护及九组命名衔接连续三次通过，字段与复验方法见 [第 239 步学习记录](docs/开发学习记录.md#第-239-步定义单文件上传任务的创建输入模型)。

已提供 CreateUploadRequest 和 ParseCreateUploadRequest，按 parent_id、name、size_bytes、sha256 顺序检查创建声明并转换为 domain.CreateUploadInput。必填大小通过指针区分缺失/null 与零字节，SHA-256 接收 64 位大小写十六进制或 null，转换为独立 32 字节数组，声明大小原值由后续服务核对能力上限。DTO、JSON/HTTP 输入、来源副本及并发回归连续三次通过，字段与复验方法见 [第 240 步学习记录](docs/开发学习记录.md#第-240-步定义单文件上传创建请求-dto)。

已提供 service.ValidateCreateUploadInput，按目标目录 ID、完整名称、声明大小顺序校验上传声明，复用 ValidateEntryName 取得展示名，支持零字节及 16 MiB 边界，超限返回 ErrUploadFileTooLarge。成功结果复用 copyOptionalValue 独立复制预期摘要，来源原值与其他结果各自保留。业务校验、JSON/DTO 衔接、副本/并发及既有命名回归连续三次通过，规则与复验方法见 [第 241 步学习记录](docs/开发学习记录.md#第-241-步实现上传创建声明的业务校验)。

已提供 domain.UploadStatus，以精确字符串常量表达上传任务十种状态，IsValid 检查合法值，IsTerminal 识别 COMPLETED、CANCELLED、EXPIRED、FAILED 四种终态。正常值、非法编码/写法及正式迁移状态/终态约束对照连续三次通过，模型与复验方法见 [第 242 步学习记录](docs/开发学习记录.md#第-242-步定义上传任务状态模型)。

已提供 domain.UploadTask，完整保存上传任务的二十八项内部事实，包含可信归属、暂存/发布对象、声明、状态、预占、租约、重试及时间。BIGINT UNSIGNED 使用 uint64，两个 INT UNSIGNED 使用 uint32，可空列使用指针，状态复用 UploadStatus。正式迁移字段/空值对照及三组 JSON/DTO/业务声明衔接连续三次通过，字段与复验方法见 [第 243 步学习记录](docs/开发学习记录.md#第-243-步定义上传任务基础记录模型)。

已提供 models.UploadTask，通过 TableName 和显式 GORM 列标签映射 upload_tasks 二十八列，沿用只读查询投影。预期摘要用可空字节切片承接，状态保留数据库文本，为后续领域校验及副本转换提供原始值。GORM 映射解析及十二组 SQL 测试连接器扫描连续三次通过，职责与复验方法见 [第 244 步学习记录](docs/开发学习记录.md#第-244-步定义上传任务数据库映射)。

已提供 Store.FindUserUploadTask，按可信用户和任务 ID 绑定查询二十八列，核对记录范围、上传方式、状态、预占、发布结果及 UTC 微秒时间，返回独立的领域任务副本。缺失或跨账号目标使用 ErrUploadTaskNotFound，异常记录使用 ErrInvalidUploadTaskData。专项及真实 MySQL 的十状态、空值、到期/终态、跨账号、写锁期间读取与完整无符号范围连续三次通过，调用与复验方法见 [第 245 步学习记录](docs/开发学习记录.md#第-245-步按用户归属读取指定上传任务)。

已提供 UploadTaskService.Get，接收可信 Actor 和任务 ID，绑定 Actor.UserID 读取任务，核对交付范围及状态，复用 copyOptionalValue 复制十三项可空字段。同账号其他设备可查询任务，来源设备、到期及终态事实保留。服务专项及真实 MySQL 组合联测连续三次通过，调用与复验方法见 [第 246 步学习记录](docs/开发学习记录.md#第-246-步组合上传任务读取服务)。

已提供 UploadTaskData、UploadTaskResponse 及 NewUploadTaskData，交付十三个公开任务字段，ID 使用十进制字符串，大小遵守 JSON 安全整数，时间转换为 UTC 微秒，可空字段明确为 null。last_error 按已批准业务码生成固定提示，未知码统一为 INTERNAL_ERROR。DTO、OpenAPI 及真实 MySQL→Service→DTO→JSON 联测连续三次通过，字段与复验方法见 [第 247 步学习记录](docs/开发学习记录.md#第-247-步定义上传任务查询响应-dto)。

已提供 handler.UploadTaskSuccess，将任务服务结果转换为公开 DTO，关联当前 request_id，以 HTTP 200、UTF-8 JSON、no-store 和 Pragma no-cache 交付。DTO 异常及取消复用统一错误响应，已提交响应通过 Written 检查结束处理。交付专项及真实 MySQL/Service/HTTP 每轮十三次 GET 连续三次通过，顺序与复验方法见 [第 248 步学习记录](docs/开发学习记录.md#第-248-步实现上传任务查询成功-http-交付)。

已提供 UploadTaskPath 与 ParseUploadTaskPath，复用 parsePathID 将路由已解码的 id 转为完整非零 uint64，格式错误返回固定 id 字段提示。合法/非法路径、独立结果与错误对象、认证先行、查询参数及编码边界连续三次通过，OpenAPI 增加 UploadTaskID 参数，规则与复验方法见 [第 249 步学习记录](docs/开发学习记录.md#第-249-步定义上传任务查询路径参数-dto)。

已提供 handler.UploadTask，组合认证上下文、上传路径 DTO、任务服务和成功交付。路径错误定位 id，缺失及跨账号目标使用 UPLOAD_NOT_FOUND 404，授权错误复用已有响应，异常结果与取消使用统一错误交付。专项及真实 JWT/MySQL/TCP 每轮二十八次 GET 连续三次通过，调用与复验方法见 [第 250 步学习记录](docs/开发学习记录.md#第-250-步实现上传任务查询-http-处理函数)。

GET /api/v1/uploads/{id} 已在启动入口接入正式认证与任务服务。RegisterUploadTaskRoute 组合既有 RequireAuthorization 与 UploadTask，启动时绑定 Store.FindUserUploadTask 和 UploadTaskService.Get。正式路由、十状态、路径/账号边界、跨设备、重启数据及授权状态联测连续三次通过，使用与复验方法见 [第 251 步学习记录](docs/开发学习记录.md#第-251-步接入正式上传任务查询路由与启动装配)。

已提供 domain.StoredObjectStatus，以精确字符串常量表达物理对象六种状态，IsValid 对齐正式迁移的状态约束。六个合法值、三十九个非法值及默认 STAGING 对照连续三次通过，状态含义与复验方法见 [第 252 步学习记录](docs/开发学习记录.md#第-252-步定义存储对象状态模型)。

已提供 domain.StoredObject，保存物理对象十七项内部事实，包含可信归属、不可变存储位置、大小、验证结果、去重生成列读取值、状态及回收时间。字段/空值/生成列对照及三组上传声明到暂存对象和任务的组装验证连续三次通过，模型与复验方法见 [第 253 步学习记录](docs/开发学习记录.md#第-253-步定义存储对象基础记录模型)。

已提供 models.StoredObject，通过 TableName 及显式 GORM 列标签只读映射 stored_objects 十七列。两个摘要字段以可空字节切片承接，状态保留数据库原文，DedupHash 接收生成列读取值。映射解析、十四组 SQL 测试连接器扫描及既有任务映射连续三次通过，职责与复验方法见 [第 254 步学习记录](docs/开发学习记录.md#第-254-步定义存储对象数据库映射)。

已提供 Store.FindUserStoredObject，按可信用户及对象 ID 读取十七列，检查归属、ASCII 位置、状态、摘要、去重生成值和回收时间，返回独立领域副本。缺失及跨账号目标使用 ErrStoredObjectNotFound。专项及真实 MySQL 六状态、生成列、零字节/空值、跨用户、写锁快照和完整无符号范围连续三次通过，调用与复验方法见 [第 255 步学习记录](docs/开发学习记录.md#第-255-步按用户归属读取指定存储对象)。

已提供 domain.CreateStagedObjectInput，保存服务端 Provider、Bucket、ObjectKey 和完整 SizeBytes。IsValid 检查必填 ASCII 文本及 32/63/255 列宽，大小保留 uint64 原值，上传上限继续由已有声明校验执行。字段/边界及现有 JSON→上传声明→暂存参数→对象/任务组装连续三次通过，模型与复验方法见 [第 256 步学习记录](docs/开发学习记录.md#第-256-步定义暂存对象创建输入模型)。

已提供 models.NewStagedObject，创建时显式映射可信用户、Provider、Bucket、ObjectKey、FileSize 和 Status 六列，采用 <-:create 权限，标识及记录时间由数据库生成。六列解析、七组插入/更新 SQL 检查及真实 MySQL 默认值、位置唯一键、用户外键和完整无符号参数连续三次通过，模型与复验方法见 [第 257 步学习记录](docs/开发学习记录.md#第-257-步定义暂存对象创建数据库映射)。

已提供 Store.CreateUserStagedObject，在已认证用户事务中校验暂存参数、固定 STAGING 插入六列，按归属及位置取得 ID，再复用完整对象读取核对初始事实。重复位置返回 ErrStoredObjectLocationConflict，错误交由外层回滚，提交成功后确认结果。创建/取消专项和真实 MySQL 单连接、范围隔离、回滚、授权复查、并发位置竞争及高位 ID 连续三次通过，调用与复验方法见 [第 258 步学习记录](docs/开发学习记录.md#第-258-步在已认证用户事务中创建暂存对象)。

已提供 models.NewUploadTask，按首期 SINGLE 流程创建映射用户/设备、暂存对象、目标目录、名称、上传方式、可空摘要、大小、阶段、预占及到期共十一列，采用 <-:create 权限。十一列解析、九组 SQL 和真实 MySQL 初始值、唯一/外键、阶段/预占约束及高位任务 ID 连续三次通过，模型与复验方法见 [第 259 步学习记录](docs/开发学习记录.md#第-259-步定义上传任务创建数据库映射)。

已提供 Store.CreateUserUploadTask，在已认证用户事务中核对声明、到期时间、目标目录及初始暂存对象，固定 SINGLE/UPLOADING 插入十一列，再定位并回读完整任务。预期摘要独立复制，重复暂存引用使用 ErrUploadTaskStagedObjectConflict，结果由外层提交确认。专项及真实 MySQL 单连接组合、回滚、授权、并发引用和高位标识连续三次通过，调用与复验方法见 [第 260 步学习记录](docs/开发学习记录.md#第-260-步在已认证用户事务中创建-single-上传任务)。

已提供 Store.ReserveUserUploadSpace，在已认证用户事务中当前读锁定配额，用安全减法判断剩余空间，零字节核对后成功，正值按原有配额条件更新 reserved_storage 和 UTC 时间。空间不足使用 ErrQuotaExceeded，外层失败可回滚。专项及真实 MySQL 边界、并发防超额、单连接重复累加和预占/对象/任务组合连续三次通过，调用与复验方法见 [第 261 步学习记录](docs/开发学习记录.md#第-261-步在已认证用户事务中预占上传空间)。

已提供 BuildCreateUploadRequestHash 和 v1.uploads.create 操作名，复用上传声明校验后，按固定字段顺序编码目录、展示名、字节数及可空 SHA-256，计算 32 字节请求摘要。目录及大小使用十进制文本，哈希使用小写十六进制并区分 null 与全零值。独立固定向量、参数/边界/并发及 HTTP 解析比较连续三次通过，规则和复验方法见 [第 262 步学习记录](docs/开发学习记录.md#第-262-步实现上传创建的幂等请求摘要)。

已提供 GenerateUploadObjectKey，按 users/{user_id}/objects/{UUIDv4} 为每次上传暂存尝试生成对象键，使用既有 UUID 库默认安全随机源和完整十进制用户 ID，失败返回空键及错误。格式/整数边界、固定向量、错误及连续/并发生成专项和正式 MySQL 生成键写入、回读、唯一键冲突与回滚连续三次通过，调用与复验方法见 [第 263 步学习记录](docs/开发学习记录.md#第-263-步实现服务端上传暂存对象键生成)。

已提供 CreateUploadService，复用已认证用户事务、完整祖先链校验、深度检查、随机对象键、配额预占及暂存/任务创建，返回提交后的独立 SINGLE/UPLOADING 任务，按方案默认 24 小时到期。服务专项与正式 MySQL 单连接、用户范围、三阶段失败/取消回滚及并发配额竞争连续三次通过，调用与复验方法见 [第 264 步学习记录](docs/开发学习记录.md#第-264-步组合上传创建核心服务)。

已提供 CreateUploadData、CreateUploadResponse 和 NewCreateUploadData，复用十三字段公开任务快照，返回 SINGLE、应用内容路径和一致的 UTC 微秒到期时间。完整 ID、显式 null、大小/时间边界和副本专项，以及正式 MySQL 创建、稳定结果保存、历史快照与四项回滚连续三次通过。OpenAPI 已补充响应 schema 和完整示例，规则及复验方法见 [第 265 步学习记录](docs/开发学习记录.md#第-265-步定义上传创建成功响应-dto)。

已提供 RestoreCreateUploadData，恢复稳定创建 JSON，核对外层四字段、任务十三字段及四项初始 null，校验完整标识、SINGLE/UPLOADING、16 MiB 大小、精确内容路径和到期时刻一致性，再转换 UTC 微秒。结构/值变异、历史快照、时间/副本/并发专项及正式 MySQL 持久异常快照识别连续三次通过，规则和复验方法见 [第 266 步学习记录](docs/开发学习记录.md#第-266-步恢复并校验上传创建响应快照)。

已提供 ValidateCreateUploadReplay，按用户/上传创建操作/键范围、保留期限、请求摘要和 UPLOAD_TASK/201/正文任务标识顺序校验已存结果，到期采用 now >= expires_at，完整快照继续由 RestoreCreateUploadData 恢复。范围/期限/冲突/资源与并发专项，以及正式 MySQL 历史结果、参数变化和异常持久关联连续三次通过，规则和复验方法见 [第 267 步学习记录](docs/开发学习记录.md#第-267-步校验上传创建幂等结果的复用条件)。

已提供 IdempotentCreateUploadService，首次请求在同一用户事务中创建预占、对象、任务并保存八天稳定结果；同键同参数重试校验后复用原快照，返回独立 JSON。服务专项与正式 MySQL 单连接、同账号其他设备、跨账号、历史恢复、四项回滚及三类并发连续三次通过，调用与复验方法见 [第 268 步学习记录](docs/开发学习记录.md#第-268-步组合上传创建幂等服务)。

已提供 handler.CreateUploadSuccess，恢复已确认提交的稳定上传快照，关联当前请求 ID 并预先编码，以 HTTP 201、UTF-8 JSON、no-store 和 Pragma no-cache 交付。成功/异常/取消/已发送及真实 POST 专项和正式 MySQL 幂等创建到实际 HTTP 交付连续三次通过，OpenAPI 补充 CreateUploadSucceeded，共 453 处本地引用核对通过。调用与复验方法见 [第 269 步学习记录](docs/开发学习记录.md#第-269-步实现上传创建成功-http-交付)。

已提供 handler.CreateUpload，按认证身份、幂等头、JSON/DTO、幂等服务和成功交付顺序处理上传创建请求，核对返回声明并映射 400/401/403/404/409/413/507/500。FILE_TOO_LARGE 提供 details.field=size_bytes 和 max_size_bytes=16777216。专项与真实 JWT/MySQL/HTTP 每轮三十次 POST 连续三次通过，OpenAPI 增加四类错误响应，461 处本地引用检查通过。调用与复验方法见 [第 270 步学习记录](docs/开发学习记录.md#第-270-步实现上传创建-http-处理函数)。

已注册正式 POST /api/v1/uploads，启动时将已校验存储配置、用户事务及上传 DTO 编解码绑定至幂等创建服务，再挂载授权和处理函数。路由、创建/查询及真实 run 启动/重启专项连续三次通过，OpenAPI 正式操作与注册一致，共十九条路径、二十一个操作和 474 处本地引用。调用与运行方法见 [第 271 步学习记录](docs/开发学习记录.md#第-271-步注册正式上传创建路由并完成启动装配)。

已提供 ValidateUploadContentState，按调用参数、任务范围/记录、SINGLE/UPLOADING/期限及大小/完整预占/接收字段顺序校验。其他有效阶段或到期返回 ErrUploadTaskNotReady，输入异常沿用数据错误，零字节与可空摘要有效。状态/边界/并发专项及正式 MySQL 读取组合连续三次通过，规则和复验方法见 [第 272 步学习记录](docs/开发学习记录.md#第-272-步校验上传内容接收前的任务状态)。

已提供 Store.LockUserUploadTask，在已认证用户事务中按可信用户及任务 ID 执行完整二十八列 FOR UPDATE 当前读，复用原任务校验和独立副本交付。真实 MySQL 核对已有快照后的最新事实、持锁阻塞、提交/回滚释放、取消及跨账号范围，专项连续三次通过。调用和复验方法见 [第 273 步学习记录](docs/开发学习记录.md#第-273-步在已认证用户事务中加锁读取上传任务)。

已提供 ValidateUploadContentLease，复用内容接收阶段判断，再核对租约配对、持有者、期限和版本。有效租约返回 ErrUploadTaskBusy，已达无符号上限的版本返回 ErrUploadLeaseVersionExhausted。恰好到期允许继续判断。业务专项及正式 MySQL 已认证事务、当前读和租约校验组合连续三次通过，边界与复验方法见 [第 274 步学习记录](docs/开发学习记录.md#第-274-步校验上传接收租约的可领取条件)。

已提供 Store.ClaimUserUploadContentLease，在已认证用户事务中锁定任务并核对预期对象/版本，读取 MySQL UTC 微秒时间，按有效阶段、期限及旧租约条件领取六十秒接收租约并安全递增版本。写后当前读复核，外层提交确认，失败/取消完整回滚。真实 MySQL 单连接、并发唯一领取和完整无符号范围连续三次通过，调用与复验方法见 [第 275 步学习记录](docs/开发学习记录.md#第-275-步在已认证用户事务中领取上传接收租约)。

已提供 Store.MarkUserStagedObjectOrphaned，在已认证用户事务中当前读核对对象、文件及未终结任务引用，确认已释放内容引用后将旧暂存对象标记 ORPHANED，并用数据库时间设置七天宽限期。重复标记保留原计时，引用切换和孤立标记可同事务提交/回滚。专项及真实 MySQL 联测连续三次通过，调用与复验方法见 [第 276 步学习记录](docs/开发学习记录.md#第-276-步在已认证用户事务中标记孤立暂存对象)。

已提供 Store.ReplaceUserUploadStagedObject，在已认证用户事务中核对任务、新旧对象和当前租约，通过数据库时间及旧对象/持有者/版本条件替换 staged_object_id，写后当前读复核。专项及真实 MySQL 连续三次验证旧租约拒绝、唯一引用冲突、并发唯一替换及租约/新对象/引用/孤立状态同事务提交与回滚。调用和复验方法见 [第 277 步学习记录](docs/开发学习记录.md#第-277-步在已认证用户事务中替换上传暂存对象)。

已提供 UploadContentPreparationService.Prepare，以一次已认证用户事务组合当前任务读取、随机持有者、租约领取及首次/重试对象准备。首次复用初始对象，重试创建新对象、条件替换引用并孤立旧对象，提交后交付独立任务和对象结果。服务专项及正式 MySQL 单连接、故障/取消整体回滚、并发和完整无符号边界连续三次通过，调用与复验方法见 [第 278 步学习记录](docs/开发学习记录.md#第-278-步组合上传内容接收准备服务)。

已提供 binding.ValidateUploadContentHeaders，按单个 application/octet-stream 和省略/单个 identity 内容编码校验原始文件请求，允许格式正确的媒体参数。校验先于租约准备及正文读取，不支持的格式沿统一 415 错误交付。新旧 HTTP 专项及真实已知长度/分块传输连续三次通过，规则和复验方法见 [第 279 步学习记录](docs/开发学习记录.md#第-279-步校验上传原始内容请求头)。

已提供 binding.ValidateUploadContentLength，使用 net/http 解析后的 ContentLength 核对任务声明大小。已知长度不一致返回 BODY_SIZE_MISMATCH 413，内部未知长度 -1 继续交给后续读取计数，无效负值返回固定 400。新旧 HTTP 专项及真实已知/分块传输连续三次通过，规则与复验方法见 [第 280 步学习记录](docs/开发学习记录.md#第-280-步校验上传请求长度与任务声明)。

已提供 service.CopyUploadContent，以固定缓冲流式复制声明范围内的文件内容并计算 SHA256，再读取一个额外检测字节确认大小。成功交付 UploadContentDigest，大小不符返回 ErrUploadBodySizeMismatch，I/O 错误和取消保留原因。暂存目标与输入流由调用方管理，失败后的已写前缀由调用方清理。专项、真实文件及 HTTP 已知/分块传输连续三次通过，调用与复验方法见 [第 281 步学习记录](docs/开发学习记录.md#第-281-步受限复制上传文件流并计算摘要)。

已提供 service.ValidateUploadContentChecksum，将成功复制的实际 SHA256 与任务可空预期摘要比较。省略时通过，显式全零值参与比较，任一字节不同返回固定 ErrUploadChecksumMismatch。真实文件与十八组 HTTP 场景连续三次验证大小/读取失败先行及摘要不符的 422 契约，规则与复验方法见 [第 282 步学习记录](docs/开发学习记录.md#第-282-步校验上传内容的预期摘要)。

已提供 service.WithUploadContentFile，在调用方指定的临时目录创建随机文件，组合受限复制、摘要校验、文件同步与回到开头，再交给文件使用回调。回调结束统一关闭并删除，失败与取消保留原因，关闭和删除错误继续合并。真实文件、panic/故障清理、并发及 HTTP 专项连续三次通过，调用过程与复验方法见 [第 283 步学习记录](docs/开发学习记录.md#第-283-步封装单次上传临时文件接收)。

已提供 UploadTempSpace，以共享单进程预算和互斥锁原子预占 128 MiB 临时容量，成功返回幂等释放方法，剩余不足返回 ErrUploadTempSpaceExhausted。零字节、完整整数、取消、并发和文件清理后释放专项连续三次通过，测试 HTTP 入口验证容量拒绝先于文件创建和正文读取。规则与复验方法见 [第 284 步学习记录](docs/开发学习记录.md#第-284-步实现上传临时区容量预占与释放)。

已提供 UploadContentFileService.WithFile，组合接收前参数校验、容量预占、完整文件交付及清理后的预算释放。清理失败时保留回收记录，RetryCleanup 确认回收后释放预算，正常错误、取消和 panic 均进入同一资源结算流程。专项及真实 HTTP 接收入口连续三次通过，调用与复验方法见 [第 285 步学习记录](docs/开发学习记录.md#第-285-步组合临时文件接收与容量预算服务)。

UploadContentFileService 已接入共享临时区最多三个在途接收名额，参数校验后先领名额，再预占容量。满额时返回 ErrUploadReceiverBusy，公开契约为 UPLOAD_BUSY 409；名额覆盖文件接收、使用及本次清理，调用结束后释放。跨服务实例、零字节、错误/取消/panic 及真实 HTTP 第四次拒绝和后续复用连续三次通过，规则与复验方法见 [第 286 步学习记录](docs/开发学习记录.md#第-286-步限制上传接收的并发名额)。

已提供 config.LoadUploadTempConfig，读取 CLOUDNEST_TEMP_DIR，默认采用系统临时目录下的 CloudNest/uploads。显式配置保留路径文本，支持中文、空格与相对路径，解析为本机绝对路径，拒绝空值、无效 UTF-8 和控制字符。目录创建与可用性检查由后续初始化步骤完成。配置包及启动入口回归通过，规则与复验方法见 [第 287 步学习记录](docs/开发学习记录.md#第-287-步读取并校验上传临时目录配置)。

已提供 storage.PrepareUploadTempDirectory，根据绝对目录配置创建所需目录并检查类型，再用随机探测文件验证写入、同步和读回，返回前关闭并删除探测文件。重复/并发初始化、文件冲突、取消与配置到实际文件接收的联测通过，调用和复验方法见 [第 288 步学习记录](docs/开发学习记录.md#第-288-步创建并检查上传临时目录)。

后端启动现已读取 CLOUDNEST_TEMP_DIR，先完成目录创建、类型与探测文件读写检查，再继续 JWT、数据库和 HTTP 启动。配置错误记录 config 阶段，初始化错误记录 upload_temp 阶段并结束启动，成功记录已解析目录。独占数据库、隔离临时目录和空闲端口的实际启动/重启验证通过，使用方法与验证见 [第 289 步学习记录](docs/开发学习记录.md#第-289-步将上传临时目录检查接入启动流程)。

已提供 Client.PutUploadContent，检查对象键、16 MiB 上限和临时文件大小，定位到开头后以单次条件 PUT 写入当前桶，返回可空 ETag、版本号及提交大小。使用 If-None-Match * 保护已有对象，沿用 SDK 签名并发送 Content-MD5，取消与存储故障按固定分类交付。真实 MinIO 空/二进制/16 MiB 读回、同键保护及测试对象版本清理复核通过，调用与验证见 [第 290 步学习记录](docs/开发学习记录.md#第-290-步将已校验临时文件写入-minio)。

已提供 Store.RecordUserUploadObjectWrite，在已认证用户事务中锁定任务与暂存对象，用数据库时间及条件更新核对当前对象、租约持有者/版本和期限，保存写入确认的 ETag、版本号与更新时间。重复结果可复用，缺失字段可补齐，冲突结果返回错误。独占 MySQL 提交/回滚、并发及完整无符号范围验证通过，调用与复验方法见 [第 291 步学习记录](docs/开发学习记录.md#第-291-步按接收租约记录暂存对象写入信息)。

已提供 Store.BeginUserUploadVerification，在同一已认证用户事务中保存当前写入确认，再按任务、对象及有效接收租约共同推进 VERIFYING，释放接收租约并清理旧错误与重试等待信息。双表写后当前读复核，故障和取消可随外层事务整体回滚。独占 MySQL 提交、重复/并发、故障注入和完整无符号边界通过验证，调用与复验方法见 [第 292 步学习记录](docs/开发学习记录.md#第-292-步将上传任务与对象推进校验阶段)。

上传临时文件服务已提供 `WithPreparedFile`。名额及容量预占成功后先执行准备回调，成功且未取消后再创建文件、读取正文。准备中断时释放名额和容量，原有文件清理和失败回收流程继续共用。调用顺序与复验方法见 [第 293 步学习记录](docs/开发学习记录.md#第-293-步增加上传正文读取前的准备入口)。

已提供 `UploadContentReceiveService.Receive`，使用已授权的任务声明，按资源预占、租约准备、临时文件接收、条件写入和事务推进的顺序返回 `VERIFYING` 任务。单连接 MySQL 验证两段事务、写回回滚和过期后的独立对象重试，真实 MinIO 适配器专项通过。调用与复验见 [第 294 步学习记录](docs/开发学习记录.md#第-294-步组合上传内容接收服务)。

已提供 `Store.RenewUserUploadContentLease`。当前对象、持有者和版本匹配且任务与租约仍有效时，以数据库时间延长接收租约，保留原版本及较长的已有期限。独占 MySQL 验证真实延期、失效拒绝、最大版本与回滚。调用与复验见 [第 295 步学习记录](docs/开发学习记录.md#第-295-步按当前租约续租上传接收)。

上传接收服务层已提供定时续租助手。它每 20 秒串行执行一次续租回调，失败时保留原始原因并取消接收。主动停止会等待正在执行的续租完成。调用和测试见 [第 296 步学习记录](docs/开发学习记录.md#第-296-步封装上传接收的定时续租)。

`UploadContentReceiveService.Receive` 已在准备事务提交后启动每 20 秒一次的续租。正文和存储写入共用可取消上下文，续租失败时返回原始原因。存储确认后先等待续租停止，再推进校验。调用及复验见 [第 297 步学习记录](docs/开发学习记录.md#第-297-步将定时续租接入上传接收)。

已提供 `binding.WithUploadTransferDeadline`，让 HTTP 正文读取与传输回调共用最长 5 分钟的截止时间，沿用更短的父期限。真实 HTTP 已验证正文未发完时退出、临时文件清理和成功后的连接复用。规则及复验见 [第 298 步学习记录](docs/开发学习记录.md#第-298-步限制上传-http-传输时间)。

已定义上传内容已接收的 `UploadContentAcceptedData` 和响应外层。接收服务的 `VERIFYING` 结果转换为字符串任务 ID 与当前状态，供后续 HTTP 202 成功交付使用。字段与复验见 [第 299 步学习记录](docs/开发学习记录.md#第-299-步定义上传内容已接收响应)。

已提供 `handler.UploadContentSuccess`，将提交到 `VERIFYING` 的任务交付为 HTTP 202，关联当前请求 ID，设置 UTF-8 JSON 和禁止缓存头。成功及失败边界通过测试入口验证。规则与复验见 [第 300 步学习记录](docs/开发学习记录.md#第-300-步交付上传内容接收成功响应)。

已提供 `handler.UploadContentFailure`，将接收失败原因转换为固定 HTTP 错误。期限助手标记 HTTP 上传超时，真实阻塞读取已验证 408 及请求 ID 交付。错误表与复验见 [第 301 步学习记录](docs/开发学习记录.md#第-301-步交付上传内容接收失败响应)。

已提供 `handler.UploadContent`，连接认证身份、任务查询、正文声明校验、传输期限与接收服务。测试入口验证了完整请求顺序，涵盖正常 202、长度或范围拒绝及服务错误。调用与复验见 [第 302 步学习记录](docs/开发学习记录.md#第-302-步组合上传内容接收处理函数)。

已提供上传正文收尾中间件。HTTP/1.x 返回响应时若正文仍未读完，会结束剩余读取并关闭本次连接，完整上传保留常规连接复用。真实网络验证与调用顺序见 [第 303 步学习记录](docs/开发学习记录.md#第-303-步收尾未读完的上传正文)。

已提供正式 `PUT /api/v1/uploads/{id}/content` 的路由注册方法与 OpenAPI 操作。正文收尾先于来源拒绝和认证，测试服务器已验证二进制请求、授权、未读正文收尾和创建/查询共存。启动入口装配随下一步接入。验证见 [第 304 步学习记录](docs/开发学习记录.md#第-304-步注册上传内容接收路由)。

正式后端已绑定 `PUT /api/v1/uploads/{id}/content`。启动入口复用共享临时区、MySQL 用户事务和 MinIO，实际写入完成后将任务和对象推进到 `VERIFYING` 并返回 202。真实启动、二进制与零字节读回及拒绝边界验证见 [第 305 步学习记录](docs/开发学习记录.md#第-305-步将上传内容接收接入后端启动)。

已提供 `Store.ListUploadVerificationCandidates`。它按数据库时间只读扫描已就绪的 `VERIFYING` 单文件任务，每批最多 100 条，返回用于后续领取的任务、用户、对象和租约版本。边界及复验见 [第 306 步学习记录](docs/开发学习记录.md#第-306-步查询待校验的上传任务)。

已提供 `Store.ClaimUploadVerificationLease`。校验 worker 可使用候选在所属用户事务中复核任务和暂存对象，领取 60 秒租约并递增版本。真实 MySQL 验证并发唯一领取、过期重领及回滚。调用与复验见 [第 307 步学习记录](docs/开发学习记录.md#第-307-步领取上传校验租约)。

已提供 `Client.OpenUploadVerificationObject`，可从 MinIO 打开待校验对象的原始内容流，按保存的版本定位并核对已知 ETag。真实版本桶已验证旧版本、`null` 版本、默认最新版本和缺失边界。调用与复验见 [第 308 步学习记录](docs/开发学习记录.md#第-308-步打开待校验的-minio-对象流)。

已提供 `service.ReadUploadVerificationDigest`，从待校验对象流一次计算实际大小和 SHA-256，复用声明长度限制及额外一字节探测。真实 MinIO 历史版本验证计数与摘要，细节见 [第 309 步学习记录](docs/开发学习记录.md#第-309-步计算待校验对象的大小和摘要)。

已提供 `service.VerifyUploadObject`，将校验租约快照检查、打开 MinIO 对象、重新计算并比较摘要、关闭对象流组成一次调用。真实版本桶和失败清理验证见 [第 310 步学习记录](docs/开发学习记录.md#第-310-步组合待校验对象的读回校验)。

已提供 `Store.WithUploadVerificationTransaction`。对象读回校验后，按当前用户、任务、对象位置和租约在短事务中复核，再交给后续写回。真实 MySQL 行锁、旧租约拒绝及回滚见 [第 311 步学习记录](docs/开发学习记录.md#第-311-步复核上传校验结果的写回资格)。

已提供 `Store.FindReusableUploadObject`。上传发布流程可在用户事务内按实测 SHA-256 与大小查找同账号的 `READY` 对象，命中时锁定候选行。查询边界和复验见 [第 312 步学习记录](docs/开发学习记录.md#第-312-步查询同用户可复用的上传对象)。

已提供 `Store.PromoteVerifiedUploadObject`。复用查询未命中后，可在写回复核事务中按当前任务和校验租约，将暂存对象推进到 `READY` 并保存实测 SHA-256。真实 MySQL 条件更新和回滚验证见 [第 313 步学习记录](docs/开发学习记录.md#第-313-步按校验租约推进暂存对象到-ready)。

已提供 `Store.SelectUploadPublicationObject`。写回复核回调可统一选择后续目录项要引用的 `READY` 对象，优先复用同账号已有对象，未命中时推进暂存对象。流程、去重冲突处理与复验见 [第 314 步学习记录](docs/开发学习记录.md#第-314-步选择上传发布对象)。

已提供 `service.BuildUploadConflictName`。它按 Demo 的编号格式生成单个上传重名候选，保留末级扩展名，并在 120 个 UTF-16 代码单元的限制内截短基名。用例和复验见 [第 315 步学习记录](docs/开发学习记录.md#第-315-步生成上传重名候选)。

已提供 `service.ResolveUploadPublicationName`。发布流程可传入当前用户事务的活动同名查询，依次检查原名和编号候选并返回第一个空闲的展示名。MySQL 联测和复验见 [第 316 步学习记录](docs/开发学习记录.md#第-316-步选择可用的上传发布名称)。

已提供 `service.BuildPublishedFileEntryInput`，将上传任务、选定名称、`READY` 对象及来源设备整理为发布文件目录项的写入输入。字段来源及复验见 [第 317 步学习记录](docs/开发学习记录.md#第-317-步整理发布文件目录项写入输入)。

已提供 `Store.FindUploadSourceDevice`。发布事务可按上传任务的设备 ID 读取同账号设备，为文件目录项提供真实的设备名称快照。查询与复验见 [第 318 步学习记录](docs/开发学习记录.md#第-318-步读取上传来源设备快照)。

已提供 `models.NewPublishedFileEntry`，映射创建发布文件目录项所需的十三列。GORM 生成 SQL 和正式 MySQL 的字段读回及约束检查见 [第 319 步学习记录](docs/开发学习记录.md#第-319-步定义发布文件目录项写入映射)。

已提供 `Store.CreatePublishedFileEntry`。上传发布事务可复核任务租约和引用，写入 `FILE/ACTIVE` 目录项，再按活动名称键读回。真实 MySQL 与回滚验证见 [第 320 步学习记录](docs/开发学习记录.md#第-320-步在发布事务中创建文件目录项)。

已提供 `Store.SettlePublishedUploadQuota`，在发布事务内将本任务预占等额转入用户已用配额，并同步清零任务预占。回滚、零字节及边界验证见 [第 321 步学习记录](docs/开发学习记录.md#第-321-步结算上传预占配额)。

已提供 `Store.CompletePublishedUpload`。文件创建并结算配额后，发布事务可按当前校验租约写入完成状态、实际名称、发布对象和结果目录项。真实 MySQL 回读及回滚见 [第 322 步学习记录](docs/开发学习记录.md#第-322-步写入上传任务的完成结果)。

已提供 `Store.ClearPublishedObjectGCTimer`。发布对象获得活动文件引用后，可在同一用户事务中清空它原有的无引用计时。联测与复验见 [第 323 步学习记录](docs/开发学习记录.md#第-323-步清除已发布对象的无引用计时)。

已提供 `Store.MarkDeduplicatedUploadObjectOrphaned`。任务完成并复用其他对象时，发布事务可为失去有效引用的暂存副本开启七天回收宽限期。行为与复验见 [第 324 步学习记录](docs/开发学习记录.md#第-324-步标记去重后剩余的暂存副本)。

已提供 `service.ValidateUploadPublicationTarget`，在当前发布事务中复核目标目录和全部祖先，并检查新增文件的深度。真实 MySQL 联测和复验见 [第 325 步学习记录](docs/开发学习记录.md#第-325-步复核上传发布的目标目录)。

已提供 `service.UploadPublicationService.Publish`。它在同一个写回复核事务中组合单文件发布步骤，提交后返回完成任务。真实 MySQL 验证了新对象、复用对象、零字节发布及整体回滚。[第 326 步学习记录](docs/开发学习记录.md#第-326-步组合单文件上传发布事务)

上传发布发现目标目录不可用时，现可在当前校验租约内将任务标为 `FAILED`、释放预占并开始暂存对象回收计时。任务查询交付 `TARGET_UNAVAILABLE`。[第 327 步学习记录](docs/开发学习记录.md#第-327-步终结发布目标不可用的上传任务)

已提供 `Store.FailUploadForChecksumMismatch`。完整读回确认 SHA-256 与声明不符时，可在当前校验租约内释放预占、记录 `FAILED/CHECKSUM_MISMATCH` 并启动暂存对象回收计时。[第 328 步学习记录](docs/开发学习记录.md#第-328-步终结摘要校验失败的上传任务)

已提供 `Store.FailUploadForBodySizeMismatch`。读回确认内容过短或超长后，可在当前校验租约内释放预占、记录 `FAILED/BODY_SIZE_MISMATCH` 并启动暂存对象回收计时。[第 329 步学习记录](docs/开发学习记录.md#第-329-步终结读回大小不符的上传任务)

已提供 `service.UploadVerificationService.VerifyAndPublish`。一次已领取的单文件校验会按读回结果调用发布或相应的失败事务，读取及关闭故障保留原任务供后续处理。[第 330 步学习记录](docs/开发学习记录.md#第-330-步组合一次后台单文件校验)

已提供 `Store.ScheduleUploadVerificationRetry`，按当前校验租约记录临时存储故障和下次尝试时间，释放租约并继续保留任务预占。扫描和领取均受下次尝试时间及任务期限约束。[第 331 步学习记录](docs/开发学习记录.md#第-331-步记录上传校验的下次重试时间)

`VerifyAndPublish` 读回遇到临时存储故障时会调用重试写回。提交后返回带下次尝试时间的 `VERIFYING` 任务，等待时间由已记录的重试次数决定，最多 30 秒。[第 332 步学习记录](docs/开发学习记录.md#第-332-步为临时存储故障安排上传校验重试)

已提供 `UploadVerificationCandidateProcessor.Process`。它从单条扫描候选领取租约，按已提交的任务查询暂存对象，再运行一次校验。[第 333 步学习记录](docs/开发学习记录.md#第-333-步处理单个上传校验候选)

已提供 `UploadVerificationBatch.RunOnce`。按给定上限扫描一次，逐条校验并汇总发布、重试和候选错误。取消时返回当前汇总。[第 334 步学习记录](docs/开发学习记录.md#第-334-步限量扫描并汇总一轮上传校验)

已提供 `RunUploadVerificationLoop`。首轮立即执行，每轮汇报后等待指定间隔再继续。取消会传给当前轮次，也可中断等待。[第 335 步学习记录](docs/开发学习记录.md#第-335-步为上传校验加入可取消的定时循环)

`cmd/server` 已提供 `newServerUploadVerificationBatch`，将正式 MySQL 仓储与 MinIO 按版本读回接到单轮校验入口。[第 336 步学习记录](docs/开发学习记录.md#第-336-步装配生产单轮上传校验入口)

后端启动后自动执行上传校验。单个 worker 启动时立即扫描，每轮最多 10 条，轮结束后等待 5 秒。退出时先等待 worker 停止，再释放 MinIO 和 MySQL 连接。[第 337 步学习记录](docs/开发学习记录.md#第-337-步将上传校验接入服务启动与退出)

真实重启联测覆盖已持久化的 `VERIFYING` 任务。在旧租约到期后，新实例首轮可领取任务，按保存的 MinIO 版本读回并发布。[第 338 步学习记录](docs/开发学习记录.md#第-338-步验证重启后恢复待校验上传)

后台上传校验的逐条错误日志包含 `task_id` 和固定的 `error_code`，可以按任务检索。[第 339 步学习记录](docs/开发学习记录.md#第-339-步记录上传校验任务的错误码)

已提供 `ListUploadExpirationCandidates`，按 MySQL 时间读取到期且可尝试处理的上传任务，供过期事务复核。[第 340 步学习记录](docs/开发学习记录.md#第-340-步扫描已到期的上传任务候选)

已提供 `BeginUploadExpiration`，按用户加锁复核到期候选，将符合条件的任务推进到 `EXPIRING`，保留任务预占供随后结算。[第 341 步学习记录](docs/开发学习记录.md#第-341-步将到期上传任务推进到过期处理阶段)

已提供 `CompleteUploadExpiration`，在同一用户事务中写入单文件任务的 `EXPIRED` 终态、释放预占并为暂存对象开始回收计时。[第 342 步学习记录](docs/开发学习记录.md#第-342-步终结过期的单文件上传任务)

已提供 `UploadExpirationCandidateProcessor.Process`，按扫描候选依次推进和收尾单文件任务，中断后可从已提交的 `EXPIRING` 接续。[第 343 步学习记录](docs/开发学习记录.md#第-343-步处理单个到期上传候选)

已提供 `UploadExpirationBatch.RunOnce`。每轮只扫描给定上限内的单文件到期任务，逐条处理并汇总终结数和候选错误。[第 344 步学习记录](docs/开发学习记录.md#第-344-步限量处理一轮单文件上传过期任务)

已提供 `RunUploadExpirationLoop`，用于立即执行首轮单文件过期处理、按间隔重复并响应取消。[第 345 步学习记录](docs/开发学习记录.md#第-345-步为单文件过期处理加入可取消的定时循环)

服务启动后自动处理到期的单文件上传。首轮立即扫描，每轮最多 100 条，结束后等待 30 秒。服务退出时等待过期 worker 停止后再关闭连接。[第 346 步学习记录](docs/开发学习记录.md#第-346-步将单文件过期处理接入服务启动与退出)

单文件上传过期处理失败时，worker 按候选记录 `task_id` 和固定 `error_code`。正常关停导致的取消保留在轮次计数中。[第 347 步学习记录](docs/开发学习记录.md#第-347-步记录上传过期任务的错误码)

已提供 `Store.ScheduleUploadExpirationRetry`，可为单文件 `EXPIRING` 任务保存下次尝试时间、恢复次数和固定数据库故障摘要。现有过期扫描会等到该时间再选中任务。[第 348 步学习记录](docs/开发学习记录.md#第-348-步记录上传过期任务的下次重试时间)

`Store.CompleteUploadExpiration` 会为确认的 MySQL 锁等待超时或死锁附加过期重试标记，同时保留原始错误。[第 349 步学习记录](docs/开发学习记录.md#第-349-步识别可重试的上传过期收尾错误)

后台过期处理遇到已标记的临时数据库故障时，会写回下次尝试时间。轮次汇总通过 `retries_scheduled` 记录已提交的重试安排。[第 350 步学习记录](docs/开发学习记录.md#第-350-步为临时数据库故障安排上传过期重试)

过期重试的重启联测覆盖等待期间跳过任务、后续扫描完成收尾和返还预占。[第 351 步学习记录](docs/开发学习记录.md#第-351-步验证重启后恢复等待重试的上传过期任务)

单文件上传取消的计划契约已写入 OpenAPI。取消处理中返回 HTTP 202，任务已到终态时返回 HTTP 200 和当前任务结果。[OpenAPI](api/openapi.yaml) · [第 352 步学习记录](docs/开发学习记录.md#第-352-步定义单文件上传取消接口契约)

已提供 `domain.ShouldBeginUploadCancellation`，按当前阶段、任务期限和租约判断能否进入 `CANCELLING`，或可直接复用当前状态。[第 353 步学习记录](docs/开发学习记录.md#第-353-步判断单文件上传任务的取消资格)

已提供 `Store.BeginUserUploadCancellation`。在已认证用户事务中按归属锁定单文件任务，满足期限和租约条件时写入 `CANCELLING`，并在提交后返回状态。[第 354 步学习记录](docs/开发学习记录.md#第-354-步在用户事务中受理单文件上传取消)
