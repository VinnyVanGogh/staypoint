package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/trackgate"
)

// Hook contract tests (task-52c7dbad). Every response a StayPoint hook prints
// is checked against the documented output schema of the client that reads
// it, and pinned byte-for-byte in testdata/hook_contract. #250 is why: agy's
// PreToolUse output requires "decision", and a bare {} denied every Gemini
// tool call for a day without any test noticing.
//
// Schemas, as documented:
//   - agy: ~/.gemini/antigravity-cli/builtin/skills/agy-customizations/docs/hooks.md
//     (keys are camelCase protojson). The agy binary's struct tag for the
//     PreToolUse decision is
//     `jsonschema:"required,enum=allow,enum=deny,enum=ask,enum=force_ask,enum=deny_unless_prior_grant"`.
//   - Claude Code: https://code.claude.com/docs/en/hooks ("PreToolUse decision
//     control", "UserPromptSubmit decision control", "JSON output").
//
// The validators are stricter than the clients: an unknown key fails, since a
// misspelt or misplaced key (additionalContext at the top level, inject_steps)
// is silently dropped by the client rather than rejected.

var updateHookGolden = flag.Bool("update-hook-golden", false, "rewrite testdata/hook_contract/*.golden.json from current hook output")

type hookSchema func(out string) error

// hookObject decodes out as the single JSON object a hook must print.
// Claude Code parses stdout as JSON only when it starts with { and ends with }.
func hookObject(out string) (map[string]json.RawMessage, error) {
	s := strings.TrimSpace(out)
	if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
		return nil, fmt.Errorf("output is not a JSON object: %q", out)
	}
	dec := json.NewDecoder(strings.NewReader(s))
	var obj map[string]json.RawMessage
	if err := dec.Decode(&obj); err != nil {
		return nil, fmt.Errorf("output is not valid JSON: %v", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("output holds more than one JSON value: %q", out)
	}
	return obj, nil
}

func onlyKeys(obj map[string]json.RawMessage, where string, allowed ...string) error {
	ok := map[string]bool{}
	for _, k := range allowed {
		ok[k] = true
	}
	for k := range obj {
		if !ok[k] {
			return fmt.Errorf("%s: key %q is not in the client schema (allowed: %s)", where, k, strings.Join(allowed, ", "))
		}
	}
	return nil
}

// stringField returns obj[key] as a string; present reports whether it was set.
func stringField(obj map[string]json.RawMessage, key string) (val string, present bool, err error) {
	raw, ok := obj[key]
	if !ok {
		return "", false, nil
	}
	if err := json.Unmarshal(raw, &val); err != nil {
		return "", true, fmt.Errorf("%q must be a string, got %s", key, raw)
	}
	return val, true, nil
}

func enumField(obj map[string]json.RawMessage, key string, values ...string) (string, bool, error) {
	v, present, err := stringField(obj, key)
	if err != nil || !present {
		return v, present, err
	}
	for _, e := range values {
		if v == e {
			return v, true, nil
		}
	}
	return v, true, fmt.Errorf("%q = %q, want one of %s", key, v, strings.Join(values, ", "))
}

func objectField(obj map[string]json.RawMessage, key string) (map[string]json.RawMessage, bool, error) {
	raw, ok := obj[key]
	if !ok {
		return nil, false, nil
	}
	var o map[string]json.RawMessage
	if err := json.Unmarshal(raw, &o); err != nil || o == nil {
		return nil, true, fmt.Errorf("%q must be an object, got %s", key, raw)
	}
	return o, true, nil
}

// claudeCommonKeys are the JSON output fields every Claude Code event takes.
var claudeCommonKeys = []string{"continue", "stopReason", "suppressOutput", "systemMessage"}

