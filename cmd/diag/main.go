//go:build !bindings

// Command diag is a throwaway probe: it builds the same dependency graph the
// desktop app builds, but stops right after the router is constructed. No
// wails, no window, no HTTP listener - so any native crash here belongs to the
// container/DI side rather than the WebView2 frontend.
//
// It also replays the first launch of a portable build, because the desktop
// binary itself cannot be exercised on Linux (its DuckDB cgo layer segfaults
// there) and the failure we care about is silent: auto-setup creates the only
// account as a plain user, the is_system_admin gate then hides the whole
// platform section, and nothing in the build log looks wrong. Setting
// DIAG_BOOTSTRAP_CHECK=1 makes the probe run that sequence against a real
// database and a real router - startup hook, then POST /auth/auto-setup - and
// fail unless the account comes back as a system administrator.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/dig"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/container"
	"github.com/Tencent/WeKnora/internal/handler"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/runtime"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// diagSetupToken stands in for the per-process token cmd/desktop generates and
// hands to the webview; the handler only compares it against the registered
// value, so any non-empty constant works.
const diagSetupToken = "diag-desktop-setup-token"

func step(format string, a ...any) {
	fmt.Printf("### %s\n", fmt.Sprintf(format, a...))
	os.Stdout.Sync()
}

func main() {
	// Mirror cmd/desktop's ensureDesktopLiteEdition(). The desktop entry marks
	// the process as Lite at runtime (packaged builds also inject it via
	// ldflags), and router.go only registers the SPA handler when
	// Edition == "lite". Without this the probe would silently exercise the
	// standard configuration and skip exactly the static-file path the
	// portable package depends on.
	handler.Edition = "lite"

	logger.ConfigureFromEnv()

	step("edition=%s", handler.Edition)
	step("BuildContainer: start")
	c := container.BuildContainer(runtime.GetContainer())
	step("BuildContainer: done")

	// Same hook, at the same point in startup, as cmd/server and cmd/desktop.
	// Without the env var this is a no-op apart from the api-key backfill.
	runtime.RunStartupBootstrap(c)

	if os.Getenv("DIAG_BOOTSTRAP_CHECK") == "1" {
		if err := checkSystemAdminBootstrap(c); err != nil {
			step("BOOTSTRAP CHECK FAILED: %v", err)
			os.Exit(1)
		}
		step("BOOTSTRAP OK - first launch ends with exactly one system admin")
	}

	done := make(chan error, 1)
	go func() {
		step("Invoke: resolving router graph...")
		err := c.Invoke(func(
			cfg *config.Config,
			router *gin.Engine,
			_ interfaces.ResourceCleaner,
		) error {
			step("Invoke: RESOLVED OK (routes=%d)", countRoutes(router))
			return nil
		})
		done <- err
	}()

	err := <-done
	if err != nil {
		step("Invoke: FAILED: %v", err)
		os.Exit(1)
	}
	step("ALL GOOD - no crash in DI graph")
}

// checkSystemAdminBootstrap replays, in order, exactly what the portable
// package does on its first launch - on a database that has never been used:
//
//  1. the shell runs the startup hook before the window opens (nothing exists
//     yet, so nothing may be created and nothing may fail);
//  2. the webview loads and the frontend POSTs /api/v1/auth/auto-setup with
//     the native token; the account it creates must already be a system
//     administrator, otherwise the platform section stays hidden until the
//     next restart;
//  3. the hook runs again on the next start and must be a no-op, leaving
//     exactly one system administrator behind.
func checkSystemAdminBootstrap(c *dig.Container) error {
	ctx := context.Background()
	envVar := runtime.BootstrapSystemAdminEnvVar
	email := strings.TrimSpace(os.Getenv(envVar))
	if email == "" {
		return fmt.Errorf("%s is not set", envVar)
	}

	// 1. Startup hook, before anybody has signed up.
	runtime.RunStartupBootstrap(c)
	if err := c.Invoke(func(userSvc interfaces.UserService) error {
		if u, err := userSvc.GetUserByEmail(ctx, email); err == nil && u != nil {
			return fmt.Errorf("user %s already exists; the check needs a fresh database", email)
		}
		return nil
	}); err != nil {
		return err
	}

	// 2. The first-launch request path, through the real router and the real
	//    handler, so this also covers the env-var gate inside AutoSetup.
	handler.SetLiteSetupToken(diagSetupToken)
	var engine *gin.Engine
	if err := c.Invoke(func(r *gin.Engine) { engine = r }); err != nil {
		return fmt.Errorf("resolve router: %w", err)
	}
	if engine == nil {
		return fmt.Errorf("router is nil")
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/auto-setup", nil)
	req.Header.Set("X-WeKnora-Desktop-Token", diagSetupToken)
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return fmt.Errorf("POST /api/v1/auth/auto-setup: status %d: %s",
			rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	var resp struct {
		Success bool `json:"success"`
		User    *struct {
			Email         string `json:"email"`
			IsSystemAdmin bool   `json:"is_system_admin"`
		} `json:"user"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		return fmt.Errorf("decode the auto-setup response: %w", err)
	}
	if resp.User == nil {
		return fmt.Errorf("the auto-setup response carries no user: %s", strings.TrimSpace(rec.Body.String()))
	}
	if !resp.User.IsSystemAdmin {
		return fmt.Errorf(
			"auto-setup returned is_system_admin=false for %s: the only account in the "+
				"deployment cannot reach the platform section", resp.User.Email)
	}
	step("BOOTSTRAP: auto-setup returned %s as a system admin", resp.User.Email)

	// 3. Next start: the hook must be a no-op and the deployment must be left
	//    with exactly one system administrator.
	runtime.RunStartupBootstrap(c)
	return c.Invoke(func(userSvc interfaces.UserService) error {
		_, total, err := userSvc.ListSystemAdmins(ctx, 0, 10)
		if err != nil {
			return fmt.Errorf("list system admins: %w", err)
		}
		if total != 1 {
			return fmt.Errorf("system admin count = %d, want exactly 1", total)
		}
		u, err := userSvc.GetUserByEmail(ctx, email)
		if err != nil || u == nil || !u.IsSystemAdmin {
			return fmt.Errorf("user %s is not a system admin after the whole sequence", email)
		}
		return nil
	})
}

func countRoutes(r *gin.Engine) int {
	if r == nil {
		return 0
	}
	return len(r.Routes())
}
