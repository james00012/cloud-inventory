package provider

import (
	"context"

	goplugin "github.com/hashicorp/go-plugin"
	cloudinv "github.com/james00012/cloud-inventory"
	pb "github.com/james00012/cloud-inventory/plugin/proto"
	"github.com/james00012/cloud-inventory/plugin/wire"
	"google.golang.org/grpc"
)

// GRPCPlugin is the go-plugin shape for this contract. The host side builds one
// with no implementation; the provider side builds one that serves.
type GRPCPlugin struct {
	goplugin.NetRPCUnsupportedPlugin

	Impl pb.InventoryServer
}

func (p *GRPCPlugin) GRPCServer(_ *goplugin.GRPCBroker, s *grpc.Server) error {
	pb.RegisterInventoryServer(s, p.Impl)
	return nil
}

func (p *GRPCPlugin) GRPCClient(_ context.Context, _ *goplugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	return pb.NewInventoryClient(c), nil
}

// Server builds the gRPC service for one resource type, which is what a
// provider binary serves and what a test can drive in process.
func Server[T cloudinv.Resource, Spec any, S any](r Resource[T, Spec, S]) pb.InventoryServer {
	return &server[T, Spec, S]{resource: r}
}

// Serve runs one resource type as a provider binary. It does not return.
func Serve(impl pb.InventoryServer) {
	goplugin.Serve(&goplugin.ServeConfig{
		HandshakeConfig: wire.Handshake,
		Plugins:         goplugin.PluginSet{wire.Name: &GRPCPlugin{Impl: impl}},
		GRPCServer:      goplugin.DefaultGRPCServer,
	})
}
