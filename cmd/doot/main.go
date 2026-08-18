// Command doot runs the Doot agent server.
//
// Subcommands:
//
//	doot          start the server
//	doot serve    start the server
//	doot migrate  apply migrations and exit
//	doot genkey   print a new DOOT_MASTER_KEY and exit
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/sumit-waani/doot/internal/agent"
	"github.com/sumit-waani/doot/internal/auth"
	"github.com/sumit-waani/doot/internal/bootstrap"
	"github.com/sumit-waani/doot/internal/config"
	"github.com/sumit-waani/doot/internal/db"
	"github.com/sumit-waani/doot/internal/events"
	"github.com/sumit-waani/doot/internal/project"
	"github.com/sumit-waani/doot/internal/secretbox"
	"github.com/sumit-waani/doot/internal/web"
)

func main() {
	command := "serve"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}

	// genkey needs no configuration, so it runs before anything is validated.
	if command == "genkey" {
		key, err := secretbox.GenerateKey()
		if err != nil {
			exit(err)
		}
		fmt.Println(key)
		return
	}

	env := config.LoadEnv()
	setupLogging(env)

	switch command {
	case "serve":
		if err := serve(env); err != nil {
			exit(err)
		}
	case "migrate":
		if err := migrateOnly(env); err != nil {
			exit(err)
		}
	default:
		exit(fmt.Errorf("unknown command %q (want: serve, migrate, genkey)", command))
	}
}

func serve(env config.Env) error {
	if err := env.Validate(); err != nil {
		return err
	}

	// Startup order is fixed. Each step must succeed before the next runs, and
	// the server does not accept traffic until all of them have.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	startCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	// 1. Connect.
	database, err := db.Open(startCtx, env.TursoDatabaseURL, env.TursoAuthToken)
	if err != nil {
		return err
	}
	defer database.Close()

	// 2. Migrate.
	if err := db.Migrate(startCtx, database); err != nil {
		return err
	}

	box, err := secretbox.FromBase64(env.MasterKey)
	if err != nil {
		return err
	}

	cfg, err := config.NewStore(startCtx, database, box)
	if err != nil {
		return err
	}
	if err := cfg.SeedSecretsFromEnv(startCtx, env.SeedSecrets); err != nil {
		return err
	}

	// 3-6. Default user, interrupted runs, pruning.
	boot, err := bootstrap.Run(startCtx, database, env.ResetAdmin)
	if err != nil {
		return err
	}

	eventLog := events.NewLog(database)

	projects := project.NewService(database, cfg, eventLog)
	agents := agent.NewService(database, cfg, eventLog, projects)
	// Unwinds any in-flight run so a deploy does not leave the loop mid-turn with
	// no process behind it.
	defer agents.Close()
	// Closing releases the Daytona client, including the state-event WebSocket
	// the SDK keeps open, and stops any sandbox heartbeat.
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := projects.Close(closeCtx); err != nil {
			slog.Warn("could not close Daytona client cleanly", "err", err)
		}
	}()

	server, err := web.NewServer(web.Options{
		DB:               database,
		Config:           cfg,
		Auth:             auth.NewService(database, !env.Dev),
		Events:           eventLog,
		Project:          projects,
		Agent:            agents,
		Dev:              env.Dev,
		UsingDefaultPass: boot.UsingDefaultPass,
	})
	if err != nil {
		return err
	}

	addr := fmt.Sprintf(":%d", env.Port)
	httpServer := &http.Server{
		Addr:    addr,
		Handler: server.Handler(),
		// No WriteTimeout: it would sever SSE streams, which are long-lived by
		// design. Idle and header timeouts still bound abusive connections.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	slog.Info("doot starting",
		"addr", addr,
		"backend", database.Kind,
		"dev", env.Dev,
		"created_default_user", boot.CreatedDefaultUser,
		"interrupted_runs", boot.InterruptedRuns,
	)
	if boot.UsingDefaultPass {
		slog.Warn("default password is still in use; change it from Settings")
	}

	// 7. Serve.
	serveErr := make(chan error, 1)
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		slog.Info("shutting down")
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	slog.Info("stopped")
	return nil
}

// migrateOnly applies migrations without starting the server, for verifying a
// schema change before deploying it.
func migrateOnly(env config.Env) error {
	if strings.TrimSpace(env.TursoDatabaseURL) == "" {
		return errors.New("TURSO_DATABASE_URL is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	database, err := db.Open(ctx, env.TursoDatabaseURL, env.TursoAuthToken)
	if err != nil {
		return err
	}
	defer database.Close()

	if err := db.Migrate(ctx, database); err != nil {
		return err
	}

	versions, err := db.AppliedVersions(ctx, database)
	if err != nil {
		return err
	}
	slog.Info("migrations up to date", "applied", versions)
	return nil
}

func setupLogging(env config.Env) {
	level := slog.LevelInfo
	switch strings.ToLower(env.LogLevel) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	var handler slog.Handler
	if env.Dev {
		handler = slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	} else {
		handler = slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	}
	slog.SetDefault(slog.New(handler))
}

func exit(err error) {
	slog.Error("fatal", "err", err)
	os.Exit(1)
}
