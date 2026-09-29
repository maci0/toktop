// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

// Package repogate holds the tests that keep the repository's own metadata
// true. It ships no code: every file here is a test, and the package exists
// only to give those tests a home and a name.
//
// The metadata is the set of files that claim something about the tree rather
// than being part of it: go.mod, the tier table and package map in
// docs/ARCHITECTURE.md, the reason column in docs/DEPENDENCIES.md, the pins in
// scripts/requirements.txt, the CHECK_CHANGELOG recipe in the Makefile, the
// action refs in .github/workflows, the go floor quoted in the README, and the
// surface a package published outside the module exposes, and the schema
// revision the --json report publishes. Each is written by hand and checked
// by CI, so each drifts silently the moment the thing it describes moves.
//
// The tests live here rather than beside the code they check because none of
// them is about a Go package. In cmd/toktop, where these tests sat, a reader
// looking for how the binary starts up found the supply-chain gate instead,
// and the gate itself was two directories from the files it reads. This
// package is above every tier (it names all of them) and imports none of them,
// which is why the tier table puts it last.
package repogate
