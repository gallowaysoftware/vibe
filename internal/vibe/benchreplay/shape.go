package benchreplay

import (
	"encoding/json"
	"strings"
)

// The boundary. Bodies become shapes HERE, once, and nothing downstream
// can reach a []byte.
//
// Everything in this file takes bytes and returns structure. The structure
// is closed: booleans, counts, and strings drawn from enumerated sets. A
// tool name survives as far as `shape`, because two responses agreeing on
// WHICH tool they called is the signal, and it goes no further — Report
// carries the agreement, never the name.

// The closed set of tool outcomes. These three strings, and no others,
// reach a rendered report.
const (
	// ToolNone: the response called no tool.
	ToolNone = "none"
	// ToolDeclared: it called one the request's own `tools` array names.
	ToolDeclared = "declared"
	// ToolUndeclared: it called something else. The name is NOT echoed —
	// a hallucinated tool name is model output, which is to say private
	// traffic, and the marker is the whole report.
	ToolUndeclared = "<undeclared>"
)

// The closed set of finish reasons. llama.cpp and every OpenAI-shaped
// server put a free string here, so it is NORMALISED rather than carried:
// an unrecognised value is model-adjacent text and reaches nothing.
const (
	FinishStop      = "stop"
	FinishLength    = "length"
	FinishToolCalls = "tool_calls"
	FinishFilter    = "content_filter"
	FinishOther     = "other"
	FinishNone      = "none"
)

func normalizeFinish(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return FinishNone
	case "stop", "eos", "stop_sequence", "end_turn":
		return FinishStop
	case "length", "max_tokens":
		return FinishLength
	case "tool_calls", "function_call", "tool_use":
		return FinishToolCalls
	case "content_filter":
		return FinishFilter
	default:
		// Everything else is OTHER. Passing an unknown value through would
		// put a string the model wrote onto the operator's terminal, and
		// would give the Report an open-set field.
		return FinishOther
	}
}

// requestFacts is the ground truth for scoring, and all of it comes from
// the REQUEST — which declares the tools, their schemas and the token
// budget. That is why the recorded response is demoted to a control in
// §5: everything that matters is computable without it.
type requestFacts struct {
	declaresTools bool
	toolNames     map[string]bool
	// requiredArgs maps a declared tool to the keys its schema marks
	// required. Empty for a tool that declares none.
	requiredArgs map[string][]string
	maxTokens    int
	wantsSchema  bool
}

// shape is one response, reduced. It is the only thing the scorer sees.
type shape struct {
	status int
	// known is false when there was no response to reduce at all — a
	// non-200 recorded row (llama-swap stores the request but never the
	// response body for those), an unparseable body, a transport failure.
	// It is NOT a shape full of zeros: absent evidence is not a healthy
	// value, and a zero-valued shape would score as "answered nothing,
	// called no tool, finished with none" — three claims nobody measured.
	known bool

	hasToolCall  bool
	toolName     string
	toolOutcome  string // ToolNone / ToolDeclared / ToolUndeclared
	argsValid    bool
	argsRequired bool
	finish       string
	outTokens    int
	empty        bool
	truncated    bool
	// schemaWanted/schemaOK cover response_format: json_schema.
	schemaWanted bool
	schemaOK     bool
}

// capturePayload mirrors llama-swap's server.ReqRespCapture. []byte
// decodes from the base64 strings the handler emits.
type capturePayload struct {
	ID          int               `json:"id"`
	ReqPath     string            `json:"req_path"`
	ReqBody     []byte            `json:"req_body"`
	RespBody    []byte            `json:"resp_body"`
	RespHeaders map[string]string `json:"resp_headers"`
}

