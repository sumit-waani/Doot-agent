// Package llm wraps the OpenAI-compatible chat completions API.
//
// The model is reached through the official OpenAI Go SDK pointed at a custom
// base URL, since the Meta Model API speaks the same protocol. Everything Doot
// needs is here: streaming, tool calling, and per-call cost accounting.
package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/shared"
)

// Purpose attributes a call to one part of the loop.
//
// This is the field that answers the only cost question worth asking: whether
// the E2E verifier is where the money actually goes.
type Purpose string

const (
	PurposePrimary    Purpose = "primary"
	PurposeReviewer   Purpose = "reviewer"
	PurposeE2E        Purpose = "e2e"
	PurposeCompaction Purpose = "compaction"
)

// ErrNoAPIKey is returned when the model credential has not been set.
var ErrNoAPIKey = errors.New("llm: API key is not set (add it in Settings)")

// ErrNoModel is returned when no model name is configured.
var ErrNoModel = errors.New("llm: model is not set (add it in Settings)")

// Config configures a Client.
type Config struct {
	APIKey  string
	BaseURL string
	Model   string

	// ContextWindow is the model's total token budget, used to decide when to
	// compact.
	ContextWindow int

	MaxOutputTokens int

	// Pricing in USD per million tokens, so cost can be corrected without a
	// redeploy.
	InputPerMtok       float64
	CachedInputPerMtok float64
	OutputPerMtok      float64

	// RequestTimeout bounds a single call. Generous by default: a long tool-using
	// turn can legitimately take minutes.
	RequestTimeout time.Duration

	// MaxRetries covers transient upstream failures.
	MaxRetries int
}

func (c Config) withDefaults() Config {
	if c.ContextWindow <= 0 {
		c.ContextWindow = 200_000
	}
	if c.MaxOutputTokens <= 0 {
		c.MaxOutputTokens = 8192
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = 10 * time.Minute
	}
	if c.MaxRetries <= 0 {
		c.MaxRetries = 2
	}
	return c
}

// Validate reports configuration that would make calls fail.
func (c Config) Validate() error {
	if strings.TrimSpace(c.APIKey) == "" {
		return ErrNoAPIKey
	}
	if strings.TrimSpace(c.Model) == "" {
		return ErrNoModel
	}
	return nil
}

// Message is one entry in the conversation, in Doot's own shape.
//
// Deliberately not the SDK's type: these values are persisted to and loaded
// from the database, and pinning the storage format to a vendor type would make
// an SDK upgrade a migration.
type Message struct {
	Role       string // system | user | assistant | tool
	Content    string
	ToolCalls  []ToolCall
	ToolCallID string // set when Role is "tool"
	Name       string
}

// ToolCall is a model request to run one tool.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string // raw JSON as emitted by the model
}

// Tool is a function the model may call.
type Tool struct {
	Name        string
	Description string
	// Parameters is a raw JSON Schema object.
	Parameters map[string]any
}

// Usage reports token consumption and cost for one call.
type Usage struct {
	PromptTokens       int64
	CachedPromptTokens int64
	CompletionTokens   int64
	CostUSD            float64
}

// Response is the result of one completion.
type Response struct {
	Content      string
	ToolCalls    []ToolCall
	FinishReason string
	Usage        Usage
	LatencyMS    int

	// PromptTokens is repeated here because it drives the compaction decision,
	// and using the API's own count avoids a client-side token estimator that
	// would drift from the model's real accounting.
	PromptTokens int64
}

// ContextUsedPct reports how full the context window is, based on the tokens the
// API actually charged for.
func (r Response) ContextUsedPct(contextWindow int) float64 {
	if contextWindow <= 0 {
		return 0
	}
	return float64(r.PromptTokens) / float64(contextWindow) * 100
}

// Stream receives incremental output. Any field may be nil.
type Stream struct {
	// OnContentDelta fires for each text fragment.
	OnContentDelta func(delta string)
	// OnToolCall fires once a tool call has fully arrived, so the UI can show it
	// before it runs.
	OnToolCall func(call ToolCall)
}

