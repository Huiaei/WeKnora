// Bootstrap-time hooks that run after the DI container is built but
// before the HTTP server starts listening. These are deliberately
// best-effort: any failure here only warns and does NOT abort startup.
// The reasoning is that an operator running with a misconfigured env
// var should still be able to bring the server up (and fix the issue
// from the running instance) rather than have a typo brick the deploy.
//
// They live here rather than in cmd/server so that every binary which
// builds the container runs the same hooks: cmd/server calls
// RunStartupBootstrap from main(), and cmd/desktop does the same. That
// matters because the desktop/lite build is a single-user deployment in
// which the system-admin promotion below is the ONLY way a system
// administrator can ever come into existence — auto-setup deliberately
// registers a plain user, so without this call every platform-scoped
// page stays unreachable forever.
package runtime

import (
	"context"
	"os"
	"strings"

	"go.uber.org/dig"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// BootstrapSystemAdminEnvVar is the env var that names the email of the user
// who may be promoted to system administrator when the deployment has no
// existing system administrators.
//
// Exported because cmd/diag asserts the promotion end to end, and because
// scripts/check-portable-workflow.py greps the launcher for the same name;
// one literal, one place to change it.
//
// Why an env var (vs a CLI subcommand)?
//   - Zero-friction in docker-compose / k8s deploys: set it once in the
//     manifest and the very first user account that signs up with that
//     email is auto-promoted, with no extra ops step.
//   - Idempotent: if the user is already a system admin, bootstrapping is
//     a no-op.
//   - Safe to leave set: once at least one system admin exists, the env
//     var stops granting privileges. That prevents a UI revoke from being
//     silently undone on the next restart.
const BootstrapSystemAdminEnvVar = "WEKNORA_BOOTSTRAP_SYSTEM_ADMIN_EMAIL"

// RunStartupBootstrap consults the env and applies any one-shot
// bootstrap actions. Currently it only handles system-admin promotion;
// future bootstrap steps (default model seeding, etc.) can be added
// here as additional dig.Invoke calls.
//
// Call it once after container.BuildContainer and before the HTTP
// listener binds — from cmd/server/main.go and from cmd/desktop/main.go.
func RunStartupBootstrap(c *dig.Container) {
	ctx := context.Background()

	// Legacy hash repair for migration 000065 placeholder rows. Invoked each
	// startup but short-circuits with a cheap EXISTS once every row is
	// backfilled (no api_key decryption on the steady-state path).
	if err := c.Invoke(func(apiKeySvc interfaces.TenantAPIKeyService) {
		if n, err := apiKeySvc.BackfillMissingKeyHashes(ctx); err != nil {
			logger.Warnf(ctx, "[bootstrap] tenant api key hash backfill failed: %v", err)
		} else if n > 0 {
			logger.Infof(ctx, "[bootstrap] backfilled %d legacy tenant api key hash(es)", n)
		}
	}); err != nil {
		logger.Warnf(ctx, "[bootstrap] failed to resolve TenantAPIKeyService: %v", err)
	}

	email := strings.TrimSpace(os.Getenv(BootstrapSystemAdminEnvVar))
	if email == "" {
		return
	}
	// dig.Invoke resolves UserService from the container; if user
	// service registration is broken we want to know loudly, but still
	// not abort startup — bootstrap is best-effort.
	if err := c.Invoke(func(userSvc interfaces.UserService) {
		PromoteBootstrapSystemAdmin(ctx, userSvc, email)
	}); err != nil {
		logger.Warnf(ctx, "[bootstrap] failed to resolve UserService: %v", err)
	}
}

// PromoteBootstrapSystemAdmin promotes the account named by `email` to
// system administrator, but only while the deployment has no system
// administrator at all. It reports whether it actually promoted someone.
//
// Two callers need this, at two different moments:
//
//   - RunStartupBootstrap, on every start (cmd/server and cmd/desktop).
//   - AuthHandler.AutoSetup, immediately after it creates the first
//     lite-edition account. The startup hook has already run by then and
//     the account did not exist yet, so without this second call the very
//     first launch of a portable build would leave its only account
//     without platform access until the next restart.
//
// It is idempotent and never fatal: every error path warns and returns
// false. It intentionally does NOT create the account - registration is a
// workflow with side effects (password hashing, tenant provisioning,
// audit) that must not be short-circuited.
func PromoteBootstrapSystemAdmin(ctx context.Context, userSvc interfaces.UserService, email string) bool {
	email = strings.TrimSpace(email)
	if email == "" {
		return false
	}
	user, err := userSvc.GetUserByEmail(ctx, email)
	if err != nil {
		// "not found" surfaces as an error in this codebase; treat it
		// gently — operators commonly set the var before the user has
		// signed up. The next restart after registration will succeed.
		logger.Warnf(ctx,
			"[bootstrap] %s=%s: user lookup failed (have they signed up yet?): %v",
			BootstrapSystemAdminEnvVar, email, err)
		return false
	}
	if user == nil {
		logger.Warnf(ctx,
			"[bootstrap] %s=%s: no matching user (will retry on next restart)",
			BootstrapSystemAdminEnvVar, email)
		return false
	}
	if user.IsSystemAdmin {
		logger.Infof(ctx,
			"[bootstrap] %s=%s: user %s is already a system admin (no-op)",
			BootstrapSystemAdminEnvVar, email, user.ID)
		return false
	}
	_, total, err := userSvc.ListSystemAdmins(ctx, 0, 1)
	if err != nil {
		logger.Warnf(ctx,
			"[bootstrap] %s=%s: cannot verify existing system admins, skipping promotion: %v",
			BootstrapSystemAdminEnvVar, email, err)
		return false
	}
	if total > 0 {
		logger.Infof(ctx,
			"[bootstrap] %s=%s: %d system admin(s) already exist; not promoting user %s",
			BootstrapSystemAdminEnvVar, email, total, user.ID)
		return false
	}
	user.IsSystemAdmin = true
	if err := userSvc.UpdateUser(ctx, user); err != nil {
		logger.Warnf(ctx,
			"[bootstrap] %s=%s: failed to promote user %s: %v",
			BootstrapSystemAdminEnvVar, email, user.ID, err)
		return false
	}
	logger.Infof(ctx,
		"[bootstrap] promoted user %s (%s) to system admin via %s",
		user.ID, email, BootstrapSystemAdminEnvVar)
	return true
}
