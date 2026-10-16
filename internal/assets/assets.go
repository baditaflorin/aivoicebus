// Package assets holds the texts aivoicebus ships inside the binary, so what an
// agent is told always matches the binary it runs and there is no file to drift.
package assets

import _ "embed"

//go:embed usage.md
var usage string

//go:embed snippet.md
var snippet string

// Usage is the full how-to, printed by `aivoicebus usage`.
func Usage() string { return usage }

// Snippet is the short block placed in each agent tool's instruction file.
func Snippet() string { return snippet }
