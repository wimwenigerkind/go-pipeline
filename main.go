package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"gopkg.in/yaml.v3"
)

type Pipeline struct {
	Image string `yaml:"image"`
	Steps []Step `yaml:"steps"`
}

type Step struct {
	Name        string            `yaml:"name"`
	Image       string            `yaml:"image"`
	Command     []string          `yaml:"command,omitempty"`
	Script      string            `yaml:"script,omitempty"`
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

	// Pull images for each step
	pulledImages := make(map[string]bool)
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
			fmt.Printf("Successfully pulled image: %s\n", step.Image)
		} else {
			fmt.Printf("Image %s already pulled, skipping...\n", step.Image)
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
		if err := executeStep(cli, ctx, step); err != nil {
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

func executeStep(cli *client.Client, ctx context.Context, step Step) error {
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

	// Create container
	containerName := fmt.Sprintf("pipeline-step-%s", step.Name)
	resp, err := cli.ContainerCreate(ctx, containerConfig, nil, nil, nil, containerName)

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

	// Stream logs to stdout
	if _, err := io.Copy(os.Stdout, logReader); err != nil {
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
