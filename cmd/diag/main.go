//go:build !bindings

// Command diag is a throwaway probe: it builds the same dependency graph the
// desktop app builds, but stops right after the router is constructed. No
// wails, no window, no HTTP listener - so any native crash here belongs to the
// container/DI side rather than the WebView2 frontend.
package main

import (
	"fmt"
	"os"

	"github.com/gin-gonic/gin"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/container"
	"github.com/Tencent/WeKnora/internal/handler"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/runtime"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

func main() {
	// Mirror cmd/desktop's ensureDesktopLiteEdition(). The desktop entry marks
	// the process as Lite at runtime (packaged builds also inject it via
	// ldflags), and router.go only registers the SPA handler when
	// Edition == "lite". Without this the probe would silently exercise the
	// standard configuration and skip exactly the static-file path the
	// portable package depends on.
	handler.Edition = "lite"

	logger.ConfigureFromEnv()

	step := func(format string, a ...any) {
		fmt.Printf("### %s\n", fmt.Sprintf(format, a...))
		os.Stdout.Sync()
	}

	step("edition=%s", handler.Edition)
	step("BuildContainer: start")
	c := container.BuildContainer(runtime.GetContainer())
	step("BuildContainer: done")

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

func countRoutes(r *gin.Engine) int {
	if r == nil {
		return 0
	}
	return len(r.Routes())
}