// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"strings"

	"github.com/maci0/toktop/internal/core"
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
		folded = strings.ReplaceAll(folded, slug, "~")
	}
	return folded
}
