package opstools

import (
	"encoding/json"
	"strings"
)

// toolEffects are the fixed effects of the ops tools whose effect does not
// depend on their parameters.
var toolEffects = map[string]Effect{
	"dev_deploy_verify": Read,
	"staypoint_query":   Read,
	"task_comment":      DevWrite,
	"task_doc":          DevWrite,
	"pr_body":           DevWrite,
}

// Declared returns the effect an ops tool call declares from its raw
// parameters, as the run timeline shows it. ok is false for a tool that is
// not an ops tool. An action or base it does not recognise is reported as
// prod_write, as the server would refuse or hold it.
func Declared(tool string, rawArgs []byte) (e Effect, ok bool) {
	tool = strings.TrimPrefix(tool, "mcp__staypoint__")
	if e, ok := toolEffects[tool]; ok {
		return e, true
	}
	switch tool {
	case "dev_host_run":
		var a struct {
			Action string `json:"action"`
		}
		_ = json.Unmarshal(rawArgs, &a)
		if e, ok := devActions[a.Action]; ok {
			return e, true
		}
		return ProdWrite, true
	case "pr_merge":
		var a struct {
			Base string `json:"base"`
		}
		_ = json.Unmarshal(rawArgs, &a)
		return MergeEffect(a.Base), true
	}
	return "", false
}
