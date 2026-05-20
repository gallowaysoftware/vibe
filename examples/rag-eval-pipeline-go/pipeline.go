package main

import (
	"embed"
	"io/fs"

	"github.com/gallowaysoftware/vibe/vamp"
)

//go:embed prompts/*.tmpl body_templates/*.tmpl
var assets embed.FS

// promptsFS / bodyTemplatesFS narrow `assets` to the matching subdirectory
// so the Stage.AssetFS roots resolve a name like "judge.tmpl" cleanly
// without a "prompts/" prefix in every builder call.
var (
	promptsFS       fs.FS = mustSub(assets, "prompts")
	bodyTemplatesFS fs.FS = mustSub(assets, "body_templates")
)

func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}
	return sub
}

// Pipeline returns the rag-eval pipeline as a Go-DSL construction. Mirror
// of examples/rag-eval-pipeline/pipeline.yaml — same DAG, same templates,
// same outputs.
func Pipeline() (*vamp.Pipeline, error) {
	p := vamp.New("rag-eval").
		Describe("Drives the rag profile's RAG stack end-to-end: embeds a list of test queries with TEI, retrieves top-k chunks from a Qdrant collection, asks the LLM to score retrieval quality per query, and synthesizes a final report.").
		Input("collection", vamp.Required())

	// 1. Materialize the query suite as JSON. Deterministic echo prompt +
	//    temperature 0 + seed 1 so foreach has a known array to iterate.
	queries := p.Text("queries").
		Capability("fast_drafting").
		Prompt("Output ONLY this JSON array of strings verbatim, with no preamble\nand no Markdown formatting:\n[\n  \"What does BGE-M3 do?\",\n  \"How does Qdrant index vectors?\",\n  \"What is hybrid search and why does it help RAG?\"\n]\n").
		Output("queries.json").
		OutputFormatJSON().
		Param("max_tokens", 512).
		Param("temperature", 0).
		Param("seed", 1)

	// 2. Embed each query via TEI.
	embedStage := p.Webhook("embed").
		After(queries).
		Foreach(queries, "query").
		URL("http://127.0.0.1:14002/v1/embeddings").
		Method("POST").
		Body(map[string]any{
			"model": "bge-m3",
			"input": "{{.query}}",
		}).
		Output("embed_{{.i}}.json")

	// 3. Retrieve top-k chunks from Qdrant; the body needs the vector
	//    inlined as a real JSON array, not a stringified one, so we use
	//    a body template file rendered through readFile / parseJSON /
	//    toJSON.
	retrieve := p.Webhook("retrieve").
		After(embedStage, queries).
		Foreach(queries, "query").
		URL("http://127.0.0.1:6333/collections/{{.inputs.collection}}/points/search").
		Method("POST").
		BodyTemplateFS(bodyTemplatesFS, "qdrant_search.json.tmpl").
		Output("retrieve_{{.i}}.json")

	// 4. Per-query judge: LLM scores retrieval relevance against the
	//    original query, structured JSON the report stage aggregates.
	judge := p.Text("judge").
		Capability("careful_reasoning").
		After(retrieve, queries).
		Foreach(queries, "query").
		PromptFS(promptsFS, "judge.tmpl").
		Output("judge_{{.i}}.json").
		OutputFormatJSON().
		Param("max_tokens", 1024).
		Param("temperature", 0).
		Param("seed", 1)

	// 5. Aggregate across all per-query judgments into a Markdown report.
	p.Text("report").
		Capability("careful_reasoning").
		After(judge).
		PromptFS(promptsFS, "report.tmpl").
		Output("report.md").
		Param("max_tokens", 4096).
		Param("temperature", 0.2)

	return p.Build()
}
