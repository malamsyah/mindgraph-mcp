package embeddings

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	defaultVoyageEndpoint = "https://api.voyageai.com/v1/embeddings"
	defaultMaxRetries     = 3
)

// VoyageClient calls Voyage AI's embedding endpoint with bounded
// exponential-backoff retries for transient errors (5xx / network).
type VoyageClient struct {
	apiKey     string
	model      string
	dimensions int
	endpoint   string
	httpClient *http.Client
	maxRetries int
	backoff    time.Duration
}

// NewVoyageClient returns a configured Voyage client. apiKey may be empty for
// dry-run usage in tests; Embed will then return an error rather than silently
// returning empty vectors.
func NewVoyageClient(apiKey, model string, dimensions int, httpClient *http.Client) *VoyageClient {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	if model == "" {
		model = "voyage-3-large"
	}
	if dimensions <= 0 {
		dimensions = 2048
	}
	return &VoyageClient{
		apiKey:     apiKey,
		model:      model,
		dimensions: dimensions,
		endpoint:   defaultVoyageEndpoint,
		httpClient: httpClient,
		maxRetries: defaultMaxRetries,
		backoff:    250 * time.Millisecond,
	}
}

// WithEndpoint overrides the default Voyage endpoint (used in tests).
func (c *VoyageClient) WithEndpoint(endpoint string) *VoyageClient {
	c.endpoint = endpoint
	return c
}

// WithBackoff overrides the initial retry backoff (used in tests).
func (c *VoyageClient) WithBackoff(initial time.Duration) *VoyageClient {
	c.backoff = initial
	return c
}

type voyageReq struct {
	Input           []string `json:"input"`
	Model           string   `json:"model"`
	InputType       string   `json:"input_type,omitempty"`
	OutputDimension int      `json:"output_dimension,omitempty"`
}

type voyageRespItem struct {
	Embedding []float32 `json:"embedding"`
	Index     int       `json:"index"`
}

type voyageResp struct {
	Data []voyageRespItem `json:"data"`
}

func (c *VoyageClient) Embed(ctx context.Context, texts []string, inputType InputType) ([][]float32, error) {
	if c.apiKey == "" {
		return nil, errors.New("voyage: missing api key")
	}
	if len(texts) == 0 {
		return nil, nil
	}
	body := voyageReq{
		Input:           texts,
		Model:           c.model,
		InputType:       string(inputType),
		OutputDimension: c.dimensions,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("voyage marshal: %w", err)
	}

	backoff := c.backoff
	var lastErr error
	for attempt := 0; attempt < c.maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
		if err != nil {
			return nil, fmt.Errorf("voyage build request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("voyage http: %w", err)
			continue
		}
		out, err := decodeVoyageResp(resp)
		_ = resp.Body.Close()
		if err == nil {
			return out, nil
		}
		lastErr = err
		// Only retry on 5xx and transport errors.
		if !shouldRetry(resp.StatusCode) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("voyage exhausted retries: %w", lastErr)
}

func decodeVoyageResp(resp *http.Response) ([][]float32, error) {
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("voyage status %d: %s", resp.StatusCode, string(body))
	}
	var parsed voyageResp
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("voyage decode: %w", err)
	}
	out := make([][]float32, len(parsed.Data))
	for _, item := range parsed.Data {
		if item.Index < 0 || item.Index >= len(out) {
			return nil, fmt.Errorf("voyage: out-of-range index %d", item.Index)
		}
		out[item.Index] = item.Embedding
	}
	return out, nil
}

func shouldRetry(status int) bool {
	return status >= 500 || status == http.StatusTooManyRequests
}