// AuditRecord is one row for the llm_calls table.
type AuditRecord struct {
	Purpose      Purpose
	Model        string
	Usage        Usage
	LatencyMS    int
	FinishReason string
	Err          error
}

// Client talks to the model.
type Client struct {
	cfg Config
	oa  openai.Client

	// audit is called after every completion, successful or not. Passing it in
	// at construction means no caller can forget to record a cost.
	audit func(context.Context, AuditRecord)
}

// New builds a Client. audit may be nil.
func New(cfg Config, audit func(context.Context, AuditRecord)) (*Client, error) {
	cfg = cfg.withDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	opts := []option.RequestOption{
		option.WithAPIKey(cfg.APIKey),
		option.WithRequestTimeout(cfg.RequestTimeout),
		option.WithMaxRetries(cfg.MaxRetries),
	}
	if base := strings.TrimSpace(cfg.BaseURL); base != "" {
		opts = append(opts, option.WithBaseURL(base))
	}

	return &Client{
		cfg:   cfg,
		oa:    openai.NewClient(opts...),
		audit: audit,
	}, nil
}

// Config returns the effective configuration.
func (c *Client) Config() Config { return c.cfg }

// Model returns the configured model name.
func (c *Client) Model() string { return c.cfg.Model }

// ContextWindow returns the configured context budget.
func (c *Client) ContextWindow() int { return c.cfg.ContextWindow }

// Complete runs one streaming completion.
//
// Streaming is always used, even when nothing is watching: it is the only way to
// surface progress live, and the usage totals still arrive in the final chunk.
func (c *Client) Complete(
	ctx context.Context,
	purpose Purpose,
	messages []Message,
	tools []Tool,
	stream *Stream,
) (Response, error) {
	started := time.Now()

	params := openai.ChatCompletionNewParams{
		Model:               shared.ChatModel(c.cfg.Model),
		Messages:            toSDKMessages(messages),
		MaxCompletionTokens: openai.Int(int64(c.cfg.MaxOutputTokens)),
		// Without this the usage totals never arrive on a streamed response, and
		// every cost row would be zero.
		StreamOptions: openai.ChatCompletionStreamOptionsParam{
			IncludeUsage: openai.Bool(true),
		},
	}
	if len(tools) > 0 {
		params.Tools = toSDKTools(tools)
	}

	resp, err := c.runStream(ctx, params, stream)
	resp.LatencyMS = int(time.Since(started).Milliseconds())

	if c.audit != nil {
		c.audit(ctx, AuditRecord{
			Purpose:      purpose,
			Model:        c.cfg.Model,
			Usage:        resp.Usage,
			LatencyMS:    resp.LatencyMS,
			FinishReason: resp.FinishReason,
			Err:          err,
		})
	}

	if err != nil {
		return resp, err
	}
	return resp, nil
}

// runStream consumes the SSE stream and assembles the response.
func (c *Client) runStream(
	ctx context.Context,
	params openai.ChatCompletionNewParams,
	stream *Stream,
) (Response, error) {
	sdkStream := c.oa.Chat.Completions.NewStreaming(ctx, params)
	defer sdkStream.Close()

	acc := openai.ChatCompletionAccumulator{}
	var out Response

	for sdkStream.Next() {
		chunk := sdkStream.Current()
		acc.AddChunk(chunk)

		if len(chunk.Choices) > 0 {
			if delta := chunk.Choices[0].Delta.Content; delta != "" && stream != nil && stream.OnContentDelta != nil {
				stream.OnContentDelta(delta)
			}
		}

		// Surface each tool call as soon as it is complete, rather than after the
		// whole turn, so the UI shows what is about to run.
		if call, ok := acc.JustFinishedToolCall(); ok && stream != nil && stream.OnToolCall != nil {
			stream.OnToolCall(ToolCall{
				ID:        call.ID,
				Name:      call.Name,
				Arguments: call.Arguments,
			})
		}
	}

	if err := sdkStream.Err(); err != nil {
		// Partial usage may still have arrived; keep it so a failed call is not
		// recorded as free.
		out.Usage = c.usageFrom(acc)
		return out, fmt.Errorf("llm: stream failed: %w", err)
	}

	if len(acc.Choices) == 0 {
		out.Usage = c.usageFrom(acc)
		return out, errors.New("llm: response contained no choices")
	}

	choice := acc.Choices[0]
	out.Content = choice.Message.Content
	out.FinishReason = choice.FinishReason
	out.ToolCalls = fromSDKToolCalls(choice.Message.ToolCalls)
	out.Usage = c.usageFrom(acc)
	out.PromptTokens = out.Usage.PromptTokens

	return out, nil
}

