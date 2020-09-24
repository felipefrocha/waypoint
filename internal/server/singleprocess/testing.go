package singleprocess

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/boltdb/bolt"
	"github.com/hashicorp/go-hclog"
	hznhub "github.com/hashicorp/horizon/pkg/hub"
	hznpb "github.com/hashicorp/horizon/pkg/pb"
	hzntest "github.com/hashicorp/horizon/pkg/testutils/central"
	hzntoken "github.com/hashicorp/horizon/pkg/token"
	wphznpb "github.com/hashicorp/waypoint-hzn/pkg/pb"
	wphzn "github.com/hashicorp/waypoint-hzn/pkg/server"
	"github.com/imdario/mergo"
	"github.com/mitchellh/go-testing-interface"
	"github.com/stretchr/testify/require"

	configpkg "github.com/hashicorp/waypoint/internal/config"
	"github.com/hashicorp/waypoint/internal/server"
	pb "github.com/hashicorp/waypoint/internal/server/gen"
	serverptypes "github.com/hashicorp/waypoint/internal/server/ptypes"
)

// TestServer starts a singleprocess server and returns the connected client.
// We use t.Cleanup to ensure resources are automatically cleaned up.
func TestServer(t testing.T, opts ...Option) pb.WaypointClient {
	return server.TestServer(t, TestImpl(t, opts...))
}

// TestImpl returns the waypoint server implementation. This can be used
// with server.TestServer. It is easier to just use TestServer directly.
func TestImpl(t testing.T, opts ...Option) pb.WaypointServer {
	impl, err := New(append(
		[]Option{WithDB(testDB(t))},
		opts...,
	)...)
	require.NoError(t, err)
	return impl
}

// TestWithURLService is an Option for testing only that creates an
// in-memory URL service server. This requires access to an external
// postgres server.
//
// If out is non-nil, it will be written to with the DevSetup info.
func TestWithURLService(t testing.T, out *hzntest.DevSetup) Option {
	// Create the test server. On test end we close the channel which quits
	// the Horizon test server.
	setupCh := make(chan *hzntest.DevSetup, 1)
	closeCh := make(chan struct{})
	t.Cleanup(func() { close(closeCh) })
	go hzntest.Dev(t, func(setup *hzntest.DevSetup) {
		hubclient, err := hznhub.NewHub(hclog.L(), setup.ControlClient, setup.HubToken)
		require.NoError(t, err)
		go hubclient.Run(context.Background(), setup.ClientListener)

		setupCh <- setup
		<-closeCh
	})
	setup := <-setupCh

	// Make our test registration API
	wphzndata := wphzn.TestServer(t,
		wphzn.WithNamespace("/"),
		wphzn.WithHznControl(setup.MgmtClient),
	)

	// Get our account token.
	wpaccountResp, err := wphzndata.Client.RegisterGuestAccount(
		context.Background(),
		&wphznpb.RegisterGuestAccountRequest{
			ServerId: "A",
		},
	)
	require.NoError(t, err)

	// We need to get the account since that is what we need to query with
	tokenInfo, err := setup.MgmtClient.GetTokenPublicKey(context.Background(), &hznpb.Noop{})
	require.NoError(t, err)
	token, err := hzntoken.CheckTokenED25519(wpaccountResp.Token, tokenInfo.PublicKey)
	require.NoError(t, err)
	setup.Account = token.Account()

	// Copy our setup config
	if out != nil {
		*out = *setup
	}

	return func(s *service, cfg *config) error {
		if cfg.serverConfig == nil {
			cfg.serverConfig = &configpkg.ServerConfig{}
		}

		cfg.serverConfig.URL = &configpkg.URL{
			Enabled:              true,
			APIAddress:           wphzndata.Addr,
			APIInsecure:          true,
			APIToken:             wpaccountResp.Token,
			ControlAddress:       fmt.Sprintf("dev://%s", setup.HubAddr),
			AutomaticAppHostname: true,
		}

		return nil
	}
}

func TestEntrypoint(t testing.T, client pb.WaypointClient) (string, string, func()) {
	instanceId, err := server.Id()
	require.NoError(t, err)

	ctx := context.Background()

	resp, err := client.UpsertDeployment(ctx, &pb.UpsertDeploymentRequest{
		Deployment: serverptypes.TestValidDeployment(t, &pb.Deployment{
			Component: &pb.Component{
				Name: "testapp",
			},
		}),
	})
	require.NoError(t, err)

	dep := resp.Deployment

	// Create the config
	stream, err := client.EntrypointConfig(ctx, &pb.EntrypointConfigRequest{
		InstanceId:   instanceId,
		DeploymentId: dep.Id,
	})
	require.NoError(t, err)

	// Wait for the first config so that we know we're registered
	_, err = stream.Recv()
	require.NoError(t, err)

	return instanceId, dep.Id, func() {
		stream.CloseSend()
	}
}

// TestRunner registers a runner and returns the ID and a function to
// deregister the runner. This uses t.Cleanup so that the runner will always
// be deregistered on test completion.
func TestRunner(t testing.T, client pb.WaypointClient, r *pb.Runner) (string, func()) {
	require := require.New(t)
	ctx := context.Background()

	// Get the runner
	if r == nil {
		r = &pb.Runner{}
	}
	id, err := server.Id()
	require.NoError(err)
	require.NoError(mergo.Merge(r, &pb.Runner{Id: id}))

	// Open the config stream
	stream, err := client.RunnerConfig(ctx)
	require.NoError(err)
	t.Cleanup(func() { stream.CloseSend() })

	// Register
	require.NoError(err)
	require.NoError(stream.Send(&pb.RunnerConfigRequest{
		Event: &pb.RunnerConfigRequest_Open_{
			Open: &pb.RunnerConfigRequest_Open{
				Runner: r,
			},
		},
	}))

	// Wait for first message to confirm we're registered
	_, err = stream.Recv()
	require.NoError(err)

	return id, func() { stream.CloseSend() }
}

// TestApp creates the app in the DB.
func TestApp(t testing.T, client pb.WaypointClient, ref *pb.Ref_Application) {
	{
		_, err := client.UpsertProject(context.Background(), &pb.UpsertProjectRequest{
			Project: &pb.Project{
				Name: ref.Project,
			},
		})
		require.NoError(t, err)
	}

	{
		_, err := client.UpsertApplication(context.Background(), &pb.UpsertApplicationRequest{
			Project: &pb.Ref_Project{Project: ref.Project},
			Name:    ref.Application,
		})
		require.NoError(t, err)
	}
}

func testDB(t testing.T) *bolt.DB {
	t.Helper()

	// Temporary directory for the database
	td, err := ioutil.TempDir("", "test")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(td) })

	// Create the DB
	db, err := bolt.Open(filepath.Join(td, "test.db"), 0600, nil)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	return db
}
