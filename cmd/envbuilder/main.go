package main

import (
	"bufio"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"

	"github.com/coder/envbuilder/options"

	"github.com/coder/coder/v2/codersdk"
	"github.com/coder/envbuilder"
	"github.com/coder/envbuilder/log"
	"github.com/coder/serpent"

	// *Never* remove this. Certificates are not bundled as part
	// of the container, so this is necessary for all connections
	// to not be insecure.
	_ "github.com/breml/rootcerts"
)

// loadEnvFile reads environment variables from a file and sets them.
// This enables container persistence by allowing fresh env vars on restart.
// The file format is KEY=VALUE, one per line. Lines starting with # are ignored.
func loadEnvFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Split on first = only
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		if key != "" {
			os.Setenv(key, value)
		}
	}
	return scanner.Err()
}

func main() {
	cmd := envbuilderCmd()
	err := cmd.Invoke().WithOS().Run()
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "error: %v", err)
		os.Exit(1)
	}
}

func envbuilderCmd() serpent.Command {
	var o options.Options
	cmd := serpent.Command{
		Use:     "envbuilder",
		Options: o.CLI(),
		Handler: func(inv *serpent.Invocation) error {
			// Load fresh environment from file if specified.
			// This enables container persistence by allowing fresh env vars
			// (like CODER_AGENT_TOKEN) on container restart.
			// Must be done early, before other env-dependent initialization.
			if o.EnvFile != "" {
				if err := loadEnvFile(o.EnvFile); err != nil {
					// Log but don't fail - file might not exist on first run
					fmt.Fprintf(os.Stderr, "Warning: failed to load env file %s: %v\n", o.EnvFile, err)
				} else {
					fmt.Fprintf(os.Stderr, "Loaded fresh environment from %s\n", o.EnvFile)
					// Re-parse options that might have been overridden
					// For now, just update the ones we know are dynamic
					if token := os.Getenv("CODER_AGENT_TOKEN"); token != "" {
						o.CoderAgentToken = token
					}
					if url := os.Getenv("CODER_AGENT_URL"); url != "" {
						o.CoderAgentURL = url
					}
				}
			}

			o.SetDefaults()
			var preExecs []func()
			preExec := func() {
				for _, fn := range preExecs {
					fn()
				}
				preExecs = nil
			}
			defer preExec() // Ensure cleanup in case of error.

			o.Logger = log.New(os.Stderr, o.Verbose)

			// Track Coder client for lifecycle reporting on failure
			var coderClient *log.CoderClient

			if o.CoderAgentURL != "" {
				u, err := url.Parse(o.CoderAgentURL)
				if err != nil {
					return fmt.Errorf("unable to parse CODER_AGENT_URL as URL: %w", err)
				}

				// Determine auth method
				switch o.CoderAuthMethod {
				case "gcp-instance-identity":
					// Use GCP instance identity authentication
					if o.GCPServiceAccount == "" {
						return errors.New("ENVBUILDER_GCP_SERVICE_ACCOUNT is required when using gcp-instance-identity auth")
					}
					client, err := log.CoderWithGCPAuth(inv.Context(), u, o.GCPServiceAccount)
					if err != nil {
						o.Logger(log.LevelError, "unable to authenticate to Coder via GCP instance identity: %s", err.Error())
					} else {
						coderClient = client
						o.Logger = log.Wrap(o.Logger, client.Logger())
						preExecs = append(preExecs, func() {
							client.Close()
						})
					}

				case "token", "":
					// Use token-based authentication (default)
					if o.CoderAgentToken == "" {
						return errors.New("CODER_AGENT_TOKEN is required when CODER_AGENT_URL is set (or use ENVBUILDER_CODER_AUTH_METHOD=gcp-instance-identity)")
					}
					coderLog, closeLogs, err := log.Coder(inv.Context(), u, o.CoderAgentToken)
					if err == nil {
						o.Logger = log.Wrap(o.Logger, coderLog)
						preExecs = append(preExecs, func() {
							closeLogs()
						})
					} else {
						o.Logger(log.LevelError, "unable to send logs to Coder: %s", err.Error())
					}

				default:
					return fmt.Errorf("invalid ENVBUILDER_CODER_AUTH_METHOD: %q (valid values: token, gcp-instance-identity)", o.CoderAuthMethod)
				}

				// Add envbuilder subsystem for telemetry
				if !slices.Contains(o.CoderAgentSubsystem, string(codersdk.AgentSubsystemEnvbuilder)) {
					o.CoderAgentSubsystem = append(o.CoderAgentSubsystem, string(codersdk.AgentSubsystemEnvbuilder))
					_ = os.Setenv("CODER_AGENT_SUBSYSTEM", strings.Join(o.CoderAgentSubsystem, ","))
				}
			}

			if o.GitSSHPrivateKeyPath != "" && o.GitSSHPrivateKeyBase64 != "" {
				return errors.New("cannot have both GIT_SSH_PRIVATE_KEY_PATH and GIT_SSH_PRIVATE_KEY_BASE64 set")
			}

			if o.GetCachedImage {
				img, err := envbuilder.RunCacheProbe(inv.Context(), o)
				if err != nil {
					o.Logger(log.LevelError, "error: %s", err)
					return err
				}
				digest, err := img.Digest()
				if err != nil {
					return fmt.Errorf("get cached image digest: %w", err)
				}
				_, _ = fmt.Fprintf(inv.Stdout, "ENVBUILDER_CACHED_IMAGE=%s@%s\n", o.CacheRepo, digest.String())
				return nil
			}

			err := envbuilder.Run(inv.Context(), o, preExec)
			if err != nil {
				o.Logger(log.LevelError, "error: %s", err)

				// Report start_error lifecycle state to Coder if we have a client
				if coderClient != nil {
					o.Logger(log.LevelInfo, "Reporting start_error lifecycle state to Coder...")
					if lifecycleErr := coderClient.ReportLifecycle(inv.Context(), codersdk.WorkspaceAgentLifecycleStartError); lifecycleErr != nil {
						o.Logger(log.LevelError, "Failed to report lifecycle state: %s", lifecycleErr.Error())
					} else {
						o.Logger(log.LevelInfo, "Successfully reported start_error to Coder")
					}
				}
			}
			return err
		},
	}
	return cmd
}
