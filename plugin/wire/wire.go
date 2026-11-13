// Package wire holds what both sides of the plugin boundary must agree on
// before they can speak: the handshake and the plugin's name.
package wire

import "github.com/hashicorp/go-plugin"

// Handshake stops a host from running a binary that is not a provider, and a
// provider from being run by hand. ProtocolVersion rises only on a change the
// wire contract cannot absorb, since the protobuf contract changes additively.
var Handshake = plugin.HandshakeConfig{
	ProtocolVersion:  1,
	MagicCookieKey:   "CLOUD_INVENTORY_PLUGIN",
	MagicCookieValue: "a-provider-speaks-the-inventory-contract",
}

// Name is the plugin both sides look up in the plugin map.
const Name = "inventory"
