package pi

import (
	"sort"
	"strings"
)

// provider is one model provider the profile serves with an API key: the
// hosts pi sends its requests to, and the headers its key travels in there.
type provider struct {
	hosts   []string
	headers []string
}

// The headers a key travels in, by the API pi speaks to the provider:
// Anthropic's Messages API takes it in x-api-key (and a compatible
// endpoint may take it as a bearer token), the OpenAI and Mistral APIs as a
// bearer token, Gemini's in x-goog-api-key (probes/pirpc).
var (
	messagesHeaders = []string{"x-api-key", "Authorization"}
	bearerHeaders   = []string{"Authorization"}
	geminiHeaders   = []string{"x-goog-api-key"}
)

// providers are the providers the profile serves, by pi's name for them:
// those with fixed first-party hosts and the key in a header, as pi 1.0.4
// defines them (packages/ai/src/providers). A provider whose key reaches no
// fixed host, or travels in no header — Bedrock, Azure, Vertex, Cloudflare,
// Copilot, the subscription logins — is not here: a model of one is refused.
var providers = map[string]provider{
	"anthropic":                  {[]string{"api.anthropic.com"}, []string{"x-api-key"}},
	"openai":                     {[]string{"api.openai.com"}, bearerHeaders},
	"google":                     {[]string{"generativelanguage.googleapis.com"}, geminiHeaders},
	"ant-ling":                   {[]string{"api.ant-ling.com"}, bearerHeaders},
	"baseten":                    {[]string{"inference.baseten.co"}, bearerHeaders},
	"cerebras":                   {[]string{"api.cerebras.ai"}, bearerHeaders},
	"deepseek":                   {[]string{"api.deepseek.com"}, bearerHeaders},
	"fireworks":                  {[]string{"api.fireworks.ai"}, messagesHeaders},
	"groq":                       {[]string{"api.groq.com"}, bearerHeaders},
	"huggingface":                {[]string{"router.huggingface.co"}, bearerHeaders},
	"kimi-coding":                {[]string{"api.kimi.com"}, messagesHeaders},
	"meta":                       {[]string{"api.meta.ai"}, bearerHeaders},
	"minimax":                    {[]string{"api.minimax.io"}, messagesHeaders},
	"minimax-cn":                 {[]string{"api.minimaxi.com"}, messagesHeaders},
	"mistral":                    {[]string{"api.mistral.ai"}, bearerHeaders},
	"moonshotai":                 {[]string{"api.moonshot.ai"}, bearerHeaders},
	"moonshotai-cn":              {[]string{"api.moonshot.cn"}, bearerHeaders},
	"nvidia":                     {[]string{"integrate.api.nvidia.com"}, bearerHeaders},
	"openrouter":                 {[]string{"openrouter.ai"}, messagesHeaders},
	"qwen-token-plan":            {[]string{"token-plan.ap-southeast-1.maas.aliyuncs.com"}, bearerHeaders},
	"qwen-token-plan-cn":         {[]string{"token-plan.cn-beijing.maas.aliyuncs.com"}, bearerHeaders},
	"qwen-token-plan-individual": {[]string{"token-plan.ap-southeast-1.maas.aliyuncs.com"}, bearerHeaders},
	"together":                   {[]string{"api.together.ai"}, bearerHeaders},
	"vercel-ai-gateway":          {[]string{"ai-gateway.vercel.sh"}, messagesHeaders},
	"xai":                        {[]string{"api.x.ai"}, bearerHeaders},
	"xiaomi":                     {[]string{"api.xiaomimimo.com"}, bearerHeaders},
	"xiaomi-token-plan-ams":      {[]string{"token-plan-ams.xiaomimimo.com"}, bearerHeaders},
	"xiaomi-token-plan-cn":       {[]string{"token-plan-cn.xiaomimimo.com"}, bearerHeaders},
	"xiaomi-token-plan-sgp":      {[]string{"token-plan-sgp.xiaomimimo.com"}, bearerHeaders},
	"zai":                        {[]string{"api.z.ai"}, bearerHeaders},
	"zai-coding-cn":              {[]string{"open.bigmodel.cn"}, bearerHeaders},
}

// splitModel is a model's provider and its id there: "anthropic/claude-x"
// is provider anthropic, model claude-x. ok is false for a model that names
// no provider the profile serves.
func splitModel(model string) (name, id string, p provider, ok bool) {
	name, id, cut := strings.Cut(model, "/")
	if !cut || id == "" {
		return "", "", provider{}, false
	}
	p, ok = providers[name]
	return name, id, p, ok
}

// route is every provider's hosts and headers together: the api_key route,
// which each placeholder's swap narrows to its model's provider.
func route() (hosts, headers []string) {
	hs, hd := map[string]bool{}, map[string]bool{}
	for _, p := range providers {
		for _, h := range p.hosts {
			hs[h] = true
		}
		for _, h := range p.headers {
			hd[h] = true
		}
	}
	for h := range hs {
		hosts = append(hosts, h)
	}
	for h := range hd {
		headers = append(headers, h)
	}
	sort.Strings(hosts)
	sort.Strings(headers)
	return hosts, headers
}
