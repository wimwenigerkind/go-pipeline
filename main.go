package main

import (
	"context"
	"fmt"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"gopkg.in/yaml.v3"
	"io"
	"os"
	"sync"
	"time"
)

type Pipeline struct {
	Image       string     `yaml:"image"`
	Steps       []Step     `yaml:"steps"`
	Definitions Definition `yaml:"definitions,omitempty"`
}

type Step struct {
	Name        string            `yaml:"name"`
	Image       string            `yaml:"image"`
	Command     []string          `yaml:"command,omitempty"`
	Script      string            `yaml:"script,omitempty"`
	Environment map[string]string `yaml:"environment,omitempty"`
}

type Definition struct {
	Services map[string]Service `yaml:"services"`
}

type Service struct {
	Image       string            `yaml:"image"`
	Environment map[string]string `yaml:"environment,omitempty"`
}

func main() {
	// Create Docker client
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		fmt.Printf("Error creating Docker client: %v\n", err)
		os.Exit(1)
	}
	defer cli.Close()

	ctx := context.Background()

	// Load Pipeline configuration from go-pipeline.yaml file
	data, err := os.ReadFile("go-pipeline.yaml")
	if err != nil {
		fmt.Printf("Error reading pipeline file: %v\n", err)
		fmt.Println("\nTip: Run with --generate-schema to create a sample configuration file")
		os.Exit(1)
	}

	var pipeline Pipeline
	if err := yaml.Unmarshal(data, &pipeline); err != nil {
		fmt.Printf("Error parsing YAML: %v\n", err)
		os.Exit(1)
	}

	// Validate pipeline
	if err := validatePipeline(pipeline); err != nil {
		fmt.Printf("Pipeline validation error: %v\n", err)
		os.Exit(1)
	}

	// Set default image for steps that don't have one
	for i, step := range pipeline.Steps {
		if step.Image == "" {
			pipeline.Steps[i].Image = pipeline.Image
		}
	}

	// Pull images for services and steps
	pulledImages := make(map[string]bool)

	// Pull service images first
	if len(pipeline.Definitions.Services) > 0 {
		for serviceName, service := range pipeline.Definitions.Services {
			if service.Image == "" {
				fmt.Printf("Service %s does not have an image specified.\n", serviceName)
				continue
			}

			// Only pull each image once
			if !pulledImages[service.Image] {
				fmt.Printf("Pulling image for service %s: %s\n", serviceName, service.Image)
				if err := pullImage(cli, ctx, service.Image); err != nil {
					fmt.Printf("Error pulling image %s: %v\n", service.Image, err)
					continue
				}
				pulledImages[service.Image] = true
				fmt.Printf("Successfully pulled service image: %s\n", service.Image)
			} else {
				fmt.Printf("Service image %s already pulled, skipping...\n", service.Image)
			}
		}
	}

	// Pull step images
	for _, step := range pipeline.Steps {
		if step.Image == "" {
			fmt.Printf("Step %s does not have an image specified.\n", step.Name)
			continue
		}

		// Only pull each image once
		if !pulledImages[step.Image] {
			fmt.Printf("Pulling image for step %s: %s\n", step.Name, step.Image)
			if err := pullImage(cli, ctx, step.Image); err != nil {
				fmt.Printf("Error pulling image %s: %v\n", step.Image, err)
				continue
			}
			pulledImages[step.Image] = true
			fmt.Printf("Successfully pulled step image: %s\n", step.Image)
		} else {
			fmt.Printf("Step image %s already pulled, skipping...\n", step.Image)
		}
	}

	// Create pipeline network
	fmt.Println("\nCreating pipeline network:")
	networkID, err := createPipelineNetwork(cli, ctx)
	if err != nil {
		fmt.Printf("Error creating pipeline network: %v\n", err)
		os.Exit(1)
	}

	// Ensure network is cleaned up on exit
	defer func() {
		fmt.Println("\nCleaning up network...")
		cleanupNetwork(cli, ctx, networkID)
	}()

	// Start services
	var serviceContainerIDs []string
	if len(pipeline.Definitions.Services) > 0 {
		fmt.Println("\nStarting services:")
		ids, err := startServices(cli, ctx, pipeline.Definitions, networkID)
		if err != nil {
			fmt.Printf("Error starting services: %v\n", err)
			os.Exit(1)
		}
		serviceContainerIDs = ids

		if len(serviceContainerIDs) > 0 {
			// Ensure services are cleaned up on exit
			defer func() {
				fmt.Println("\nCleaning up services...")
				stopServices(cli, ctx, serviceContainerIDs)
			}()
		}
	}

	// Execute pipeline steps
	fmt.Println("\nExecuting pipeline steps:")
	for _, step := range pipeline.Steps {
		if step.Image == "" {
			fmt.Printf("Skipping step %s: no image specified\n", step.Name)
			continue
		}

		fmt.Printf("Executing step: %s\n", step.Name)
		if err := executeStep(cli, ctx, step, networkID); err != nil {
			fmt.Printf("Error executing step %s: %v\n", step.Name, err)
			// You might want to decide whether to continue or exit here
			continue
		}
		fmt.Printf("Step %s completed successfully\n", step.Name)
	}

	fmt.Println("\nPipeline execution completed!")
}

