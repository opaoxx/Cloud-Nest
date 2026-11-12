# CloudNest

学习导向的自用文件云盘，采用 Go 后端、React Web、Wails 桌面端和 Flutter 安卓端。

当前已完成本机开发环境、基础容器及首期八张业务表初始化。后端提供 /ping、/health/live、/health/ready、受保护的 GET /api/v1/me、POST /api/v1/auth/login 和 POST /api/v1/auth/refresh，支持配置及 CORS 白名单放行/预检、JSON 日志、依赖客户端、退出信号处理、请求 ID 和统一错误响应。启动时加载 JWT 配置并检查数据库连接、迁移记录及已有存储桶访问，文件业务按技术方案分阶段实现。

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
