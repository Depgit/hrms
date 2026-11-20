// Package legacy embeds the PostgreSQL migrations of the app before it moved to MongoDB (001 to 030), applied in
// file-name order. They are only used by `hrms import-pg`, which brings an old PostgreSQL database up to date with
// them before copying it. The MongoDB schema lives in internal/store (schema_gen.go and the migrations there).
package legacy

import "embed"

//go:embed *.sql
var FS embed.FS
