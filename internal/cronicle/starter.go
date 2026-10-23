package cronicle

import _ "embed"

// starterHCL is the cronicle.hcl that `cronicle init` writes: a small DAG
// of inline commands that runs anywhere sh and python3 do.
//
//go:embed starter/cronicle.hcl
var starterHCL []byte

// Guide is the usage and authoring guide: written as AGENTS.md by
// `cronicle init` and printed by `cronicle guide`.
//
//go:embed starter/AGENTS.md
var Guide string
