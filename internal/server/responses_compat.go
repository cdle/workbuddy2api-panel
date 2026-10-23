// responses_compat.go — small shims so the adapted translation layer
// (responses_adapters.go / responses_stream.go) compiles against this codebase.
// Config fields the upstream proxy derived from env are fixed constants here:
// workbuddy2api has a single chat upstream and no model override.
package server

func cloneMap(source map[string]any) map[string]any {
	clone := make(map[string]any, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

// respProxyConfig 提供翻译层需要的只读配置视图（原上游 proxy.Config 的裁剪版）。
type respProxyConfig struct{}

func (respProxyConfig) ModelOverride() string  { return "" }
func (respProxyConfig) ReasoningMode() ReasoningMode { return ReasoningEffort }
func (respProxyConfig) UpstreamBaseURL() string     { return "" }
