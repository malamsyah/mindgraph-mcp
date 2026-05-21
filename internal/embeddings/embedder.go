package embeddings

import "context"

// InputType signals to the embedding provider whether the text being embedded
// is a corpus document (to be stored & indexed) or a query (to be matched
// against existing documents). The two often use slightly different prompts
// internally for asymmetric retrieval.
type InputType string

const (
	InputDocument InputType = "document"
	InputQuery    InputType = "query"
)

// Embedder is the abstraction over an embedding provider (Voyage AI in v1).
// Implementations should retry transient failures and return a final error
// only after exhausting retries.
type Embedder interface {
	Embed(ctx context.Context, texts []string, inputType InputType) ([][]float32, error)
}
