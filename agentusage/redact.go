// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"github.com/maci0/toktop/internal/core"
	"golang.org/x/text/unicode/norm"
)

// redactStorePath folds the account that owns $HOME out of text a store
// path reached. [core.RedactHome] knows the path spelling only, and the
// stores these lines name a working directory with [pathSlug]: a cwd of
// /home/dev/Desktop/vllm-spark-0731 becomes the directory name
// home-dev-Desktop-vllm-spark-0731, with the separators turned into '-'. That
// is not a path any prefix fold can see, so the account spelled out inside
// the slug reaches the audit line un-folded, and a walk failure names it in
// the error beside the root as well.
//
// The fold is prefix-shaped: the slug of $HOME leads the slug of every
// directory under it, so replacing it leaves ~/Desktop/vllm-spark-0731, which
// is what the operator needs to find the project. A sibling home whose name
// merely starts with the account (/home/dev-old) is folded too, because the
// slug separator and the name have nothing to tell them apart once encoded.
func redactStorePath(s string) string {
	folded := core.RedactHome(s)
	if slug := pathSlug(HomeDir()); slug != "" {
		// Compose both sides and fold case, the way RedactHome folds a path
		// element. The store names the directory from its own cwd spelling
		// and HomeDir spells the account the environment's way, so on the
		// case-folding file systems the two differ as bytes while naming
		// one account: a byte-exact ReplaceAll put "users-dev" through
		// beside a slug of "Users-Dev" and left the account in the line.
		// The composed spellings are what compare, a decomposed account
		// ("jose" + U+0301) against a precomposed one included.
		folded = core.ReplaceFold(norm.NFC.String(folded), norm.NFC.String(slug), "~")
	}
	return folded
}
