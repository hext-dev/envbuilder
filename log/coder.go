package log

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync"
	"time"

	"cdr.dev/slog"
	"cdr.dev/slog/sloggers/sloghuman"
	"cloud.google.com/go/compute/metadata"
	"github.com/coder/coder/v2/agent/proto"
	"github.com/coder/coder/v2/codersdk"
	"github.com/coder/coder/v2/codersdk/agentsdk"
	"github.com/coder/retry"
	"github.com/google/uuid"
	"golang.org/x/mod/semver"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var (
	// We set a relatively high connection timeout for the initial connection.
	// There is an unfortunate race between the envbuilder container starting and the
	// associated provisioner job completing.
	rpcConnectTimeout  = 30 * time.Second
	logSendGracePeriod = 10 * time.Second
	minAgentAPIV2      = "v2.9"
)

// Coder establishes a connection to the Coder instance located at coderURL and
// authenticates using token. It then establishes a dRPC connection to the Agent
// API and begins sending logs. If the version of Coder does not support the
// Agent API, it will fall back to using the PatchLogs endpoint. The closer is
// used to close the logger and to wait at most logSendGracePeriod for logs to
// be sent. Cancelling the context will close the logs immediately without
// waiting for logs to be sent.
func Coder(ctx context.Context, coderURL *url.URL, token string) (logger Func, closer func(), err error) {
	// To troubleshoot issues, we need some way of logging.
	metaLogger := slog.Make(sloghuman.Sink(os.Stderr))
	defer metaLogger.Sync()
	client := initClient(coderURL, token)
	bi, err := client.SDK.BuildInfo(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("get coder build version: %w", err)
	}
	if semver.Compare(semver.MajorMinor(bi.Version), minAgentAPIV2) < 0 {
		metaLogger.Warn(ctx, "Detected Coder version incompatible with AgentAPI v2, falling back to deprecated API", slog.F("coder_version", bi.Version))
		logger, closer = sendLogsV1(ctx, client, metaLogger.Named("send_logs_v1"))
		return logger, closer, nil
	}

	// Create a new context so we can ensure the connection is torn down.
	ctx, cancel := context.WithCancel(ctx)
	defer func() {
		if err != nil {
			cancel()
		}
	}()
	// Note that ctx passed to initRPC will be inherited by the
	// underlying connection, nothing we can do about that here.
	dac, err := initRPC(ctx, client, metaLogger.Named("init_rpc"))
	if err != nil {
		// Logged externally
		return nil, nil, fmt.Errorf("init coder rpc client: %w", err)
	}
	ls := agentsdk.NewLogSender(metaLogger.Named("coder_log_sender"))
	metaLogger.Warn(ctx, "Sending logs via AgentAPI v2", slog.F("coder_version", bi.Version))
	logger, loggerCloser := sendLogsV2(ctx, dac, ls, metaLogger.Named("send_logs_v2"))
	var closeOnce sync.Once
	closer = func() {
		loggerCloser()

		closeOnce.Do(func() {
			// Typically cancel would be after Close, but we want to be
			// sure there's nothing that might block on Close.
			cancel()
			_ = dac.DRPCConn().Close()
		})
	}
	return logger, closer, nil
}

// CoderClient wraps the connection to Coder and provides lifecycle reporting.
type CoderClient struct {
	client    *agentsdk.Client
	rpcClient proto.DRPCAgentClient20
	logger    Func
	closer    func()
}

// Logger returns the log function for sending logs to Coder.
func (c *CoderClient) Logger() Func {
	return c.logger
}

// Close closes the connection to Coder.
func (c *CoderClient) Close() {
	if c.closer != nil {
		c.closer()
	}
}

