package icon

import _ "embed"

// DeejLogoPNG is the embedded PNG version of the logo for platforms (like Linux) that expect PNG tray icons
//
//go:embed logo-512.png
var DeejLogoPNG []byte
