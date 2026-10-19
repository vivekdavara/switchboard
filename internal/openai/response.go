package openai

// ChatCompletion is a non-streamed chat completion, as OpenAI writes it.
type ChatCompletion struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"` // "chat.completion"
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
}

// Choice is one of a completion's answers.
type Choice struct {
	Index        int     `json:"index"`
	Message      Reply   `json:"message"`
	Logprobs     *string `json:"logprobs"` // always null here
	FinishReason string  `json:"finish_reason"`
}

// Reply is the assistant's message in a choice.
type Reply struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Usage is the token count of a completion.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ModelList is the answer to GET /v1/models.
type ModelList struct {
	Object string  `json:"object"` // "list"
	Data   []Model `json:"data"`
}

// Model is one entry of a ModelList.
type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"` // "model"
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// ChatCompletionChunk is one event of a streamed chat completion: "data: <chunk>" and a blank line. The stream ends
// with the event "data: [DONE]".
type ChatCompletionChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"` // "chat.completion.chunk"
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
	Usage   *Usage        `json:"usage,omitempty"` // only on the last chunk, when stream_options.include_usage asks
}

// ChunkChoice is one choice's part of a chunk.
type ChunkChoice struct {
	Index        int     `json:"index"`
	Delta        Delta   `json:"delta"`
	Logprobs     *string `json:"logprobs"` // always null here
	FinishReason *string `json:"finish_reason"`
}

// Delta is what a chunk adds to a choice's message: the role first, then the content piece by piece.
type Delta struct {
	Role    string  `json:"role,omitempty"`
	Content *string `json:"content,omitempty"`
}

// Done is the data of a stream's last event.
const Done = "[DONE]"
