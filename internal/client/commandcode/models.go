package commandcode

import "strings"

var aliases = map[string]string{
	"deepseek-v4-pro": "deepseek/deepseek-v4-pro",
	"deepseek-v4": "deepseek/deepseek-v4-pro",
	"glm-5.2": "zai-org/GLM-5.2",
	"glm5.2": "zai-org/GLM-5.2",
	"kimi-k3": "moonshotai/Kimi-K3",
	"kimi3": "moonshotai/Kimi-K3",
}

var efforts = map[string][]string{
	"deepseek/deepseek-v4-pro": {"high", "max"},
	"zai-org/GLM-5.2": {"high", "max"},
}

func ResolveModel(model string) string { if v, ok := aliases[strings.ToLower(strings.TrimSpace(model))]; ok { return v }; return model }
func ResolveEffort(model, requested string) string { requested = strings.ToLower(strings.TrimSpace(requested)); valid := efforts[ResolveModel(model)]; if requested == "" || len(valid) == 0 { return requested }; rank := map[string]int{"low": 0, "medium": 1, "high": 2, "xhigh": 3, "max": 4}; if rank[requested] < rank[valid[0]] { return valid[0] }; for i := len(valid)-1; i >= 0; i-- { if rank[valid[i]] <= rank[requested] { return valid[i] } }; return valid[0] }