// extractRequest reads the ground truth out of a captured request body.
//
// It reads only the fields §5 needs and never retains the prose. A body
// that will not parse yields ok=false, which the harvest counts as
// `malformed` and drops: a request nobody could read is not a request that
// declared no tools.
func extractRequest(body []byte) (requestFacts, bool) {
	var req struct {
		MaxTokens        int             `json:"max_tokens"`
		MaxCompletionTok int             `json:"max_completion_tokens"`
		ResponseFormat   json.RawMessage `json:"response_format"`
		Tools            []struct {
			Function struct {
				Name       string `json:"name"`
				Parameters struct {
					Required []string `json:"required"`
				} `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return requestFacts{}, false
	}
	f := requestFacts{
		toolNames:    map[string]bool{},
		requiredArgs: map[string][]string{},
		maxTokens:    req.MaxTokens,
	}
	if f.maxTokens == 0 {
		f.maxTokens = req.MaxCompletionTok
	}
	for _, t := range req.Tools {
		if t.Function.Name == "" {
			continue
		}
		f.declaresTools = true
		f.toolNames[t.Function.Name] = true
		f.requiredArgs[t.Function.Name] = append([]string(nil), t.Function.Parameters.Required...)
	}
	if len(req.ResponseFormat) > 0 && string(req.ResponseFormat) != "null" {
		var rf struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(req.ResponseFormat, &rf) == nil {
			f.wantsSchema = rf.Type == "json_schema" || rf.Type == "json_object"
		}
	}
	return f, true
}

// chatResponse is the subset of a non-streaming chat completion the scorer
// reads. Content is decoded ONLY to ask whether it is empty and whether it
// parses as JSON when a schema was requested; it is never stored on a
// shape and never rendered.
type chatResponse struct {
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Content   json.RawMessage `json:"content"`
			ToolCalls []struct {
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Timings struct {
		PredictedN         float64 `json:"predicted_n"`
		PredictedMS        float64 `json:"predicted_ms"`
		PredictedPerSecond float64 `json:"predicted_per_second"`
	} `json:"timings"`
}

// shapeOf reduces a non-streaming chat completion against the facts its
// request declared.
func shapeOf(status int, body []byte, f requestFacts) shape {
	s := shape{status: status, toolOutcome: ToolNone, finish: FinishNone}
	var r chatResponse
	if err := json.Unmarshal(body, &r); err != nil || len(r.Choices) == 0 {
		return s // known stays false
	}
	s.known = true
	s.outTokens = r.Usage.CompletionTokens
	c := r.Choices[0]
	s.finish = normalizeFinish(c.FinishReason)
	s.empty = len(c.Message.ToolCalls) == 0 && isEmptyContent(c.Message.Content)
	// Truncation is measured against the REQUEST's own budget, which is
	// the only budget that was ever in force for this sample.
	s.truncated = s.finish == FinishLength || (f.maxTokens > 0 && s.outTokens >= f.maxTokens)
	if len(c.Message.ToolCalls) > 0 {
		tc := c.Message.ToolCalls[0]
		s.hasToolCall = true
		s.toolName = tc.Function.Name
		s.toolOutcome = ToolUndeclared
		if f.toolNames[tc.Function.Name] {
			s.toolOutcome = ToolDeclared
		}
		s.argsValid, s.argsRequired = checkArgs(tc.Function.Arguments, f.requiredArgs[tc.Function.Name])
	}
	if f.wantsSchema {
		s.schemaWanted = true
		s.schemaOK = isJSONObject(contentString(c.Message.Content))
	}
	return s
}

// shapeOfRecorded reduces a RECORDED response, which may be a buffered SSE
// frame stream rather than one JSON object.
//
// Streaming text is deliberately not reassembled: llama.cpp's chunk
// boundaries are not a stable contract, and this package compares
// structure and never text. Tool-call ARGUMENT fragments are a different
// matter — concatenation is the documented OpenAI streaming contract and
// every client performs it — so they are joined, which is what lets the
// noise floor mean anything on a fleet whose rows are all
// text/event-stream.
func shapeOfRecorded(status int, contentType string, body []byte, f requestFacts) shape {
	if status != 0 && status != 200 {
		// llama-swap stores the request but never the response body for a
		// non-200 (metrics.go's recorder.Status() != 200 branch). The most
		// interesting requests in a sample are the ones with nothing to
		// compare against; they are counted, never guessed at.
		return shape{status: status, toolOutcome: ToolNone, finish: FinishNone}
	}
	if isSSE(contentType, body) {
		return shapeOfStream(status, body, f)
	}
	return shapeOf(status, body, f)
}

// isSSE decides which reducer a recorded body needs.
//
// The capture carries `resp_headers`, and content-type is the discriminator
// llama-swap already put on the wire — the same field C7a keys
// resp_content_type on. It is used FIRST, because the fallback is a
// heuristic and a heuristic that ran first would misroute an ordinary JSON
// completion whose content happens to contain the characters "data:" (a
// data URI, a pasted log line, the words "data: see below"). Such a body
// finds no line beginning with `data:` in the stream reducer, comes back
// UNKNOWN, and silently leaves the divergence denominator — shrinking the
// n of a figure that is already a proportion.
//
// The fallback exists because a capture may carry no headers at all, and
// it matches on a line PREFIX rather than a substring for the same reason.
func isSSE(contentType string, body []byte) bool {
	if ct := strings.ToLower(contentType); ct != "" {
		return strings.Contains(ct, "event-stream")
	}
	head := body
	if len(head) > 4096 {
		head = head[:4096]
	}
	for _, line := range strings.SplitN(string(head), "\n", 64) {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		return strings.HasPrefix(t, "data:")
	}
	return false
}

// shapeOfStream folds an SSE capture into the same shape a JSON response
// yields.
func shapeOfStream(status int, body []byte, f requestFacts) shape {
	s := shape{status: status, toolOutcome: ToolNone, finish: FinishNone}
	var (
		argsB    strings.Builder
		toolName string
		sawTool  bool
		sawText  bool
		sawAny   bool
	)
	for _, line := range strings.Split(string(body), "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "data:")
		if !ok {
			continue
		}
		rest = strings.TrimSpace(rest)
		if rest == "" || rest == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				FinishReason string `json:"finish_reason"`
				Delta        struct {
					Content   json.RawMessage `json:"content"`
					ToolCalls []struct {
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
			Usage struct {
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(rest), &chunk) != nil {
			continue
		}
		sawAny = true
		if chunk.Usage.CompletionTokens > 0 {
			s.outTokens = chunk.Usage.CompletionTokens
		}
		for _, c := range chunk.Choices {
			if c.FinishReason != "" {
				s.finish = normalizeFinish(c.FinishReason)
			}
			if !isEmptyContent(c.Delta.Content) {
				sawText = true
			}
			for _, tc := range c.Delta.ToolCalls {
				sawTool = true
				if tc.Function.Name != "" {
					toolName = tc.Function.Name
				}
				argsB.WriteString(tc.Function.Arguments)
			}
		}
	}
	if !sawAny {
		return s // known stays false: nothing in this capture parsed
	}
	s.known = true
	s.empty = !sawTool && !sawText
	s.truncated = s.finish == FinishLength || (f.maxTokens > 0 && s.outTokens >= f.maxTokens)
	if sawTool {
		s.hasToolCall = true
		s.toolName = toolName
		s.toolOutcome = ToolUndeclared
		if f.toolNames[toolName] {
			s.toolOutcome = ToolDeclared
		}
		s.argsValid, s.argsRequired = checkArgs(argsB.String(), f.requiredArgs[toolName])
	}
	// Schema conformance is deliberately NOT answered for a streamed
	// recorded response. Judging it needs the text reassembled and §5
	// refuses to reassemble streamed text, so the question is DROPPED
	// rather than answered "did not conform" — an unanswerable question
	// must not become a failure nobody measured.
	return s
}

// checkArgs asks the two structural questions §5 names: does the arguments
// blob parse as JSON, and does it carry the keys the request's own schema
// marked required.
func checkArgs(args string, required []string) (valid, hasRequired bool) {
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(args), &obj) != nil {
		return false, false
	}
	for _, k := range required {
		if _, ok := obj[k]; !ok {
			return true, false
		}
	}
	return true, true
}

func isEmptyContent(raw json.RawMessage) bool {
	return strings.TrimSpace(contentString(raw)) == ""
}

// contentString unwraps the two content encodings without keeping the
// text: OpenAI's plain string and the content-parts array. The return
// value is consumed on the same line by an emptiness or JSON-validity
// question and is never stored.
func contentString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return string(raw)
}

func isJSONObject(s string) bool {
	var obj map[string]json.RawMessage
	return json.Unmarshal([]byte(strings.TrimSpace(s)), &obj) == nil
}

// disagree reports STRUCTURAL disagreement between two responses. It is
// deliberately not text similarity: "same tool, different arguments" is a
// real signal and "different wording" is not one, so no edit distance, no
// embedding, no LLM judge.
//
// Two shapes where either is unknown do not disagree — they are not
// compared at all, and the caller counts them out of n.
func disagree(a, b shape) bool {
	switch {
	case a.hasToolCall != b.hasToolCall:
		return true
	case a.hasToolCall && a.toolName != b.toolName:
		return true
	case a.hasToolCall && a.argsValid != b.argsValid:
		return true
	case a.finish != b.finish:
		return true
	case a.empty != b.empty:
		return true
	}
	return false
}
