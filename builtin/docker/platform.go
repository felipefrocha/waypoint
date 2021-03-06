package docker

import (
	"context"

	"fmt"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/hashicorp/go-hclog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"github.com/hashicorp/waypoint-plugin-sdk/component"
	"github.com/hashicorp/waypoint-plugin-sdk/docs"
	"github.com/hashicorp/waypoint-plugin-sdk/terminal"
)

const (
	labelId    = "waypoint.hashicorp.com/id"
	labelNonce = "waypoint.hashicorp.com/nonce"
)

// Platform is the Platform implementation for Docker.
type Platform struct {
	config PlatformConfig
}

// Config implements Configurable
func (p *Platform) Config() (interface{}, error) {
	return &p.config, nil
}

// DeployFunc implements component.Platform
func (p *Platform) DeployFunc() interface{} {
	return p.Deploy
}

// DestroyFunc implements component.Destroyer
func (p *Platform) DestroyFunc() interface{} {
	return p.Destroy
}

// ValidateAuthFunc implements component.Authenticator
func (p *Platform) ValidateAuthFunc() interface{} {
	return p.ValidateAuth
}

// AuthFunc implements component.Authenticator
func (p *Platform) AuthFunc() interface{} {
	return p.Auth
}

func (p *Platform) Auth() error {
	return nil
}

func (p *Platform) ValidateAuth() error {
	return nil
}

// Deploy deploys an image to Docker.
func (p *Platform) Deploy(
	ctx context.Context,
	log hclog.Logger,
	src *component.Source,
	job *component.JobInfo,
	img *Image,
	deployConfig *component.DeploymentConfig,
	ui terminal.UI,
) (*Deployment, error) {
	// We'll update the user in real time
	sg := ui.StepGroup()
	defer sg.Wait()

	cli, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "unable to create Docker client: %s", err)
	}

	if p.config.ServicePort == 0 {
		p.config.ServicePort = 3000
	}

	cli.NegotiateAPIVersion(ctx)

	// Create our deployment and set an initial ID
	var result Deployment
	id, err := component.Id()
	if err != nil {
		return nil, err
	}
	result.Id = id
	result.Name = src.App

	s := sg.Add("Setting up waypoint network")
	defer func() { s.Abort() }()

	nets, err := cli.NetworkList(ctx, types.NetworkListOptions{
		Filters: filters.NewArgs(filters.Arg("label", "use=waypoint")),
	})

	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "unable to list Docker networks: %s", err)
	}

	if len(nets) == 0 {
		_, err = cli.NetworkCreate(ctx, "waypoint", types.NetworkCreate{
			Driver:         "bridge",
			CheckDuplicate: true,
			Internal:       false,
			Attachable:     true,
			Labels: map[string]string{
				"use": "waypoint",
			},
		})

		if err != nil {
			return nil, status.Errorf(codes.FailedPrecondition, "unable to create Docker network: %s", err)
		}
	}

	s.Done()

	s = sg.Add("Creating new container")

	port := fmt.Sprint(p.config.ServicePort)
	np, err := nat.NewPort("tcp", port)
	if err != nil {
		return nil, err
	}

	cfg := container.Config{
		AttachStdout: true,
		AttachStderr: true,
		AttachStdin:  true,
		OpenStdin:    true,
		StdinOnce:    true,
		Image:        img.Image + ":" + img.Tag,
		ExposedPorts: nat.PortSet{np: struct{}{}},
		Env:          []string{"PORT=" + port},
	}

	if c := p.config.Command; len(c) > 0 {
		cfg.Cmd = c
	}

	bindings := nat.PortMap{}
	bindings[np] = []nat.PortBinding{
		{
			HostPort: "",
		},
	}

	hostconfig := container.HostConfig{
		Binds:        []string{src.App + "-scratch" + ":/input"},
		PortBindings: bindings,
	}

	netconfig := network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			"waypoint": {},
		},
	}

	for k, v := range p.config.StaticEnvVars {
		cfg.Env = append(cfg.Env, k+"="+v)
	}

	for k, v := range deployConfig.Env() {
		cfg.Env = append(cfg.Env, k+"="+v)
	}

	cfg.Labels = map[string]string{
		labelId:     result.Id,
		"app":       src.App,
		"workspace": job.Workspace,
	}

	name := src.App + "-" + id

	cr, err := cli.ContainerCreate(ctx, &cfg, &hostconfig, &netconfig, name)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "unable to create Docker container: %s", err)
	}

	s.Update("Starting container")
	err = cli.ContainerStart(ctx, cr.ID, types.ContainerStartOptions{})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "unable to start Docker container: %s", err)
	}
	s.Done()

	s = sg.Add("App deployed as container: " + name)
	s.Done()

	result.Container = cr.ID

	return &result, nil
}

