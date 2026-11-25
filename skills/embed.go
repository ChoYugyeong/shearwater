package skills

import "embed"

// Ops holds the bundled shearwater-ops skill files embedded into the sw binary.
//
//go:embed shearwater-ops
var Ops embed.FS