func validatePipeline(pipeline Pipeline) error {
	if len(pipeline.Steps) == 0 {
		return fmt.Errorf("pipeline must have at least one step")
	}

	stepNames := make(map[string]bool)
	for _, step := range pipeline.Steps {
		if step.Name == "" {
			return fmt.Errorf("step name cannot be empty")
		}

		if stepNames[step.Name] {
			return fmt.Errorf("duplicate step name: %s", step.Name)
		}
		stepNames[step.Name] = true

		if step.Image == "" && pipeline.Image == "" {
			return fmt.Errorf("step %s has no image specified and no default image is set", step.Name)
		}

		if len(step.Command) > 0 && step.Script != "" {
			return fmt.Errorf("step %s cannot have both command and script", step.Name)
		}
	}

	return nil
}

func startServices(cli *client.Client, ctx context.Context, definition Definition, networkID string) ([]string, error) {
	var serviceContainerIDs []string

	for serviceName, service := range definition.Services {
		fmt.Printf("Starting service: %s\n", serviceName)

		// Create container configuration for service
		containerConfig := &container.Config{
			Image:        service.Image,
			AttachStdout: false,
			AttachStderr: false,
		}

		// Set environment variables for service
		if len(service.Environment) > 0 {
			var envVars []string
			for key, value := range service.Environment {
				envVars = append(envVars, fmt.Sprintf("%s=%s", key, value))
			}
			containerConfig.Env = envVars
		}

		// Create network configuration
		networkingConfig := &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				networkID: {
					Aliases: []string{serviceName}, // This enables DNS resolution by service name
				},
			},
		}

		// Create service container
		containerName := fmt.Sprintf("pipeline-service-%s", serviceName)
		resp, err := cli.ContainerCreate(ctx, containerConfig, nil, networkingConfig, nil, containerName)
		if err != nil {
			return serviceContainerIDs, fmt.Errorf("failed to create service container %s: %w", serviceName, err)
		}

		// Start service container
		if err := cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
			return serviceContainerIDs, fmt.Errorf("failed to start service container %s: %w", serviceName, err)
		}

		serviceContainerIDs = append(serviceContainerIDs, resp.ID)
		fmt.Printf("Service %s started successfully (container: %s, network: %s)\n", serviceName, resp.ID[:12], networkID[:12])
	}

	return serviceContainerIDs, nil
}

func stopServices(cli *client.Client, ctx context.Context, serviceContainerIDs []string) {
	var wg sync.WaitGroup

	for _, containerID := range serviceContainerIDs {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()

			fmt.Printf("Stopping service container: %s\n", id[:12])

			// Stop the container
			if err := cli.ContainerStop(ctx, id, container.StopOptions{}); err != nil {
				fmt.Printf("Warning: failed to stop service container %s: %v\n", id[:12], err)
			}

			// Remove the container
			if err := cli.ContainerRemove(ctx, id, container.RemoveOptions{}); err != nil {
				fmt.Printf("Warning: failed to remove service container %s: %v\n", id[:12], err)
			} else {
				fmt.Printf("Service container %s stopped and removed\n", id[:12])
			}
		}(containerID)
	}

	wg.Wait()
	fmt.Println("All services stopped and cleaned up")
}

