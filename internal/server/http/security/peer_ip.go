package security

import (
	"fmt"
	"net/http"
	"net/netip"
)

// DirectPeerIP 只解析服务器提供的 TCP RemoteAddr，忽略所有客户端转发头。
// 当前按直连部署；代理后的原始用户 IP 需要另行配置可信代理规则。
func DirectPeerIP(request *http.Request) (netip.Addr, error) {
	// 1. 解析完整 IP:端口，不做 DNS 查询或转发头回退。
	if request == nil {
		return netip.Addr{}, fmt.Errorf("请求缺少有效的 TCP 来源地址")
	}
	peer, err := netip.ParseAddrPort(request.RemoteAddr)
	// 2. 拒绝无效端口和区域标识，合并 IPv4 映射地址。
	if err != nil || peer.Port() == 0 || peer.Addr().Zone() != "" {
		return netip.Addr{}, fmt.Errorf("请求缺少有效的 TCP 来源地址")
	}
	return peer.Addr().Unmap(), nil
}
