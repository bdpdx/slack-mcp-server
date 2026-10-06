package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bdpdx/slack-mcp-server/pkg/agentchat"
	"github.com/bdpdx/slack-mcp-server/pkg/filesdir"
	"github.com/bdpdx/slack-mcp-server/pkg/provider"
	"github.com/bdpdx/slack-mcp-server/pkg/server"
	"github.com/bdpdx/slack-mcp-server/pkg/setup"
	"github.com/bdpdx/slack-mcp-server/pkg/toolconfig"
	"github.com/mattn/go-isatty"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var defaultSseHost = "127.0.0.1"
var defaultSsePort = 13080

func main() {
	if len(os.Args) > 1 && os.Args[1] == "chat" {
		os.Exit(agentchat.RunCLI(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}

	if len(os.Args) > 1 && os.Args[1] == "setup" {
		os.Exit(setup.Main(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "uninstall" {
		os.Exit(setup.UninstallMain(os.Args[2:]))
	}

	var transport string
	var enabledToolsFlag string
	var noCache bool
	var envFile string
	flag.StringVar(&transport, "t", "stdio", "Transport type (stdio, sse or http)")
	flag.StringVar(&transport, "transport", "stdio", "Transport type (stdio, sse or http)")
	flag.StringVar(&enabledToolsFlag, "e", "", "Comma-separated list of enabled tools (empty = every read tool, plus write tools enabled by their SLACK_MCP_*_TOOL settings)")
	flag.StringVar(&enabledToolsFlag, "enabled-tools", "", "Comma-separated list of enabled tools (empty = every read tool, plus write tools enabled by their SLACK_MCP_*_TOOL settings)")
	flag.BoolVar(&noCache, "no-cache", false, "Skip user/channel cache loading on startup for faster initialization. Lookups by #channel-name or @username will not work; use channel/user IDs instead.")
	flag.StringVar(&envFile, "env-file", "", "Path to the slack-mcp-server.env file (default: detected from the Codex or Claude Code session)")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage of %s (MCP server):\n", os.Args[0])
		flag.PrintDefaults()
		fmt.Fprintf(flag.CommandLine.Output(), "\nAgent chat helpers: %s chat --help\n", os.Args[0])
	}
	flag.Parse()

	envPath, err := agentchat.ResolveEnvFile(envFile, os.Getenv)
	if err == nil {
		err = agentchat.LoadEnvFile(envPath)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "slack-mcp-server: %v\n", err)
		os.Exit(1)
	}

	if enabledToolsFlag == "" {
		enabledToolsFlag = os.Getenv("SLACK_MCP_ENABLED_TOOLS")
	}

	var enabledTools []string
	if enabledToolsFlag != "" {
		for _, tool := range strings.Split(enabledToolsFlag, ",") {
			tool = strings.TrimSpace(tool)
			if tool != "" {
				enabledTools = append(enabledTools, tool)
			}
		}
	}

	logger, err := newLogger(transport)
	if err != nil {
		panic(err)
	}
	defer logger.Sync()

	err = server.ValidateEnabledTools(enabledTools)
	if err != nil {
		logger.Fatal("error in SLACK_MCP_ENABLED_TOOLS",
			zap.String("context", "console"),
			zap.Error(err),
		)
	}

	toolCfg, err := toolconfig.Load(enabledTools, os.Getenv)
	if err != nil {
		logger.Fatal("invalid tool configuration",
			zap.String("context", "console"),
			zap.Error(err),
		)
	}

	// The one folder files move through between Slack and this machine.
	// Without it the server still runs; saving downloads and uploading from
	// a path are unavailable.
	filesDir, err := filesdir.Path(os.Getenv)
	if err == nil {
		err = filesdir.Ensure(filesDir)
	}
	if err != nil {
		logger.Warn("files folder unavailable; attachment saving and path uploads are disabled",
			zap.String("context", "console"),
			zap.Error(err),
		)
	} else {
		toolCfg.FilesDir = filesDir
	}

	// Validate the network transport before contacting Slack: sse/http
	// refuse to start without an API key unless explicitly allowed on a
	// loopback address.
	var host, port string
	var httpSecurity *server.HTTPSecurity
	switch transport {
	case "stdio":
	case "sse", "http":
		host = os.Getenv("SLACK_MCP_HOST")
		if host == "" {
			host = defaultSseHost
		}
		port = os.Getenv("SLACK_MCP_PORT")
		if port == "" {
			port = strconv.Itoa(defaultSsePort)
		}
		httpSecurity, err = server.NewHTTPSecurity(host, port, os.Getenv, logger)
		if err != nil {
			logger.Fatal("Insecure transport configuration",
				zap.String("context", "console"),
				zap.Error(err),
			)
		}
	default:
		logger.Fatal("Invalid transport type",
			zap.String("context", "console"),
			zap.String("transport", transport),
			zap.String("allowed", "stdio, sse, http"),
		)
	}

	p := provider.New(transport, logger)
	s := server.NewMCPServer(p, logger, toolCfg)

	if noCache {
		p.SkipCache()
		logger.Info("Cache loading disabled via --no-cache flag",
			zap.String("context", "console"),
		)
	} else {
		go func() {
			var once sync.Once

			newUsersWatcher(p, &once, logger)()
			newChannelsWatcher(p, &once, logger)()
		}()
	}

	switch transport {
	case "stdio":
		// Wait for caches to be ready before accepting stdio requests.
		// With stale-while-revalidate this exits in one tick (~100ms).
		// On cold start (no cache), this blocks until the initial fetch completes.
		for {
			if ready, _ := p.IsReady(); ready {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err := s.ServeStdio(); err != nil {
			logger.Fatal("Server error",
				zap.String("context", "console"),
				zap.Error(err),
			)
		}
	case "sse", "http":
		endpoint := net.JoinHostPort(host, port)
		if transport == "sse" {
			endpoint += "/sse"
		} else {
			endpoint += "/mcp"
		}
		logger.Info(
			fmt.Sprintf("%s server listening on %s", strings.ToUpper(transport), endpoint),
			zap.String("context", "console"),
			zap.String("host", host),
			zap.String("port", port),
		)

		if ready, _ := p.IsReady(); !ready {
			logger.Info("Slack MCP Server is still warming up caches",
				zap.String("context", "console"),
			)
		}

		var err error
		if transport == "sse" {
			err = s.ListenAndServeSSE(host, port, httpSecurity)
		} else {
			err = s.ListenAndServeHTTP(host, port, httpSecurity)
		}
		if err != nil {
			logger.Fatal("Server error",
				zap.String("context", "console"),
				zap.Error(err),
			)
		}
	default:
		logger.Fatal("Invalid transport type",
			zap.String("context", "console"),
			zap.String("transport", transport),
			zap.String("allowed", "stdio, sse, http"),
		)
	}
}

func newUsersWatcher(p *provider.ApiProvider, once *sync.Once, logger *zap.Logger) func() {
	return func() {
		logger.Info("Caching users collection...",
			zap.String("context", "console"),
		)
		refreshWithRetry("users", p.RefreshUsers, logger)
		announceReady(p, once, logger)
	}
}

func newChannelsWatcher(p *provider.ApiProvider, once *sync.Once, logger *zap.Logger) func() {
	return func() {
		logger.Info("Caching channels collection...",
			zap.String("context", "console"),
		)
		refreshWithRetry("channels", p.RefreshChannels, logger)
		announceReady(p, once, logger)
	}
}

func announceReady(p *provider.ApiProvider, once *sync.Once, logger *zap.Logger) {
	if ready, _ := p.IsReady(); ready {
		once.Do(func() {
			logger.Info("Slack MCP Server is fully ready",
				zap.String("context", "console"),
			)
		})
	}
}

var (
	cacheRetryInitialDelay = 5 * time.Second
	cacheRetryMaxDelay     = 5 * time.Minute
)

// refreshWithRetry runs a cache refresh until it succeeds, logging failures
// and backing off exponentially. It runs in a background goroutine, so it
// must never exit the process.
func refreshWithRetry(name string, refresh func(context.Context) error, logger *zap.Logger) {
	delay := cacheRetryInitialDelay
	for attempt := 1; ; attempt++ {
		err := refresh(context.Background())
		if err == nil {
			return
		}
		logger.Error("Failed to load Slack cache, will retry",
			zap.String("context", "console"),
			zap.String("cache", name),
			zap.Int("attempt", attempt),
			zap.Duration("retry_in", delay),
			zap.Error(err),
		)
		time.Sleep(delay)
		delay = min(delay*2, cacheRetryMaxDelay)
	}
}

func newLogger(transport string) (*zap.Logger, error) {
	atomicLevel := zap.NewAtomicLevelAt(zap.InfoLevel)
	if envLevel := os.Getenv("SLACK_MCP_LOG_LEVEL"); envLevel != "" {
		if err := atomicLevel.UnmarshalText([]byte(envLevel)); err != nil {
			// stdout carries the JSON-RPC stream on stdio; never write to it.
			fmt.Fprintf(os.Stderr, "Invalid log level '%s': %v, using 'info'\n", envLevel, err)
		}
	}

	useJSON := shouldUseJSONFormat()
	useColors := shouldUseColors() && !useJSON

	outputPath := "stdout"
	if transport == "stdio" {
		outputPath = "stderr"
	}

	var config zap.Config

	if useJSON {
		config = zap.Config{
			Level:            atomicLevel,
			Development:      false,
			Encoding:         "json",
			OutputPaths:      []string{outputPath},
			ErrorOutputPaths: []string{"stderr"},
			EncoderConfig: zapcore.EncoderConfig{
				TimeKey:       "timestamp",
				LevelKey:      "level",
				NameKey:       "logger",
				MessageKey:    "message",
				StacktraceKey: "stacktrace",
				EncodeLevel:   zapcore.LowercaseLevelEncoder,
				EncodeTime:    zapcore.RFC3339TimeEncoder,
				EncodeCaller:  zapcore.ShortCallerEncoder,
			},
		}
	} else {
		config = zap.Config{
			Level:            atomicLevel,
			Development:      true,
			Encoding:         "console",
			OutputPaths:      []string{outputPath},
			ErrorOutputPaths: []string{"stderr"},
			EncoderConfig: zapcore.EncoderConfig{
				TimeKey:          "timestamp",
				LevelKey:         "level",
				NameKey:          "logger",
				MessageKey:       "msg",
				StacktraceKey:    "stacktrace",
				EncodeLevel:      getConsoleLevelEncoder(useColors),
				EncodeTime:       zapcore.ISO8601TimeEncoder,
				EncodeCaller:     zapcore.ShortCallerEncoder,
				ConsoleSeparator: " | ",
			},
		}
	}

	logger, err := config.Build(zap.AddCaller())
	if err != nil {
		return nil, err
	}

	logger = logger.With(zap.String("app", "slack-mcp-server"))

	return logger, err
}

// shouldUseJSONFormat determines if JSON format should be used
func shouldUseJSONFormat() bool {
	if format := os.Getenv("SLACK_MCP_LOG_FORMAT"); format != "" {
		return strings.ToLower(format) == "json"
	}

	if env := os.Getenv("ENVIRONMENT"); env != "" {
		switch strings.ToLower(env) {
		case "production", "prod", "staging":
			return true
		case "development", "dev", "local":
			return false
		}
	}

	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" ||
		os.Getenv("DOCKER_CONTAINER") != "" ||
		os.Getenv("container") != "" {
		return true
	}

	if !isatty.IsTerminal(os.Stdout.Fd()) {
		return true
	}

	return false
}

func shouldUseColors() bool {
	if colorEnv := os.Getenv("SLACK_MCP_LOG_COLOR"); colorEnv != "" {
		return colorEnv == "true" || colorEnv == "1"
	}

	if os.Getenv("NO_COLOR") != "" {
		return false
	}

	if os.Getenv("FORCE_COLOR") != "" {
		return true
	}

	if env := os.Getenv("ENVIRONMENT"); env == "development" || env == "dev" {
		return isatty.IsTerminal(os.Stdout.Fd())
	}

	return isatty.IsTerminal(os.Stdout.Fd())
}

func getConsoleLevelEncoder(useColors bool) zapcore.LevelEncoder {
	if useColors {
		return zapcore.CapitalColorLevelEncoder
	}
	return zapcore.CapitalLevelEncoder
}