// claudeHookSpecific checks hookSpecificOutput, when present, names event and
// holds only keys.
func claudeHookSpecific(obj map[string]json.RawMessage, event string, keys ...string) (map[string]json.RawMessage, error) {
	hso, present, err := objectField(obj, "hookSpecificOutput")
	if err != nil || !present {
		return nil, err
	}
	if err := onlyKeys(hso, "hookSpecificOutput", append([]string{"hookEventName"}, keys...)...); err != nil {
		return nil, err
	}
	if name, _, err := stringField(hso, "hookEventName"); err != nil || name != event {
		return nil, fmt.Errorf("hookSpecificOutput.hookEventName = %q, want %q (%v)", name, event, err)
	}
	return hso, nil
}

// claudePreToolUseSchema: decision in hookSpecificOutput.permissionDecision;
// the top-level decision/reason pair is deprecated but still read, with
// "approve"/"block" mapping to allow/deny. A deny must carry a reason, and
// when both forms are present they must agree.
func claudePreToolUseSchema(out string) error {
	obj, err := hookObject(out)
	if err != nil {
		return err
	}
	if err := onlyKeys(obj, "top level", append(claudeCommonKeys, "decision", "reason", "hookSpecificOutput")...); err != nil {
		return err
	}
	legacy, hasLegacy, err := enumField(obj, "decision", "approve", "block")
	if err != nil {
		return err
	}
	legacyReason, _, err := stringField(obj, "reason")
	if err != nil {
		return err
	}
	hso, err := claudeHookSpecific(obj, "PreToolUse", "permissionDecision", "permissionDecisionReason", "updatedInput", "additionalContext")
	if err != nil {
		return err
	}
	var perm, permReason string
	hasPerm := false
	if hso != nil {
		if perm, hasPerm, err = enumField(hso, "permissionDecision", "allow", "deny", "ask", "defer"); err != nil {
			return err
		}
		if permReason, _, err = stringField(hso, "permissionDecisionReason"); err != nil {
			return err
		}
		if _, _, err := objectField(hso, "updatedInput"); err != nil {
			return err
		}
	}
	if hasLegacy && hasPerm && (legacy == "block") != (perm == "deny") {
		return fmt.Errorf("top-level decision %q disagrees with permissionDecision %q", legacy, perm)
	}
	if legacy == "block" && strings.TrimSpace(legacyReason) == "" {
		return fmt.Errorf("decision block with an empty reason")
	}
	if perm == "deny" && strings.TrimSpace(permReason) == "" {
		return fmt.Errorf("permissionDecision deny with an empty permissionDecisionReason")
	}
	return nil
}

// claudeUserPromptSubmitSchema: context goes in
// hookSpecificOutput.additionalContext; the only top-level decision is "block".
func claudeUserPromptSubmitSchema(out string) error {
	obj, err := hookObject(out)
	if err != nil {
		return err
	}
	if err := onlyKeys(obj, "top level", append(claudeCommonKeys, "decision", "reason", "hookSpecificOutput")...); err != nil {
		return err
	}
	if _, _, err := enumField(obj, "decision", "block"); err != nil {
		return err
	}
	hso, err := claudeHookSpecific(obj, "UserPromptSubmit", "additionalContext", "sessionTitle", "suppressOriginalPrompt")
	if err != nil {
		return err
	}
	if hso != nil {
		if ctx, present, err := stringField(hso, "additionalContext"); err != nil {
			return err
		} else if present && strings.TrimSpace(ctx) == "" {
			return fmt.Errorf("hookSpecificOutput.additionalContext is empty")
		}
	}
	return nil
}

