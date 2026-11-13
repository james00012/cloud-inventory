// Package schema holds the protocol's JSON schema, which is the contract a
// provider in any language implements.
package schema

import _ "embed"

// Protocol is the schema every message on the wire is valid against.
//
//go:embed protocol.schema.json
var Protocol []byte
