package probe

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/maci0/toktop/internal/core"
)

// probeOllama measures a generation against the Ollama /api/generate dialect,
// whose terminal frame carries the engine's own eval_count and eval_duration
// and whose stream is NDJSON rather than SSE.
func probeOllama(ctx context.Context, r Request, s *core.ProbeSample) (tokens int, evalDur, ttft time.Duration, err error) {
	body, _ := json.Marshal(map[string]any{
		"model":  r.Model,
		"prompt": promptText,
		"stream": true,
		// Thinking models (deepseek-r1, qwen3, …) generate a reasoning
		// trace that num_predict does not cap. Turning think off keeps the
		// probe inside the 32-token budget instead of filling the 30s
		// timeout (and a billed gateway's invoice).
		"think": false,
		"options": map[string]any{
			"num_predict": probeTokens,
			"temperature": 0.2,
		},
	})
	resp, err := postJSON(ctx, r.Base+"/api/generate", body)
	if err != nil {
		return 0, 0, 0, err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(io.LimitReader(resp.Body, probeStreamMax))
	sc.Buffer(make([]byte, 0, probeBufInit), probeLineMax)
	var reported, contentBytes, reasoning, frames int
	hungUp := false
	for sc.Scan() {
		// The budget is checked per line, before the line is classified, so a
		// line the decoder skips cannot escape it: a blank separator and a
		// malformed frame both leave tokens and contentBytes at zero, and a
		// stream of them is an exchange that will not end on its own. The
		// frame that trips the cap is the one after the last looked at, so the
		// counter is read here and advanced below.
		if overBudget(tokens+reasoning, contentBytes, frames) {
			hungUp = true
			break
		}
		frames++
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var chunk struct {
			Response     string `json:"response"`
			Thinking     string `json:"thinking"`
			Done         bool   `json:"done"`
			DoneReason   string `json:"done_reason"`
			EvalCount    int    `json:"eval_count"`
			EvalDuration int64  `json:"eval_duration"`
			Error        string `json:"error"`
		}
		// A frame the probe cannot decode is not a frame to skip: the tokens
		// it carried are gone, and the frames after it are counted as if they
		// were the whole generation, so the sample would report a short,
		// plausible measurement with OK set. The OpenAI path refuses the same
		// input for the same reason.
		if err := json.Unmarshal(line, &chunk); err != nil {
			return 0, 0, ttft, fmt.Errorf("decode stream frame: %w", err)
		}
		// Ollama streams failures as {"error":…} lines with HTTP 200; decoding
		// them as content would report a green probe with invented throughput.
		if chunk.Error != "" {
			return 0, 0, ttft, fmt.Errorf("engine error: %s", core.Snippet([]byte(chunk.Error)))
		}
		// The same closed set the OpenAI dialect's finish_reason is read
		// against, on the frame field that spells it here: a serving stack in
		// front of the daemon can end a turn without decoding it, and the
		// reason it says so is engine- and model-chosen text, so it takes the
		// same bound and the same terminal-escape stripping. Checked on every
		// frame rather than on the terminal one, because a stack that ends the
		// exchange early need not mark the frame done.
		if chunk.DoneReason != "" && refusedFinishReasons[chunk.DoneReason] {
			return 0, 0, ttft, fmt.Errorf("engine refused: %s", core.Snippet([]byte(chunk.DoneReason)))
		}
		// Non-stream Ollama answers in one object with both response and
		// done=true; counting only !Done frames treated that as empty.
		if chunk.Response != "" {
			tokens++
			contentBytes += len(chunk.Response)
			if ttft == 0 {
				ttft = time.Since(s.At)
			}
		}
		if chunk.Thinking != "" {
			reasoning++
			contentBytes += len(chunk.Thinking)
		}
		// The terminal frame is read before the budget check: it can be the
		// frame that trips the cap, and its eval_count/eval_duration are the
		// engine's own numbers. Breaking on the cap first dropped them and
		// fell back to frame counting with no decode duration.
		if chunk.Done {
			if chunk.EvalCount > 0 {
				reported = chunk.EvalCount
				evalDur = nanoseconds(chunk.EvalDuration)
			}
			break
		}
		if overBudget(tokens+reasoning, contentBytes, frames) {
			hungUp = true
			break
		}
	}
	if err := streamReadErr(ctx, sc.Err(), tokens, hungUp); err != nil {
		return 0, 0, ttft, err
	}
	n, trust := resolveTokens(tokens, reported)
	if !trust {
		evalDur = 0 // eval_duration is paired with the rejected count
	}
	if n == 0 { // stream closed without a single token: engine is broken
		return 0, 0, ttft, fmt.Errorf("empty stream")
	}
	return n, evalDur, ttft, nil
}
