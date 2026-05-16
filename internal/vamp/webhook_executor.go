package vamp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

// webhookExecutor implements StageExecutor for type: webhook stages. It
// renders the URL, method, headers, and body templates, POSTs the resulting
// JSON to a Slack/Discord/Mattermost-compatible incoming webhook URL, and
// records the response body in the stage's output file.
//
// Webhooks are the canonical "final stage that announces completion"
// primitive — they're a thin wrapper around net/http with template rendering
// glued on, so we deliberately don't pull in any extra dependencies. The
// executor talks plain HTTPS over the injectable httpDoer, which means tests
// stub every call without spinning up a real server (though we use
// httptest.Server in the test file because it's still the clearest way to
// assert on the resulting request shape).
type webhookExecutor struct {
	// doer is the injectable HTTP transport. nil means use http.DefaultClient.
	// Tests stub this to capture the rendered request without making a real
	// network call.
	doer httpDoer
}

// Compile-time guarantee that webhookExecutor satisfies StageExecutor.
var _ StageExecutor = (*webhookExecutor)(nil)

// maxWebhookErrorBody bounds how much of a non-2xx response body we surface
// in the returned error. 1 KiB is enough to identify the failure mode
// (rate-limit JSON, Slack's plain "invalid_payload" body, an HTML error page
// preview) without flooding logs.
const maxWebhookErrorBody = 1024

