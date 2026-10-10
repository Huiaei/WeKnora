package runtime

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/dig"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// stubUserService implements only the three methods RunStartupBootstrap
// touches; the embedded interface satisfies the rest of UserService so the
// stub keeps compiling when the interface grows.
type stubUserService struct {
	interfaces.UserService

	user *types.User
	err  error

	listTotal int64
	listErr   error

	updateErr error

	lookups []string
	updated []*types.User
}

func (s *stubUserService) GetUserByEmail(_ context.Context, email string) (*types.User, error) {
	s.lookups = append(s.lookups, email)
	return s.user, s.err
}

func (s *stubUserService) ListSystemAdmins(context.Context, int, int) ([]*types.User, int64, error) {
	return nil, s.listTotal, s.listErr
}

func (s *stubUserService) UpdateUser(_ context.Context, user *types.User) error {
	if s.updateErr != nil {
		return s.updateErr
	}
	s.updated = append(s.updated, user)
	return nil
}

type stubAPIKeyService struct {
	interfaces.TenantAPIKeyService

	calls int
	n     int
	err   error
}

func (s *stubAPIKeyService) BackfillMissingKeyHashes(context.Context) (int, error) {
	s.calls++
	return s.n, s.err
}

// newBootstrapContainer builds a throwaway dig container holding only the
// services the bootstrap resolves. A nil service is simply not provided, so
// the best-effort resolution-failure paths can be exercised too.
func newBootstrapContainer(t *testing.T, userSvc interfaces.UserService, keySvc interfaces.TenantAPIKeyService) *dig.Container {
	t.Helper()
	c := dig.New()
	if userSvc != nil {
		if err := c.Provide(func() interfaces.UserService { return userSvc }); err != nil {
			t.Fatalf("provide UserService: %v", err)
		}
	}
	if keySvc != nil {
		if err := c.Provide(func() interfaces.TenantAPIKeyService { return keySvc }); err != nil {
			t.Fatalf("provide TenantAPIKeyService: %v", err)
		}
	}
	return c
}

func TestRunStartupBootstrapPromotesNamedUserWhenNoAdminExists(t *testing.T) {
	t.Setenv(BootstrapSystemAdminEnvVar, "admin@weknora.local")

	user := &types.User{ID: "u1", Email: "admin@weknora.local"}
	users := &stubUserService{user: user}
	keys := &stubAPIKeyService{}

	RunStartupBootstrap(newBootstrapContainer(t, users, keys))

	if len(users.lookups) != 1 || users.lookups[0] != "admin@weknora.local" {
		t.Fatalf("lookups = %v, want exactly [admin@weknora.local]", users.lookups)
	}
	if !user.IsSystemAdmin {
		t.Fatal("user was not promoted to system admin")
	}
	if len(users.updated) != 1 || users.updated[0] != user {
		t.Fatalf("updated = %v, want exactly the looked-up user", users.updated)
	}
	if keys.calls != 1 {
		t.Fatalf("BackfillMissingKeyHashes calls = %d, want 1", keys.calls)
	}
}

func TestRunStartupBootstrapIsNoopWhenASystemAdminAlreadyExists(t *testing.T) {
	t.Setenv(BootstrapSystemAdminEnvVar, "admin@weknora.local")

	user := &types.User{ID: "u1", Email: "admin@weknora.local"}
	users := &stubUserService{user: user, listTotal: 1}

	RunStartupBootstrap(newBootstrapContainer(t, users, &stubAPIKeyService{}))

	if len(users.updated) != 0 {
		t.Fatalf("updated = %v, want none: an existing admin must not be joined by a second one", users.updated)
	}
	if user.IsSystemAdmin {
		t.Fatal("user was promoted even though a system admin already existed")
	}
}

func TestRunStartupBootstrapIsNoopWhenUserIsAlreadyAdmin(t *testing.T) {
	t.Setenv(BootstrapSystemAdminEnvVar, "admin@weknora.local")

	user := &types.User{ID: "u1", Email: "admin@weknora.local", IsSystemAdmin: true}
	users := &stubUserService{user: user, listTotal: 1}

	RunStartupBootstrap(newBootstrapContainer(t, users, &stubAPIKeyService{}))

	if len(users.updated) != 0 {
		t.Fatalf("updated = %v, want none for an idempotent re-run", users.updated)
	}
}

func TestRunStartupBootstrapSkipsPromotionWhenEnvVarIsUnset(t *testing.T) {
	t.Setenv(BootstrapSystemAdminEnvVar, "")

	users := &stubUserService{user: &types.User{ID: "u1", Email: "admin@weknora.local"}}
	keys := &stubAPIKeyService{}

	RunStartupBootstrap(newBootstrapContainer(t, users, keys))

	if len(users.lookups) != 0 || len(users.updated) != 0 {
		t.Fatalf("lookups = %v updated = %v, want none without %s", users.lookups, users.updated, BootstrapSystemAdminEnvVar)
	}
	// The legacy-hash backfill is unconditional: it is not a privilege grant.
	if keys.calls != 1 {
		t.Fatalf("BackfillMissingKeyHashes calls = %d, want 1", keys.calls)
	}
}

func TestRunStartupBootstrapDoesNotCreateAnUnknownUser(t *testing.T) {
	t.Setenv(BootstrapSystemAdminEnvVar, "nobody@weknora.local")

	// A user who has not signed up yet surfaces as an error here.
	users := &stubUserService{err: errors.New("record not found")}

	RunStartupBootstrap(newBootstrapContainer(t, users, &stubAPIKeyService{}))

	if len(users.updated) != 0 {
		t.Fatalf("updated = %v, want none: bootstrap must never create or promote an unknown account", users.updated)
	}
}

func TestRunStartupBootstrapFailsClosedWhenAdminListIsUnavailable(t *testing.T) {
	t.Setenv(BootstrapSystemAdminEnvVar, "admin@weknora.local")

	user := &types.User{ID: "u1", Email: "admin@weknora.local"}
	users := &stubUserService{user: user, listErr: errors.New("database is down")}

	RunStartupBootstrap(newBootstrapContainer(t, users, &stubAPIKeyService{}))

	// Being unable to prove that no admin exists must not grant privileges.
	if len(users.updated) != 0 || user.IsSystemAdmin {
		t.Fatal("user was promoted without verifying that no system admin exists")
	}
}

func TestRunStartupBootstrapSurvivesResolutionFailures(t *testing.T) {
	t.Setenv(BootstrapSystemAdminEnvVar, "admin@weknora.local")

	// Neither service is registered: both invokes fail and only warn.
	RunStartupBootstrap(newBootstrapContainer(t, nil, nil))
}

func TestRunStartupBootstrapSurvivesBackfillAndUpdateErrors(t *testing.T) {
	t.Setenv(BootstrapSystemAdminEnvVar, "admin@weknora.local")

	user := &types.User{ID: "u1", Email: "admin@weknora.local"}
	users := &stubUserService{user: user, updateErr: errors.New("write failed")}
	keys := &stubAPIKeyService{err: errors.New("backfill failed")}

	// Both failures are non-fatal: startup must not be aborted.
	RunStartupBootstrap(newBootstrapContainer(t, users, keys))

	if keys.calls != 1 {
		t.Fatalf("BackfillMissingKeyHashes calls = %d, want 1", keys.calls)
	}
	if len(users.updated) != 0 {
		t.Fatalf("updated = %v, want none after a write error", users.updated)
	}
}