// agyPreToolUseSchema: decision is required; {} is read as a deny with an
// empty reason (#250). A deny must say why: agy shows the reason verbatim.
func agyPreToolUseSchema(out string) error {
	obj, err := hookObject(out)
	if err != nil {
		return err
	}
	if err := onlyKeys(obj, "top level", "decision", "reason", "permissionOverrides", "overwrite"); err != nil {
		return err
	}
	decision, present, err := enumField(obj, "decision", "allow", "deny", "ask", "force_ask", "deny_unless_prior_grant")
	if err != nil {
		return err
	}
	if !present {
		return fmt.Errorf(`"decision" is required (agy reads its absence as a deny with no reason)`)
	}
	reason, _, err := stringField(obj, "reason")
	if err != nil {
		return err
	}
	if decision == "deny" && strings.TrimSpace(reason) == "" {
		return fmt.Errorf("deny with an empty reason")
	}
	if raw, ok := obj["permissionOverrides"]; ok {
		var s []string
		if err := json.Unmarshal(raw, &s); err != nil {
			return fmt.Errorf(`"permissionOverrides" must be an array of strings, got %s`, raw)
		}
	}
	if _, _, err := objectField(obj, "overwrite"); err != nil {
		return err
	}
	return nil
}

// agyPreInvocationSchema: optional injectSteps, each step exactly one of
// toolCall, userMessage or ephemeralMessage.
func agyPreInvocationSchema(out string) error {
	obj, err := hookObject(out)
	if err != nil {
		return err
	}
	if err := onlyKeys(obj, "top level", "injectSteps"); err != nil {
		return err
	}
	raw, ok := obj["injectSteps"]
	if !ok {
		return nil
	}
	var steps []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &steps); err != nil {
		return fmt.Errorf(`"injectSteps" must be an array of objects, got %s`, raw)
	}
	for i, step := range steps {
		where := fmt.Sprintf("injectSteps[%d]", i)
		if err := onlyKeys(step, where, "toolCall", "userMessage", "ephemeralMessage"); err != nil {
			return err
		}
		if len(step) != 1 {
			return fmt.Errorf("%s must hold exactly one step type, got %d keys", where, len(step))
		}
		for _, k := range []string{"userMessage", "ephemeralMessage"} {
			if v, present, err := stringField(step, k); err != nil {
				return fmt.Errorf("%s: %v", where, err)
			} else if present && strings.TrimSpace(v) == "" {
				return fmt.Errorf("%s.%s is empty", where, k)
			}
		}
		if tc, present, err := objectField(step, "toolCall"); err != nil {
			return fmt.Errorf("%s: %v", where, err)
		} else if present {
			if name, _, _ := stringField(tc, "name"); name == "" {
				return fmt.Errorf("%s.toolCall.name is required", where)
			}
		}
	}
	return nil
}

type hookContractCase struct {
	name   string
	schema hookSchema
	out    string
}

// hookContractCases is every distinct response the pre-tool and prompt hooks
// print, rendered through the same functions the hook paths call.
func hookContractCases(t *testing.T) []hookContractCase {
	pinnedInput := json.RawMessage(`{"command":"bash /tmp/fix.sh","description":"run fix"}`)
	notices := []string{"⚠️ [STAYPOINT QUOTA NOTICE]: quota low.", "📡 [STAYPOINT WIRE :: PEER AGENT BROADCASTS]:\n  • [general] <a>: hi"}
	return []hookContractCase{
		// Claude Code PreToolUse.
		{"claude_pretool_allow", claudePreToolUseSchema, preToolAllowJSON(trackgate.ClientClaude)},
		{"claude_pretool_allow_stdout", claudePreToolUseSchema, captureStdout(t, preToolAllow)},
		{"claude_pretool_deny", claudePreToolUseSchema, preToolDeny(trackgate.ClientClaude, "Board denied: pushes to main")},
		{"claude_pretool_deny_empty_reason", claudePreToolUseSchema, preToolDeny(trackgate.ClientClaude, "  ")},
		{"claude_pretool_block_stdout", claudePreToolUseSchema, captureStdout(t, func() { preToolBlock("Board gate unreachable; command blocked (x)") })},
		{"claude_pretool_deferred", claudePreToolUseSchema, claudeBlockJSON(deferredMessage("gr-1"))},
		{"claude_pretool_nested_agent", claudePreToolUseSchema, preToolDeny(trackgate.ClientClaude, nestedAgentReason)},
		{"claude_pretool_pinned", claudePreToolUseSchema, pinnedHookOutput(pinnedInput, "bash /dev/fd/3 3<<'EOF'\necho fixed\nEOF")},
		// Claude Code UserPromptSubmit.
		{"claude_prompt_empty", claudeUserPromptSubmitSchema, promptHookOutput(false, nil)},
		{"claude_prompt_notices", claudeUserPromptSubmitSchema, promptHookOutput(false, notices)},
		// agy PreToolUse.
		{"agy_pretool_allow", agyPreToolUseSchema, preToolAllowJSON(trackgate.ClientGemini)},
		{"agy_pretool_deny", agyPreToolUseSchema, preToolDeny(trackgate.ClientGemini, "Gemini may not write code in work repos: /repo/app.py")},
		{"agy_pretool_deny_empty_reason", agyPreToolUseSchema, preToolDeny(trackgate.ClientGemini, "")},
		{"agy_pretool_nested_agent", agyPreToolUseSchema, preToolDeny(trackgate.ClientGemini, nestedAgentReason)},
		// agy PreInvocation (staypoint hook prompt).
		{"agy_prompt_empty", agyPreInvocationSchema, promptHookOutput(true, nil)},
		{"agy_prompt_notices", agyPreInvocationSchema, promptHookOutput(true, notices)},
	}
}