// usageFrom extracts usage and prices it.
func (c *Client) usageFrom(acc openai.ChatCompletionAccumulator) Usage {
	u := Usage{
		PromptTokens:       acc.Usage.PromptTokens,
		CachedPromptTokens: acc.Usage.PromptTokensDetails.CachedTokens,
		CompletionTokens:   acc.Usage.CompletionTokens,
	}
	u.CostUSD = c.Cost(u)
	return u
}

// Cost prices a usage record.
//
// Cached prompt tokens are billed at their own rate and are a subset of the
// prompt total, so they are subtracted out rather than counted twice.
func (c *Client) Cost(u Usage) float64 {
	const perMillion = 1_000_000.0

	fresh := u.PromptTokens - u.CachedPromptTokens
	if fresh < 0 {
		fresh = 0
	}

	cost := float64(fresh)/perMillion*c.cfg.InputPerMtok +
		float64(u.CachedPromptTokens)/perMillion*c.cfg.CachedInputPerMtok +
		float64(u.CompletionTokens)/perMillion*c.cfg.OutputPerMtok

	return cost
}

// ---------------------------------------------------------------- conversion

// toSDKMessages converts Doot messages into SDK params.
func toSDKMessages(messages []Message) []openai.ChatCompletionMessageParamUnion {
	out := make([]openai.ChatCompletionMessageParamUnion, 0, len(messages))

	for _, m := range messages {
		switch m.Role {
		case "system":
			out = append(out, openai.SystemMessage(m.Content))

		case "user":
			out = append(out, openai.UserMessage(m.Content))

		case "tool":
			out = append(out, openai.ToolMessage(m.Content, m.ToolCallID))

		case "assistant":
			// An assistant turn carrying tool calls has to be built by hand; the
			// helper only takes content.
			if len(m.ToolCalls) == 0 {
				out = append(out, openai.AssistantMessage(m.Content))
				continue
			}

			calls := make([]openai.ChatCompletionMessageToolCallParam, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				calls = append(calls, openai.ChatCompletionMessageToolCallParam{
					ID: tc.ID,
					Function: openai.ChatCompletionMessageToolCallFunctionParam{
						Name:      tc.Name,
						Arguments: tc.Arguments,
					},
				})
			}

			assistant := &openai.ChatCompletionAssistantMessageParam{ToolCalls: calls}
			if m.Content != "" {
				assistant.Content = openai.ChatCompletionAssistantMessageParamContentUnion{
					OfString: openai.String(m.Content),
				}
			}
			out = append(out, openai.ChatCompletionMessageParamUnion{OfAssistant: assistant})
		}
	}

	return out
}

func toSDKTools(tools []Tool) []openai.ChatCompletionToolParam {
	out := make([]openai.ChatCompletionToolParam, 0, len(tools))
	for _, t := range tools {
		fn := shared.FunctionDefinitionParam{
			Name:       t.Name,
			Parameters: shared.FunctionParameters(t.Parameters),
		}
		if t.Description != "" {
			fn.Description = openai.String(t.Description)
		}
		out = append(out, openai.ChatCompletionToolParam{Function: fn})
	}
	return out
}

func fromSDKToolCalls(calls []openai.ChatCompletionMessageToolCall) []ToolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]ToolCall, 0, len(calls))
	for _, c := range calls {
		out = append(out, ToolCall{
			ID:        c.ID,
			Name:      c.Function.Name,
			Arguments: c.Function.Arguments,
		})
	}
	return out
}
