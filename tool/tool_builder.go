package tool

import (
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"

	"egent-lobehub/yamlconfig"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

var envVarRe = regexp.MustCompile(`\$([A-Z][A-Z0-9_]*)`)

func BuildToolsFromConfig(cfg *yamlconfig.AgentConfig) ([]tool.BaseTool, error) {
	tools := make([]tool.BaseTool, 0, len(cfg.Tools))

	for _, t := range cfg.Tools {
		params := buildParams(t.Parameters)
		defaults := buildDefaults(t.Parameters)
		info := &schema.ToolInfo{
			Name:        t.Name,
			Desc:        t.Description,
			ParamsOneOf: schema.NewParamsOneOfByParams(params),
		}

		url := resolveEnvVars(t.URL)
		headers := resolveHeaders(t.HTTPHeaders)

		apiTool := NewAPITool(info, url, headers, defaults, t.Method)
		tools = append(tools, apiTool)
	}

	if len(tools) == 0 {
		return nil, fmt.Errorf("no tools built from config")
	}

	return tools, nil
}

func buildParams(parameters []yamlconfig.Parameter) map[string]*schema.ParameterInfo {
	params := make(map[string]*schema.ParameterInfo, len(parameters))
	for _, p := range parameters {
		pi := &schema.ParameterInfo{
			Desc:     p.Description,
			Type:     mapType(p.Type),
			Required: p.Required,
		}
		params[p.Name] = pi
	}
	return params
}

func buildDefaults(parameters []yamlconfig.Parameter) map[string]any {
	defaults := make(map[string]any)
	for _, p := range parameters {
		if p.Default != nil {
			defaults[p.Name] = p.Default
		}
	}
	return defaults
}

func mapType(t string) schema.DataType {
	switch strings.ToLower(t) {
	case "str", "string":
		return schema.String
	case "int", "integer":
		return schema.Integer
	case "float", "number":
		return schema.Number
	case "bool", "boolean":
		return schema.Boolean
	case "array", "list":
		return schema.Array
	case "object", "map":
		return schema.Object
	default:
		return schema.String
	}
}

func resolveEnvVars(s string) string {
	return envVarRe.ReplaceAllStringFunc(s, func(match string) string {
		varName := match[1:]
		val := os.Getenv(varName)
		if val == "" {
			slog.Warn("env var not set", "var", varName)
			return match
		}
		return val
	})
}

func resolveHeaders(headers map[string]string) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	resolved := make(map[string]string, len(headers))
	for k, v := range headers {
		resolved[k] = resolveEnvVars(v)
	}
	return resolved
}