func TestHookContract_GoldenOutputsMatchClientSchemas(t *testing.T) {
	dir := filepath.Join("testdata", "hook_contract")
	cases := hookContractCases(t)
	seen := map[string]bool{}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.schema(c.out); err != nil {
				t.Fatalf("schema violation: %v\noutput: %s", err, c.out)
			}
			got := strings.TrimSpace(c.out) + "\n"
			path := filepath.Join(dir, c.name+".golden.json")
			if *updateHookGolden {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("missing golden (run go test -run TestHookContract -update-hook-golden): %v", err)
			}
			if got != string(want) {
				t.Errorf("output drifted from %s\n got: %s\nwant: %s", path, got, want)
			}
		})
		seen[c.name+".golden.json"] = true
	}
	// A golden with no case means a response was dropped from the table.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !seen[e.Name()] {
			t.Errorf("stale golden %s has no case", e.Name())
		}
	}
}

// The tracking gate's real block paths: reasons hold temp paths, so these are
// schema-checked, not pinned.
func TestHookContract_TrackingGateOutputs(t *testing.T) {
	work, _ := trackingEnv(t)
	gate := productionTrackingGate()
	for name, tc := range map[string]struct {
		raw    []byte
		format string
		schema hookSchema
	}{
		"claude edit":       {claudePayload("Edit", map[string]string{"file_path": filepath.Join(work, "a.go")}, work, "s"), "auto", claudePreToolUseSchema},
		"claude git commit": {claudePayload("Bash", map[string]string{"command": "git commit -m x"}, work, "s"), "claude", claudePreToolUseSchema},
		"agy write":         {agyCall("write_to_file", map[string]any{"TargetFile": filepath.Join(work, "main.go")}, work), "auto", agyPreToolUseSchema},
		"agy run_command":   {agyCall("run_command", map[string]any{"CommandLine": "git push", "Cwd": work}, work), "gemini", agyPreToolUseSchema},
		"agy read":          {agyCall("view_file", map[string]any{"AbsolutePath": filepath.Join(work, "a.go")}, work), "gemini", agyPreToolUseSchema},
		"agy unparseable":   {[]byte(`not json`), "gemini", agyPreToolUseSchema},
		"claude read":       {claudePayload("Read", map[string]string{"file_path": filepath.Join(work, "a.go")}, work, "s"), "auto", claudePreToolUseSchema},
	} {
		client, out, blocked := runTrackingGate(tc.raw, tc.format, noEnv, gate)
		if !blocked {
			// handleHookPreTool prints the allow for the client it parsed.
			out = preToolAllowJSON(client)
		}
		if err := tc.schema(out); err != nil {
			t.Errorf("%s (blocked=%v): %v\noutput: %s", name, blocked, err, out)
		}
	}
}