func pullImage(cli *client.Client, ctx context.Context, imageName string) error {
	reader, err := cli.ImagePull(ctx, imageName, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("failed to pull image: %w", err)
	}
	defer reader.Close()

	// Copy the pull output to stdout to see progress
	if _, err := io.Copy(os.Stdout, reader); err != nil {
		return fmt.Errorf("error reading pull output: %w", err)
	}
	return nil
}

func executeStep(cli *client.Client, ctx context.Context, step Step, networkID string) error {
	// Prepare command
	var cmd []string
	if len(step.Command) > 0 {
		cmd = step.Command
	} else if step.Script != "" {
		// If script is provided, execute it with sh
		cmd = []string{"sh", "-c", step.Script}
	} else {
		// Default command if none specified
		cmd = []string{"echo", fmt.Sprintf("Executing step: %s", step.Name)}
	}

	// Create container configuration
	containerConfig := &container.Config{
		Image:        step.Image,
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
	}

	// Set environment variables
	if len(step.Environment) > 0 {
		var envVars []string
		for key, value := range step.Environment {
			envVars = append(envVars, fmt.Sprintf("%s=%s", key, value))
		}
		containerConfig.Env = envVars
	}

	// Create network configuration
	networkingConfig := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			networkID: {},
		},
	}

	// Create container
	containerName := fmt.Sprintf("pipeline-step-%s", step.Name)
	resp, err := cli.ContainerCreate(ctx, containerConfig, nil, networkingConfig, nil, containerName)

	if err != nil {
		return fmt.Errorf("failed to create container: %w", err)
	}

	// Start container
	if err := cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("failed to start container: %w", err)
	}

	// Get container logs
	logReader, err := cli.ContainerLogs(ctx, resp.ID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
	})
	if err != nil {
		return fmt.Errorf("failed to get container logs: %w", err)
	}
	defer logReader.Close()

	// Stream logs to stdout (properly handle Docker log format)
	if _, err := stdcopy.StdCopy(os.Stdout, os.Stderr, logReader); err != nil {
		fmt.Printf("Warning: error reading container logs: %v\n", err)
	}

	// Wait for container to finish
	statusCh, errCh := cli.ContainerWait(ctx, resp.ID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("error waiting for container: %w", err)
		}
	case status := <-statusCh:
		if status.StatusCode != 0 {
			return fmt.Errorf("container exited with non-zero status: %d", status.StatusCode)
		}
	}

	// Clean up: remove container
	if err := cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{}); err != nil {
		fmt.Printf("Warning: failed to remove container %s: %v\n", resp.ID[:12], err)
	}

	return nil
}

func createPipelineNetwork(cli *client.Client, ctx context.Context) (string, error) {
	networkName := fmt.Sprintf("pipeline-network-%d", time.Now().Unix())

	fmt.Printf("Creating pipeline network: %s\n", networkName)

	// Create network configuration
	networkConfig := network.CreateOptions{
		Driver:     "bridge",
		EnableIPv6: &[]bool{false}[0],
		Internal:   false,
	}

	// Create the network
	resp, err := cli.NetworkCreate(ctx, networkName, networkConfig)
	if err != nil {
		return "", fmt.Errorf("failed to create network: %w", err)
	}

	fmt.Printf("Pipeline network created successfully: %s\n", resp.ID[:12])
	return resp.ID, nil
}

func cleanupNetwork(cli *client.Client, ctx context.Context, networkID string) {
	if networkID == "" {
		return
	}

	fmt.Printf("Removing pipeline network: %s\n", networkID[:12])

	if err := cli.NetworkRemove(ctx, networkID); err != nil {
		fmt.Printf("Warning: failed to remove network %s: %v\n", networkID[:12], err)
	} else {
		fmt.Printf("Pipeline network removed successfully\n")
	}
}
