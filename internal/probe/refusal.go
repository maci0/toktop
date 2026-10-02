// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package probe

// Why a generation that answers nothing is still not a measurement. A model
// that declines, and a host-side content filter that cuts a stream, both
// arrive over HTTP 200 carrying the shape of an ordinary completion: a choice
// with a little text in it, sometimes a usage count beside it. Decoded as an
// ordinary completion they yield a time to first token and a decode rate for
// text no model produced, and the dashboard renders those next to real
// measurements with nothing to tell them apart. The engine-error paths in
// openai.go refuse that reading for an `{"error":…}` frame; these are the same
// refusal for the answers that carry it inside the choices instead.

// refusedFinishReasons are the endings that are not a generation. A choice
// ending in one of these was not decoded from the prompt: a content filter cut
// it, or the model declined.
//
// An ending naming a reason not in this set ("stop", "tool_calls", "length",
// "eos_token", "END_TURN") is a normal ending and is not read as a refusal. The
// set is deliberately closed rather than a check for anything but the ordinary
// endings: a list a provider could extend would call a healthy engine broken
// the day it added a spelling, and every member here has the same whole
// content — the model did not answer.
var refusedFinishReasons = map[string]bool{
	"content_filter":     true, // Azure OpenAI, and the gateways fronting it
	"refusal":            true, // OpenAI structured outputs
	"SAFETY":             true, // Gemini, through a gateway
	"RECITATION":         true, // Gemini, through a gateway
	"BLOCKLIST":          true,
	"PROHIBITED_CONTENT": true,
	"SPII":               true, // sensitive personally identifiable information
}

// refusalMessage reports why a choice declined to answer, or "" when it
// answered. The reason is engine- and model-chosen text, so it takes the same
// bound and the same terminal-escape stripping an engine error does
// (engineErrorText); the readout clips it to the cell width at render time.
//
// The refusal field is read first and quoted in front of the reason: it is the
// model's own sentence for what it would not do, where the reason is one word
// of a provider's vocabulary. Either alone is enough — a gateway that reports
// a refusal with no reason, and a filter that reports a reason with none, name
// the same failure.
func refusalMessage(c openaiChunk) string {
	for _, ch := range c.Choices {
		if r := ch.Message.Refusal; r != "" {
			return engineErrorText(r)
		}
		if ch.FinishReason != "" && refusedFinishReasons[ch.FinishReason] {
			return engineErrorText(ch.FinishReason)
		}
	}
	return ""
}
