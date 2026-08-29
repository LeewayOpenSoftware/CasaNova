/*
credit: https://github.com/containrrr/watchtower
*/
package docker

import (
	"context"
	"fmt"
	"strings"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/errdefs"
	"github.com/samber/lo"
)

func ImageName(containerInfo *container.InspectResponse) string {
	imageName := containerInfo.Config.Image

	if !strings.Contains(imageName, ":") {
		imageName = imageName + ":latest"
	}

	return imageName
}

func Container(ctx context.Context, id string) (*container.InspectResponse, error) {
	cli, err := SharedClient()
	if err != nil {
		return nil, err
	}

	containerInfo, err := cli.ContainerInspect(ctx, id)
	if err != nil {
		return nil, err
	}

	return &containerInfo, nil
}

func CloneContainer(ctx context.Context, id string, newName string) (string, error) {
	containerInfo, err := Container(ctx, id)
	if err != nil {
		return "", err
	}

	imageInfo, err := InspectImage(ctx, containerInfo.Image)
	if err != nil {
		return "", err
	}

	config := runtimeConfig(containerInfo, imageInfo)
	hostConfig := hostConfig(containerInfo)
	networkConfig := &network.NetworkingConfig{EndpointsConfig: containerInfo.NetworkSettings.Networks}
	simpleNetworkConfig := simpleNetworkConfig(networkConfig)

	cli, err := SharedClient()
	if err != nil {
		return "", err
	}

	newContainer, err := cli.ContainerCreate(ctx, config, hostConfig, simpleNetworkConfig, nil, newName)
	if err != nil {
		return "", err
	}

	if !(hostConfig.NetworkMode.IsHost()) {
		for k := range simpleNetworkConfig.EndpointsConfig {
			if err := cli.NetworkDisconnect(ctx, k, newContainer.ID, true); err != nil {
				return newContainer.ID, err
			}
		}

		for k, v := range networkConfig.EndpointsConfig {
			if err := cli.NetworkConnect(ctx, k, newContainer.ID, v); err != nil {
				return newContainer.ID, err
			}
		}
	}

	return newContainer.ID, nil
}

func RemoveContainer(ctx context.Context, id string) error {
	cli, err := SharedClient()
	if err != nil {
		return err
	}

	return cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true})
}

func RenameContainer(ctx context.Context, id string, name string) error {
	cli, err := SharedClient()
	if err != nil {
		return err
	}

	return cli.ContainerRename(ctx, id, name)
}

func StartContainer(ctx context.Context, id string) error {
	cli, err := SharedClient()
	if err != nil {
		return err
	}

	containerInfo, err := cli.ContainerInspect(ctx, id)
	if err != nil {
		return err
	}

	if !containerInfo.State.Running {
		return cli.ContainerStart(ctx, id, container.StartOptions{})
	}

	return nil
}

func StopContainer(ctx context.Context, id string) error {
	cli, err := SharedClient()
	if err != nil {
		return err
	}

	containerInfo, err := cli.ContainerInspect(ctx, id)
	if err != nil {
		return err
	}

	if containerInfo.State.Running {
		if err := cli.ContainerStop(ctx, id, container.StopOptions{}); err != nil {
			return err
		}

		if err := WaitContainer(ctx, id, container.WaitConditionNotRunning); err != nil {
			return err
		}
	}

	return nil
}

func WaitContainer(ctx context.Context, id string, condition container.WaitCondition) error {
	cli, err := SharedClient()
	if err != nil {
		return err
	}

	wait, errChan := cli.ContainerWait(ctx, id, condition)
	for {
		select {
		case err := <-errChan:
			// If the container was already removed (e.g. between Stop and Wait),
			// treat it as success — the container is gone, which satisfies the wait.
			if errdefs.IsNotFound(err) {
				return nil
			}
			return err
		case <-wait:
			return nil
		}
	}
}

func runtimeConfig(containerInfo *container.InspectResponse, imageInfo *image.InspectResponse) *container.Config {
	// Deep copy so mutations don't affect the original inspect response.
	configCopy := *containerInfo.Config
	config := &configCopy
	hostConfig := containerInfo.HostConfig
	imageConfig := imageInfo.Config

	if config.WorkingDir == imageConfig.WorkingDir {
		config.WorkingDir = ""
	}

	if config.User == imageConfig.User {
		config.User = ""
	}

	if hostConfig.NetworkMode.IsContainer() {
		config.Hostname = ""
	}

	if utils.CompareStringSlices(config.Entrypoint, imageConfig.Entrypoint) {
		config.Entrypoint = nil
		if utils.CompareStringSlices(config.Cmd, imageConfig.Cmd) {
			config.Cmd = nil
		}
	}

	config.Env = lo.Filter(config.Env, func(s string, i int) bool { return !lo.Contains(imageConfig.Env, s) })

	// Keep all container labels as-is. Image labels are already baked into
	// the image and do not need to be stripped from the container config.
	// The previous logic incorrectly removed user-customised labels that
	// happened to match image defaults.

	config.Volumes = lo.OmitBy(config.Volumes, func(k string, v struct{}) bool {
		v2, ok := imageConfig.Volumes[k]
		return ok && v == v2
	})

	// subtract ports exposed in image from container
	for k := range config.ExposedPorts {
		if _, ok := imageConfig.ExposedPorts[string(k)]; ok {
			delete(config.ExposedPorts, k)
		}
	}

	for p := range containerInfo.HostConfig.PortBindings {
		config.ExposedPorts[p] = struct{}{}
	}

	config.Image = ImageName(containerInfo)
	return config
}

func hostConfig(containerInfo *container.InspectResponse) *container.HostConfig {
	// Deep copy so mutations don't affect the original inspect response.
	copied := *containerInfo.HostConfig

	// Deep copy the Links slice separately since slices are reference types.
	if containerInfo.HostConfig.Links != nil {
		copied.Links = make([]string, len(containerInfo.HostConfig.Links))
		copy(copied.Links, containerInfo.HostConfig.Links)
	}

	for i, link := range copied.Links {
		name := link[0:strings.Index(link, ":")]
		alias := link[strings.LastIndex(link, "/"):]

		copied.Links[i] = fmt.Sprintf("%s:%s", name, alias)
	}

	return &copied
}

// simpleNetworkConfig is a networkConfig with only 1 network.
// see: https://github.com/docker/docker/issues/29265
func simpleNetworkConfig(networkConfig *network.NetworkingConfig) *network.NetworkingConfig {
	oneEndpoint := make(map[string]*network.EndpointSettings)
	for k, v := range networkConfig.EndpointsConfig {
		oneEndpoint[k] = v
		// we only need 1
		break
	}
	return &network.NetworkingConfig{EndpointsConfig: oneEndpoint}
}
