package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/gjson"
	"github.com/tidwall/resp"
)

const (
	pluginName         = "llm-tier-router"
	userAPIKeyHeader   = "X-User-API-Key"
	modelHeader        = "X-Model"
	redisKeyCtx        = "redis_key"
	tierCtx            = "selected_tier"
	redisTTLSeconds    = 86400
	redisCallTimeout   = 1000
	requestBodyMaxSize = 8 * 1024 * 1024
)

func main() {}

func init() {
	wrapper.SetCtx(
		pluginName,
		wrapper.ParseConfig(parseConfig),
		wrapper.ProcessRequestHeaders(onHttpRequestHeaders),
		wrapper.ProcessRequestBody(onHttpRequestBody),
		wrapper.ProcessResponseBody(onHttpResponseBody),
	)
}

type Config struct {
	RedisService string              `json:"redis_service"`
	RedisPort    int                 `json:"redis_port"`
	RedisPass    string              `json:"redis_pass"`
	ModelTiers   map[string][]Tier   `json:"model_tiers"`
	RedisClient  wrapper.RedisClient `json:"-"`
}

type Tier struct {
	MaxToken       int    `json:"max_token"`
	TargetProvider string `json:"target_provider"`
	TargetModel    string `json:"target_model"`
}

func parseConfig(raw gjson.Result, config *Config) error {
	if err := json.Unmarshal([]byte(raw.Raw), config); err != nil {
		return fmt.Errorf("parse config failed: %w", err)
	}

	if config.RedisService == "" {
		return errors.New("redis_service is required")
	}
	if config.RedisPort <= 0 {
		config.RedisPort = 6379
	}
	if len(config.ModelTiers) == 0 {
		return errors.New("model_tiers is required")
	}

	for model, tiers := range config.ModelTiers {
		for i, tier := range tiers {
			if tier.MaxToken <= 0 {
				return fmt.Errorf("model[%s].tiers[%d].max_token must be greater than 0", model, i)
			}
			if strings.TrimSpace(tier.TargetProvider) == "" {
				return fmt.Errorf("model[%s].tiers[%d].target_provider is required", model, i)
			}
			if strings.TrimSpace(tier.TargetModel) == "" {
				return fmt.Errorf("model[%s].tiers[%d].target_model is required", model, i)
			}
		}

		sort.Slice(tiers, func(i, j int) bool {
			return tiers[i].MaxToken < tiers[j].MaxToken
		})
		config.ModelTiers[model] = tiers
	}

	client := newRedisClient(*config)
	if err := client.Init("", config.RedisPass, redisCallTimeout); err != nil {
		return fmt.Errorf("init redis failed: %w", err)
	}
	config.RedisClient = client
	return nil
}

func onHttpRequestHeaders(ctx wrapper.HttpContext, config Config) types.Action {
	ctx.SetRequestBodyBufferLimit(requestBodyMaxSize)
	ctx.BufferRequestBody()
	_ = proxywasm.RemoveHttpRequestHeader("content-length")

	// 用户身份来自 New API 透传的 X-User-API-Key，真实 LLM 密钥不会暴露给调用方。
	userKey, _ := proxywasm.GetHttpRequestHeader(userAPIKeyHeader)
	userKey = strings.TrimSpace(userKey)
	if userKey == "" {
		sendJSON(401, `{"error":"缺失 X-User-API-Key 参数"}`)
		return types.HeaderStopAllIterationAndWatermark
	}

	// 按用户 Key 和日期生成 Redis 日累计 Key；日期进入 Key，TTL 负责自动清理。
	redisKey := dailyRedisKey(userKey)
	ctx.SetContext(redisKeyCtx, redisKey)

	client := config.RedisClient
	if err := client.Get(redisKey, func(value resp.Value) {
		if value.Error() != nil {
			proxywasm.LogErrorf("redis GET failed, key=%s, err=%v", redisKey, value.Error())
			sendJSON(503, `{"error":"redis 服务不可用，请稍后重试"}`)
			return
		}

		usedTokens := 0
		if !value.IsNull() {
			usedTokens = value.Integer()
		}

		// 从 X-Model 请求头获取模型名
		model, err := extractModelFromRequest()
		if err != nil {
			proxywasm.LogErrorf("extract model failed: %v", err)
			sendJSON(400, `{"error":"缺少 X-Model 请求头"}`)
			return
		}
		tiers, ok := config.ModelTiers[model]
		if !ok {
			proxywasm.LogErrorf("model %s not found in model_tiers config", model)
			sendJSON(400, `{"error":"不支持的模型: `+model+`"}`)
			return
		}

		tier := selectTier(tiers, usedTokens)
		ctx.SetContext(tierCtx, tier)
		applyTierMetadata(tier, usedTokens)

		if err := proxywasm.ResumeHttpRequest(); err != nil {
			proxywasm.LogErrorf("resume request failed: %v", err)
		}
	}); err != nil {
		proxywasm.LogErrorf("dispatch redis GET failed, key=%s, err=%v", redisKey, err)
		sendJSON(503, `{"error":"redis 服务不可用，请稍后重试"}`)
		return types.HeaderStopAllIterationAndBuffer
	}

	return types.HeaderStopAllIterationAndBuffer
}