// Destroy deletes the K8S deployment.
func (p *Platform) Destroy(
	ctx context.Context,
	log hclog.Logger,
	deployment *Deployment,
	ui terminal.UI,
) error {
	cli, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		return err
	}

	cli.NegotiateAPIVersion(ctx)

	// We'll update the user in real time
	st := ui.Status()
	defer st.Close()
	st.Update("Deleting container...")

	// Check if the container exists
	_, err = cli.ContainerInspect(ctx, deployment.Container)
	if client.IsErrNotFound(err) {
		return nil
	}

	// Remove it
	return cli.ContainerRemove(ctx, deployment.Container, types.ContainerRemoveOptions{
		Force: true,
	})
}

// Config is the configuration structure for the Platform.
type PlatformConfig struct {
	// The command to run in the container. This is an array of arguments
	// that are executed directly. These are not executed in the context of
	// a shell. If you want to use a shell, add that to this command manually.
	Command []string `hcl:"command,optional"`

	// A path to a directory that will be created for the service to store
	// temporary data.
	ScratchSpace string `hcl:"scratch_path,optional"`

	// Environment variables that are meant to configure the application in a static
	// way. This might be control an image that has mulitple modes of operation,
	// selected via environment variable. Most configuration should use the waypoint
	// config commands.
	StaticEnvVars map[string]string `hcl:"static_environment,optional"`

	// Port that your service is running on within the actual container.
	// Defaults to port 3000.
	// TODO Evaluate if this should remain as a default 3000, should be a required field,
	// or default to another port.
	ServicePort uint `hcl:"service_port,optional"`
}

func (p *Platform) Documentation() (*docs.Documentation, error) {
	doc, err := docs.New(docs.FromConfig(&PlatformConfig{}))
	if err != nil {
		return nil, err
	}

	doc.Description("Deploy a container to Docker, local or remote")

	doc.Example(`
deploy {
  use "docker" {
	command      = ["ps"]
	service_port = 3000
	static_environment = {
	  "environment": "production",
	  "LOG_LEVEL": "debug"
	}
  }
}
`)

	doc.Input("docker.Image")
	doc.Output("docker.Deployment")

	doc.SetField(
		"command",
		"the command to run to start the application in the container",
		docs.Default("the image entrypoint"),
	)

	doc.SetField(
		"scratch_path",
		"a path within the container to store temporary data",
		docs.Summary(
			"docker will mount a tmpfs at this path",
		),
	)

	doc.SetField(
		"static_environment",
		"environment variables to expose to the application",
		docs.Summary(
			"these environment variables should not be run of the mill",
			"configuration variables, use waypoint config for that.",
			"These variables are used to control over all container modes,",
			"such as configuring it to start a web app vs a background worker",
		),
	)

	doc.SetField(
		"service_port",
		"port that your service is running on in the container",
		docs.Default("3000"),
	)

	return doc, nil
}

var (
	_ component.Platform     = (*Platform)(nil)
	_ component.Configurable = (*Platform)(nil)
	_ component.Destroyer    = (*Platform)(nil)
)
