// Package migrations contains the reviewed, versioned database schema.
package migrations

import _ "embed"

// InitialSQL creates the eight initial business tables.
//
//go:embed 000001_initial.up.sql
var InitialSQL string