// The validators must reject the shapes that broke clients in practice, or a
// passing golden test proves nothing.
func TestHookContract_SchemasRejectKnownBadShapes(t *testing.T) {
	for _, c := range []struct {
		name   string
		schema hookSchema
		out    string
	}{
		{"#250 agy bare {}", agyPreToolUseSchema, `{}`},
		{"agy empty deny reason", agyPreToolUseSchema, `{"decision":"deny","reason":" "}`},
		{"agy claude-style block", agyPreToolUseSchema, `{"decision":"block","reason":"x"}`},
		{"agy snake_case key", agyPreToolUseSchema, `{"decision":"allow","permission_overrides":[]}`},
		{"agy prompt snake_case steps", agyPreInvocationSchema, `{"inject_steps":[{"ephemeralMessage":"x"}]}`},
		{"agy prompt snake_case step", agyPreInvocationSchema, `{"injectSteps":[{"ephemeral_message":"x"}]}`},
		{"agy prompt two step types", agyPreInvocationSchema, `{"injectSteps":[{"ephemeralMessage":"x","userMessage":"y"}]}`},
		{"claude prompt top-level additionalContext", claudeUserPromptSubmitSchema, `{"additionalContext":"x"}`},
		{"claude prompt wrong event", claudeUserPromptSubmitSchema, `{"hookSpecificOutput":{"hookEventName":"PreToolUse","additionalContext":"x"}}`},
		{"claude prompt agy shape", claudeUserPromptSubmitSchema, `{"injectSteps":[{"ephemeralMessage":"x"}]}`},
		{"claude pretool agy allow", claudePreToolUseSchema, `{"decision":"allow"}`},
		{"claude pretool bad permission", claudePreToolUseSchema, `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"block"}}`},
		{"claude pretool missing event", claudePreToolUseSchema, `{"hookSpecificOutput":{"permissionDecision":"deny","permissionDecisionReason":"x"}}`},
		{"claude pretool deny no reason", claudePreToolUseSchema, `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny"}}`},
		{"claude pretool disagreeing forms", claudePreToolUseSchema, `{"decision":"block","reason":"x","hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow"}}`},
		{"not an object", claudePreToolUseSchema, `"allow"`},
		{"two objects", agyPreToolUseSchema, `{"decision":"allow"}{"decision":"deny"}`},
	} {
		if err := c.schema(c.out); err == nil {
			t.Errorf("%s: schema accepted %s", c.name, c.out)
		}
	}
}

// Every response handleHookPreTool can print to agy goes through
// preToolAllowJSON or preToolDeny: the Claude-only helpers (preToolAllow,
// preToolBlock, a literal "{}") must not appear before it returns for Gemini.
func TestHookContract_PreToolGeminiPathUsesClientRenderers(t *testing.T) {
	src, err := os.ReadFile("hook_cmd.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	start := strings.Index(s, "func handleHookPreTool() {")
	end := strings.Index(s, "client == trackgate.ClientGemini || hookPreToolTrackingOnly")
	if start < 0 || end < start {
		t.Fatal("handleHookPreTool Gemini branch not found; update this test with the hook")
	}
	for _, bad := range []string{"preToolAllow()", "preToolBlock(", `fmt.Println("{}")`, "claudeBlockJSON("} {
		if strings.Contains(s[start:end], bad) {
			t.Errorf("handleHookPreTool reaches %s before the Gemini return; agy needs preToolAllowJSON/preToolDeny", bad)
		}
	}
	// No hook output in these files is a hand-written {} any more.
	for _, f := range []string{"hook_cmd.go", "hook_tracking.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), `fmt.Println("{}")`) {
			t.Errorf(`%s prints a literal "{}"; render it through the client-aware helpers`, f)
		}
	}
}