func extractModelFromRequest() (string, error) {
	model, err := proxywasm.GetHttpRequestHeader(modelHeader)
	if err != nil {
		return "", fmt.Errorf("获取 %s 请求头失败: %w", modelHeader, err)
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return "", fmt.Errorf("缺少 %s 请求头", modelHeader)
	}
	return model, nil
}

func onHttpRequestBody(ctx wrapper.HttpContext, config Config, body []byte) types.Action {
	tier, ok := ctx.GetContext(tierCtx).(Tier)
	if !ok || tier.TargetModel == "" || len(body) == 0 {
		return types.DataContinue
	}

	rewrittenBody, err := replaceRequestModel(body, tier.TargetModel)
	if err != nil {
		proxywasm.LogErrorf("replace request model failed: %v", err)
		return types.DataContinue
	}
	if err := proxywasm.ReplaceHttpRequestBody(rewrittenBody); err != nil {
		proxywasm.LogErrorf("replace request body failed: %v", err)
		return types.DataContinue
	}

	// 移除内部头，路由匹配已完成
	removeInternalHeaders()

	return types.DataContinue
}

func removeInternalHeaders() {
	_ = proxywasm.RemoveHttpRequestHeader(userAPIKeyHeader)  // X-User-API-Key
	_ = proxywasm.RemoveHttpRequestHeader(modelHeader)       // X-Model
	_ = proxywasm.RemoveHttpRequestHeader("X-Tier-Provider") // X-Tier-Provider
}

func onHttpResponseBody(ctx wrapper.HttpContext, config Config, body []byte) types.Action {
	addTokens := totalTokensFromOpenAIResponse(body)
	if addTokens <= 0 {
		return types.ActionContinue
	}

	redisKey := ctx.GetStringContext(redisKeyCtx, "")
	if redisKey == "" {
		return types.ActionContinue
	}

	// 5. Lua 保证 INCRBY 与 EXPIRE 在 Redis 侧原子执行，适合高并发下累加 Token。
	lua := `
local current = redis.call("INCRBY", KEYS[1], ARGV[1])
redis.call("EXPIRE", KEYS[1], ARGV[2])
return current
`

	client := config.RedisClient
	err := client.Eval(
		lua,
		1,
		[]interface{}{redisKey},
		[]interface{}{addTokens, redisTTLSeconds},
		func(value resp.Value) {
			if value.Error() != nil {
				proxywasm.LogErrorf("redis EVAL failed, key=%s, tokens=%d, err=%v", redisKey, addTokens, value.Error())
			}
		},
	)
	if err != nil {
		proxywasm.LogErrorf("dispatch redis EVAL failed, key=%s, tokens=%d, err=%v", redisKey, addTokens, err)
	}

	return types.ActionContinue
}

func newRedisClient(config Config) wrapper.RedisClient {
	clusterName := fmt.Sprintf("outbound|%d||%s", config.RedisPort, config.RedisService)
	return wrapper.NewRedisClusterClient(namedCluster{
		clusterName: clusterName,
		hostName:    config.RedisService,
	})
}

func dailyRedisKey(userKey string) string {
	return fmt.Sprintf("higress:llm:token:%s:%s", time.Now().Format("20060102"), userKey)
}

func selectTier(tiers []Tier, usedTokens int) Tier {
	for _, tier := range tiers {
		if usedTokens < tier.MaxToken {
			return tier
		}
	}
	return tiers[len(tiers)-1]
}

func applyTierMetadata(tier Tier, usedTokens int) {
	headers := map[string]string{
		"X-Higress-Tier-Limit": strconv.Itoa(tier.MaxToken),
		"X-Higress-Used-Token": strconv.Itoa(usedTokens),
	}

	if tier.TargetProvider != "" {
		headers["X-Tier-Provider"] = tier.TargetProvider
	}
	if tier.TargetModel != "" {
		headers["X-Higress-Target-Model"] = tier.TargetModel
	}

	for key, value := range headers {
		if err := proxywasm.ReplaceHttpRequestHeader(key, value); err != nil {
			proxywasm.LogErrorf("replace request header %s failed: %v", key, err)
		}
	}
}

func totalTokensFromOpenAIResponse(body []byte) int {
	var response struct {
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return 0
	}
	return response.Usage.TotalTokens
}

func replaceRequestModel(body []byte, model string) ([]byte, error) {
	var request map[string]interface{}
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, err
	}
	request["model"] = model
	return json.Marshal(request)
}

func sendJSON(statusCode uint32, body string) {
	_ = proxywasm.SendHttpResponse(
		statusCode,
		[][2]string{{"content-type", "application/json; charset=utf-8"}},
		[]byte(body),
		-1,
	)
}

type namedCluster struct {
	clusterName string
	hostName    string
}

func (c namedCluster) ClusterName() string {
	return c.clusterName
}

func (c namedCluster) HostName() string {
	return c.hostName
}
