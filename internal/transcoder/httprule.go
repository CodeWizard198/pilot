package transcoder

import (
	"fmt"

	"github.com/jhump/protoreflect/desc"
	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/proto"
)

type HTTPRule struct {
	Method string // http方法
	Path   string // 请求路径
	Body   string // body字段 为*时表示合并所有参数
}

// ExtractHTTPRules 从方法描述符中提取HTTP规则
func ExtractHTTPRules(method *desc.MethodDescriptor) ([]*HTTPRule, error) {
	// 获取gRPC Options
	opts := method.GetMethodOptions()
	if opts == nil {
		return nil, nil
	}

	// 检查是否设置 google.api.http option
	if !proto.HasExtension(opts, annotations.E_Http) {
		return nil, nil
	}

	// 获取http规则
	ext := proto.GetExtension(opts, annotations.E_Http)
	httpRule, ok := ext.(*annotations.HttpRule)
	if !ok || httpRule == nil {
		return nil, nil
	}

	rules := make([]*HTTPRule, 0)

	// 解析主要规则
	mainRule, err := parseHTTPRule(httpRule)
	if err != nil {
		return nil, fmt.Errorf("failed to parse HTTP rule: %w", err)
	}
	if mainRule != nil {
		rules = append(rules, mainRule)
	}

	// 解析额外绑定规则
	for _, additionalRule := range httpRule.AdditionalBindings {
		rule, err := parseHTTPRule(additionalRule)
		if err != nil {
			return nil, fmt.Errorf("failed to parse additional binding: %w", err)
		}
		if rule != nil {
			rules = append(rules, rule)
		}
	}

	return rules, nil
}

// parseHTTPRule 解析单个HTTP规则
func parseHTTPRule(rule *annotations.HttpRule) (*HTTPRule, error) {
	if rule == nil {
		return nil, nil
	}

	httpRule := &HTTPRule{
		Body: rule.Body,
	}

	// 解析HTTP方法和路径
	switch pattern := rule.Pattern.(type) {
	case *annotations.HttpRule_Get:
		httpRule.Method = "GET"
		httpRule.Path = pattern.Get
	case *annotations.HttpRule_Post:
		httpRule.Method = "POST"
		httpRule.Path = pattern.Post
	case *annotations.HttpRule_Put:
		httpRule.Method = "PUT"
		httpRule.Path = pattern.Put
	case *annotations.HttpRule_Delete:
		httpRule.Method = "DELETE"
		httpRule.Path = pattern.Delete
	case *annotations.HttpRule_Patch:
		httpRule.Method = "PATCH"
		httpRule.Path = pattern.Patch
	case *annotations.HttpRule_Custom:
		httpRule.Method = pattern.Custom.Kind
		httpRule.Path = pattern.Custom.Path
	default:
		return nil, fmt.Errorf("unknown HTTP rule pattern type")
	}

	return httpRule, nil
}