// ReportLifecycle reports a lifecycle state change to Coder via the dRPC API.
func (c *CoderClient) ReportLifecycle(ctx context.Context, state codersdk.WorkspaceAgentLifecycle) error {
	if c.rpcClient == nil {
		return fmt.Errorf("no RPC client available for lifecycle reporting")
	}

	// Map codersdk lifecycle state to proto lifecycle state
	var protoState proto.Lifecycle_State
	switch state {
	case codersdk.WorkspaceAgentLifecycleCreated:
		protoState = proto.Lifecycle_CREATED
	case codersdk.WorkspaceAgentLifecycleStarting:
		protoState = proto.Lifecycle_STARTING
	case codersdk.WorkspaceAgentLifecycleStartTimeout:
		protoState = proto.Lifecycle_START_TIMEOUT
	case codersdk.WorkspaceAgentLifecycleStartError:
		protoState = proto.Lifecycle_START_ERROR
	case codersdk.WorkspaceAgentLifecycleReady:
		protoState = proto.Lifecycle_READY
	case codersdk.WorkspaceAgentLifecycleShuttingDown:
		protoState = proto.Lifecycle_SHUTTING_DOWN
	case codersdk.WorkspaceAgentLifecycleShutdownTimeout:
		protoState = proto.Lifecycle_SHUTDOWN_TIMEOUT
	case codersdk.WorkspaceAgentLifecycleShutdownError:
		protoState = proto.Lifecycle_SHUTDOWN_ERROR
	case codersdk.WorkspaceAgentLifecycleOff:
		protoState = proto.Lifecycle_OFF
	default:
		return fmt.Errorf("unknown lifecycle state: %s", state)
	}

	_, err := c.rpcClient.UpdateLifecycle(ctx, &proto.UpdateLifecycleRequest{
		Lifecycle: &proto.Lifecycle{
			State:     protoState,
			ChangedAt: timestamppb.Now(),
		},
	})
	return err
}

// CoderWithGCPAuth establishes a connection to Coder using GCP instance identity
// authentication. This allows envbuilder to communicate with Coder without requiring
// a pre-existing agent token - useful when the agent runs inside the container that
// envbuilder is building.
func CoderWithGCPAuth(ctx context.Context, coderURL *url.URL, serviceAccount string) (*CoderClient, error) {
	metaLogger := slog.Make(sloghuman.Sink(os.Stderr))
	defer metaLogger.Sync()

	// Create a client without a token first
	client := agentsdk.New(coderURL)

	// Check if we're running on GCP
	if !metadata.OnGCE() {
		return nil, fmt.Errorf("not running on GCP, cannot use gcp-instance-identity auth")
	}

	// Create GCP metadata client
	gcpClient := metadata.NewClient(nil)

	// Authenticate using GCP instance identity
	metaLogger.Info(ctx, "Authenticating to Coder using GCP instance identity", slog.F("service_account", serviceAccount))
	authResp, err := client.AuthGoogleInstanceIdentity(ctx, serviceAccount, gcpClient)
	if err != nil {
		return nil, fmt.Errorf("GCP instance identity auth failed: %w", err)
	}

	// Set the session token we received
	client.SetSessionToken(authResp.SessionToken)
	metaLogger.Info(ctx, "Successfully authenticated to Coder via GCP instance identity")

	// Get build info to determine API version
	bi, err := client.SDK.BuildInfo(ctx)
	if err != nil {
		return nil, fmt.Errorf("get coder build version: %w", err)
	}

	var logFunc Func
	var closer func()
	var rpcClient proto.DRPCAgentClient20

	if semver.Compare(semver.MajorMinor(bi.Version), minAgentAPIV2) < 0 {
		metaLogger.Warn(ctx, "Detected Coder version incompatible with AgentAPI v2, falling back to deprecated API", slog.F("coder_version", bi.Version))
		logFunc, closer = sendLogsV1(ctx, client, metaLogger.Named("send_logs_v1"))
		// No RPC client available for lifecycle reporting in v1
	} else {
		// Create a new context so we can ensure the connection is torn down.
		connCtx, cancel := context.WithCancel(ctx)
		dac, err := initRPC(connCtx, client, metaLogger.Named("init_rpc"))
		if err != nil {
			cancel()
			return nil, fmt.Errorf("init coder rpc client: %w", err)
		}
		// Store the RPC client for lifecycle reporting
		rpcClient = dac

		ls := agentsdk.NewLogSender(metaLogger.Named("coder_log_sender"))
		metaLogger.Info(ctx, "Sending logs via AgentAPI v2", slog.F("coder_version", bi.Version))
		var loggerCloser func()
		logFunc, loggerCloser = sendLogsV2(connCtx, dac, ls, metaLogger.Named("send_logs_v2"))

		var closeOnce sync.Once
		closer = func() {
			loggerCloser()
			closeOnce.Do(func() {
				cancel()
				_ = dac.DRPCConn().Close()
			})
		}
	}

	return &CoderClient{
		client:    client,
		rpcClient: rpcClient,
		logger:    logFunc,
		closer:    closer,
	}, nil
}

