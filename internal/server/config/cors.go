package config

import (
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// originDomainPattern 限定 ASCII 域名标签，每个标签不超过 63 字符，允许根域名末尾的点。
var originDomainPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*\.?$`)

// numericOriginLabelPattern 排除浏览器会按特殊 IPv4 规则解释的数字或十六进制末段。
var numericOriginLabelPattern = regexp.MustCompile(`^(?:[0-9]+|0x[0-9a-f]*)$`)

// LoadAllowedOrigins 读取 CLOUDNEST_ALLOWED_ORIGINS，返回规范化并去重的来源列表。
// 未设置时采用 http://127.0.0.1:5173；显式空值或任一无效条目返回 nil 和错误。
// 本方法只处理配置，不放行跨域请求，也不执行 DNS 或网络访问。
func LoadAllowedOrigins() ([]string, error) {
	// 1. 读取环境变量，未设置时使用本地前端默认来源。
	configuredOrigins := envOrDefault("CLOUDNEST_ALLOWED_ORIGINS", "http://127.0.0.1:5173")

	// 2. 按逗号拆分列表，去除条目首尾空白。
	originTexts := strings.Split(configuredOrigins, ",")
	allowedOrigins := make([]string, 0, len(originTexts))
	seenOrigins := make(map[string]bool)
	for originIndex, originText := range originTexts {
		originText = strings.TrimSpace(originText)

		// 3. 逐项校验并规范化来源，失败时返回配置位置。
		origin, err := normalizeAllowedOrigin(originText)
		if err != nil {
			return nil, fmt.Errorf("CLOUDNEST_ALLOWED_ORIGINS 第 %d 项无效: %w", originIndex+1, err)
		}

		// 4. 按首次出现顺序去重，返回完整白名单。
		if !seenOrigins[origin] {
			allowedOrigins = append(allowedOrigins, origin)
			seenOrigins[origin] = true
		}
	}
	return allowedOrigins, nil
}

// normalizeAllowedOrigin 将受支持的 HTTP(S) 根地址转换为来源字符串。
// 错误只包含本地规则，不回显原始 URL 或解析库中可能含凭证的错误。
func normalizeAllowedOrigin(value string) (string, error) {
	// 1. 只接受显式 HTTP(S) 协议的根地址，拒绝参数、片段、凭证和内部空白。
	if value == "" || strings.ContainsFunc(value, unicode.IsSpace) {
		return "", fmt.Errorf("来源不能为空或包含内部空白")
	}
	originURL, err := url.Parse(value)
	if err != nil || originURL.Hostname() == "" || (originURL.Scheme != "http" && originURL.Scheme != "https") {
		return "", fmt.Errorf("来源必须包含明确的 http:// 或 https:// 协议与主机")
	}
	if originURL.User != nil || (originURL.Path != "" && originURL.Path != "/") || originURL.RawPath != "" || strings.ContainsAny(value, "?#") {
		return "", fmt.Errorf("来源不能包含账号密码、业务路径、查询参数或片段")
	}

	// 2. 规范化 IP 或 ASCII 域名，拒绝浏览器可能以其他方式解释的主机写法。
	hostname := strings.ToLower(originURL.Hostname())
	originHost := hostname
	address, addressError := netip.ParseAddr(hostname)
	//能解析成ip地址
	if addressError == nil {
		if address.Zone() != "" || address.Is4In6() {
			return "", fmt.Errorf("来源不支持区域标识或 IPv4 映射的 IPv6")
		}
		if address.Is6() {
			if !strings.HasPrefix(originURL.Host, "[") {
				return "", fmt.Errorf("IPv6 来源必须使用方括号")
			}
			originHost = "[" + address.String() + "]"
		} else if strings.HasPrefix(originURL.Host, "[") {
			return "", fmt.Errorf("IPv4 来源不能使用方括号")
		} else {
			originHost = address.String()
		}
	} else {
		//不能解析成ip地址，判断是否是域名
		domain := strings.TrimSuffix(hostname, ".")
		lastLabel := domain[strings.LastIndex(domain, ".")+1:]
		if len(domain) > 253 || !originDomainPattern.MatchString(hostname) || numericOriginLabelPattern.MatchString(lastLabel) {
			return "", fmt.Errorf("主机必须是 ASCII 域名、标准 IPv4 或带方括号的 IPv6，不支持通配或特殊数字主机")
		}
	}

	// 3. 校验显式端口，移除默认端口，再输出不带路径的来源。
	if strings.HasSuffix(originURL.Host, ":") {
		return "", fmt.Errorf("显式端口不能为空")
	}
	portText := originURL.Port()
	if portText != "" {
		portNumber, err := strconv.ParseUint(portText, 10, 16)
		if err != nil || portNumber == 0 {
			return "", fmt.Errorf("端口必须是 1～65535 的数字")
		}
		isDefaultPort := (originURL.Scheme == "http" && portNumber == 80) || (originURL.Scheme == "https" && portNumber == 443)
		if !isDefaultPort {
			originHost += ":" + strconv.FormatUint(portNumber, 10)
		}
	}
	return originURL.Scheme + "://" + originHost, nil
}