// Execute renders the stage's templated fields, fires the HTTP request, and
// writes the response body to <RunDir>/<rendered output>.
//
// Body rendering: body and body_template_file are mutually exclusive
// (Validate already enforced this). The inline body map is rendered leaf-by-
// leaf as a Go template, then JSON-marshaled. body_template_file's contents
// are rendered as a single template and sent verbatim (so the user can
// produce any JSON shape they want without us round-tripping through Go
// types).
//
// Retry-on-5xx coupling: the stage may declare its own retry: block, in
// which case we leave that to the runner's runWithRetry wrapper untouched.
// When retry_on_5xx is true (default) AND the stage has no explicit retry
// policy, Pipeline.Validate synthesises a default retry policy with
// transient-mode classification — so this executor just has to produce an
// error string the runner's isRetryable() heuristic recognises (the
// literal "HTTP 5xx" status form matches the http5xxRE regex).
func (w *webhookExecutor) Execute(ctx context.Context, in StageInput) (*StageOutput, error) {
	st := in.Stage
	if st == nil {
		return nil, errors.New("webhook: missing stage")
	}

	// Foreach binding goes into every template render so users can fan out
	// notifications (e.g. one per recipient).
	var extra map[string]any
	if st.Foreach != nil {
		extra = map[string]any{st.Foreach.Var: in.Item, "i": in.ItemIdx}
	}

	urlStr, err := renderWebhookTemplate(st, "url", st.URL, in, extra)
	if err != nil {
		return nil, fmt.Errorf("stage %s: render url: %w", st.ID, err)
	}
	if strings.TrimSpace(urlStr) == "" {
		return nil, fmt.Errorf("stage %s: rendered url is empty", st.ID)
	}

	method := strings.ToUpper(st.Method)
	if method == "" {
		method = http.MethodPost
	}

	// Render the body. Inline body wins per validation; otherwise pull from
	// the template file.
	var bodyBytes []byte
	switch {
	case len(st.Body) > 0:
		rendered, err := renderWebhookMap(st, "body", st.Body, in, extra)
		if err != nil {
			return nil, fmt.Errorf("stage %s: render body: %w", st.ID, err)
		}
		// Marshal AFTER template rendering so users see the same JSON the
		// server sees: {"text":"Pipeline X done"} not Go's
		// map-fmt representation.
		marshaled, err := json.Marshal(rendered)
		if err != nil {
			return nil, fmt.Errorf("stage %s: marshal body: %w", st.ID, err)
		}
		bodyBytes = marshaled
	case st.BodyTemplateFile != "":
		path := st.BodyTemplateFile
		if !filepath.IsAbs(path) {
			path = filepath.Join(in.PipelineDir, path)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("stage %s: read body_template_file %s: %w", st.ID, path, err)
		}
		rendered, err := renderWebhookTemplate(st, "body_template_file", string(raw), in, extra)
		if err != nil {
			return nil, fmt.Errorf("stage %s: render body_template_file: %w", st.ID, err)
		}
		bodyBytes = []byte(rendered)
	}

	// Render headers BEFORE building the request so a template error doesn't
	// leak a half-constructed request to the caller.
	renderedHeaders := make(map[string]string, len(st.Headers))
	for k, v := range st.Headers {
		val, err := renderWebhookTemplate(st, "header:"+k, v, in, extra)
		if err != nil {
			return nil, fmt.Errorf("stage %s: render header %q: %w", st.ID, k, err)
		}
		renderedHeaders[k] = val
	}

	// GET requests typically don't carry bodies. We still allow it (some
	// custom webhook endpoints accept a body on non-POST) but only set the
	// Content-Type when there IS a body, so a plain GET ping doesn't lie to
	// the server about what it's sending.
	var reqBody io.Reader
	if len(bodyBytes) > 0 {
		reqBody = bytes.NewReader(bodyBytes)
	}
	req, err := http.NewRequestWithContext(ctx, method, urlStr, reqBody)
	if err != nil {
		return nil, fmt.Errorf("stage %s: build request: %w", st.ID, err)
	}
	if len(bodyBytes) > 0 && method != http.MethodGet {
		// Default Content-Type for the inline-body case is JSON. The
		// body_template_file path also produces JSON by convention (the
		// example/README is explicit about this), so the same default is
		// reasonable. Users who need a different content type can set it via
		// headers; user-provided headers overwrite this default below.
		req.Header.Set("Content-Type", "application/json")
		req.ContentLength = int64(len(bodyBytes))
	}
	for k, v := range renderedHeaders {
		req.Header.Set(k, v)
	}

	doer := w.doer
	if doer == nil {
		doer = http.DefaultClient
	}

	if in.Log != nil {
		fmt.Fprintf(in.Log, "webhook: %s %s\n", method, urlStr)
	}
	resp, err := doer.Do(req)
	if err != nil {
		return nil, fmt.Errorf("stage %s: request: %w", st.ID, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		preview := respBody
		if len(preview) > maxWebhookErrorBody {
			preview = preview[:maxWebhookErrorBody]
		}
		// The 5xx-vs-other split is what the runner's transient-error
		// classifier keys on (via http5xxRE). When retry_on_5xx is true and
		// the user didn't set their own retry policy we want the runner to
		// treat this as transient — see the caller in exec.go's executor
		// registration for the policy injection that makes that happen.
		return nil, fmt.Errorf("stage %s: webhook returned HTTP %d: %s", st.ID, resp.StatusCode, strings.TrimSpace(string(preview)))
	}

	// Return the response body as StageOutput.Text. The runner's
	// executeStage path persists this to <RunDir>/<rendered output> the
	// same way it handles text-stage completions — same as how the
	// youtube stage records the watch URL — so the executor doesn't
	// own the write path.
	return &StageOutput{Text: string(respBody)}, nil
}

// renderWebhookTemplate is a small wrapper that mirrors the package-level
// renderTemplate but adds the webhook-only template helpers:
//   - {{ .pipeline_name }} — the parent Pipeline.Name
//   - {{ env "VAR" }} — read an env var (returns "" when unset). Note
//     this is the FuncMap-style call without a leading dot: Go's
//     text/template engine cannot invoke function-valued map entries with
//     arguments via `.field "arg"` syntax (map values aren't callable),
//     so the canonical webhook env-var lookup uses the bare `env` FuncMap
//     identifier. The example README documents this.
func renderWebhookTemplate(st *Stage, name, raw string, in StageInput, extra map[string]any) (string, error) {
	deps := st.Inputs
	tmpl, err := template.New(st.ID + ":" + name).
		Option("missingkey=error").
		Funcs(templateFuncs()).
		Funcs(template.FuncMap{
			"env": webhookEnvFunc,
		}).
		Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse template: %w", err)
	}
	stages := make(map[string]map[string]any, len(deps))
	for _, dep := range deps {
		res, ok := in.Prior[dep]
		if !ok {
			return "", fmt.Errorf("input %q has no output yet (scheduler bug or unresolved dependency)", dep)
		}
		stages[dep] = map[string]any{
			"output":  res.Output,
			"outputs": res.Outputs,
		}
	}
	data := map[string]any{
		"inputs":        in.Inputs,
		"stages":        stages,
		"runDir":        in.RunDir,
		"pipeline_name": in.PipelineName,
	}
	for k, v := range extra {
		data[k] = v
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// renderWebhookMap walks an inline body map and renders every string leaf as
// a Go template, leaving non-string leaves alone (so users can use bools and
// numbers directly). Nested maps and slices are recursed.
func renderWebhookMap(st *Stage, name string, src map[string]any, in StageInput, extra map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(src))
	for k, v := range src {
		rendered, err := renderWebhookValue(st, name+"."+k, v, in, extra)
		if err != nil {
			return nil, err
		}
		out[k] = rendered
	}
	return out, nil
}

// renderWebhookValue dispatches on the concrete type to render strings via
// the template engine while preserving the original shape for numbers /
// bools / nested containers. YAML decodes into:
//   - string -> string (template-rendered)
//   - bool / int / float -> as-is
//   - map[string]any -> recurse
//   - map[any]any -> recurse (yaml.v3 occasionally emits this when keys are
//     ambiguously typed; we coerce to map[string]any so JSON marshal sees
//     a stable shape — JSON rejects map[any]any with a clear error and we
//     want body rendering to succeed silently)
//   - []any -> recurse element-by-element
func renderWebhookValue(st *Stage, name string, v any, in StageInput, extra map[string]any) (any, error) {
	switch tv := v.(type) {
	case string:
		return renderWebhookTemplate(st, name, tv, in, extra)
	case map[string]any:
		return renderWebhookMap(st, name, tv, in, extra)
	case map[any]any:
		coerced := make(map[string]any, len(tv))
		for k, val := range tv {
			coerced[fmt.Sprint(k)] = val
		}
		return renderWebhookMap(st, name, coerced, in, extra)
	case []any:
		out := make([]any, len(tv))
		for i, item := range tv {
			rendered, err := renderWebhookValue(st, fmt.Sprintf("%s[%d]", name, i), item, in, extra)
			if err != nil {
				return nil, err
			}
			out[i] = rendered
		}
		return out, nil
	default:
		// Everything else (bool, int, float64, nil) passes through verbatim.
		return v, nil
	}
}

// webhookEnvFunc reads an env var, returning "" when unset. Exposed as
// `env` in the webhook template FuncMap. Defined as a named function (not
// an anonymous closure registered inline) so the FuncMap entry has a
// stable identity per call and so the godoc surfaces the semantics.
func webhookEnvFunc(name string) string {
	return os.Getenv(name)
}
