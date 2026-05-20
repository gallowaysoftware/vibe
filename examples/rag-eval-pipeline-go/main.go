// Command rag-eval-pipeline-go is the Go-DSL port of the YAML pipeline at
// examples/rag-eval-pipeline/pipeline.yaml. Same DAG, same templates,
// same outputs — built with the github.com/gallowaysoftware/vibe/vamp
// package's fluent builders instead of yaml.
//
// Usage:
//
//	go run . validate
//	go run . viz --format mermaid
//	go run . run --input collection=<qdrant-collection-name>
//
// Or build a binary and ship the pipeline as that binary:
//
//	go build -o rag-eval .
//	./rag-eval run --input collection=docs
package main

import "github.com/gallowaysoftware/vibe/vamp"

func main() { vamp.Main(Pipeline) }