type coderLogSender interface {
	Enqueue(uuid.UUID, ...agentsdk.Log)
	SendLoop(context.Context, agentsdk.LogDest) error
	Flush(uuid.UUID)
	WaitUntilEmpty(context.Context) error
}

func initClient(coderURL *url.URL, token string) *agentsdk.Client {
	client := agentsdk.New(coderURL)
	client.SetSessionToken(token)
	return client
}

func initRPC(ctx context.Context, client *agentsdk.Client, l slog.Logger) (proto.DRPCAgentClient20, error) {
	var c proto.DRPCAgentClient20
	var err error
	retryCtx, retryCancel := context.WithTimeout(ctx, rpcConnectTimeout)
	defer retryCancel()
	attempts := 0
	for r := retry.New(100*time.Millisecond, time.Second); r.Wait(retryCtx); {
		attempts++
		// Maximize compatibility.
		c, err = client.ConnectRPC20(ctx)
		if err != nil {
			l.Debug(ctx, "Failed to connect to Coder", slog.F("error", err), slog.F("attempt", attempts))
			continue
		}
		break
	}
	if c == nil {
		return nil, err
	}
	return proto.NewDRPCAgentClient(c.DRPCConn()), nil
}

// sendLogsV1 uses the PatchLogs endpoint to send logs.
// This is deprecated, but required for backward compatibility with older versions of Coder.
func sendLogsV1(ctx context.Context, client *agentsdk.Client, l slog.Logger) (logger Func, closer func()) {
	// nolint: staticcheck // required for backwards compatibility
	sendLog, flushAndClose := agentsdk.LogsSender(agentsdk.ExternalLogSourceID, client.PatchLogs, slog.Logger{})
	var mu sync.Mutex
	return func(lvl Level, msg string, args ...any) {
			log := agentsdk.Log{
				CreatedAt: time.Now(),
				Output:    fmt.Sprintf(msg, args...),
				Level:     codersdk.LogLevel(lvl),
			}
			mu.Lock()
			defer mu.Unlock()
			if err := sendLog(ctx, log); err != nil {
				l.Warn(ctx, "failed to send logs to Coder", slog.Error(err))
			}
		}, func() {
			ctx, cancel := context.WithTimeout(ctx, logSendGracePeriod)
			defer cancel()
			if err := flushAndClose(ctx); err != nil {
				l.Warn(ctx, "failed to flush logs", slog.Error(err))
			}
		}
}

// sendLogsV2 uses the v2 agent API to send logs. Only compatibile with coder versions >= 2.9.
func sendLogsV2(ctx context.Context, dest agentsdk.LogDest, ls coderLogSender, l slog.Logger) (logger Func, closer func()) {
	sendCtx, sendCancel := context.WithCancel(ctx)
	done := make(chan struct{})
	uid := uuid.New()
	go func() {
		defer close(done)
		if err := ls.SendLoop(sendCtx, dest); err != nil {
			if !errors.Is(err, context.Canceled) {
				l.Warn(ctx, "failed to send logs to Coder", slog.Error(err))
			}
		}
	}()

	var closeOnce sync.Once
	return func(l Level, msg string, args ...any) {
			ls.Enqueue(uid, agentsdk.Log{
				CreatedAt: time.Now(),
				Output:    fmt.Sprintf(msg, args...),
				Level:     codersdk.LogLevel(l),
			})
		}, func() {
			closeOnce.Do(func() {
				// Trigger a flush and wait for logs to be sent.
				ls.Flush(uid)
				ctx, cancel := context.WithTimeout(ctx, logSendGracePeriod)
				defer cancel()
				err := ls.WaitUntilEmpty(ctx)
				if err != nil {
					l.Warn(ctx, "log sender did not empty", slog.Error(err))
				}

				// Stop the send loop.
				sendCancel()
			})

			// Wait for the send loop to finish.
			<-done
		}
}
