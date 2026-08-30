package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestPairCodeAndDeviceOwnership(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "agora.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	user, err := db.GetOrCreateLocalUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().UTC().Add(time.Minute)
	if err := db.CreatePairCode(ctx, user.UserID, HashSecret("pair-secret"), expires); err != nil {
		t.Fatal(err)
	}
	owner, err := db.ConsumePairCode(ctx, HashSecret("pair-secret"), time.Now().UTC())
	if err != nil || owner != user.UserID {
		t.Fatalf("consume = %q, %v", owner, err)
	}
	if _, err := db.ConsumePairCode(ctx, HashSecret("pair-secret"), time.Now().UTC()); err == nil {
		t.Fatal("expected consumed code rejection")
	}
	if err := db.CreateDevice(ctx, Device{ID: "device-1", UserID: user.UserID, CredentialHash: HashSecret("credential"), CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	owned, err := db.UserOwnsDaemon(ctx, user.UserID, "device-1")
	if err != nil || !owned {
		t.Fatalf("ownership = %v, %v", owned, err)
	}
	if err := db.RevokeDevice(ctx, "device-1", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	owned, err = db.UserOwnsDaemon(ctx, user.UserID, "device-1")
	if err != nil || owned {
		t.Fatalf("revoked ownership = %v, %v", owned, err)
	}
	stillBelongs, err := db.UserHasDevice(ctx, user.UserID, "device-1")
	if err != nil || !stillBelongs {
		t.Fatalf("revoked device membership = %v, %v", stillBelongs, err)
	}
	if _, err := db.GetDeviceByCredentialHash(ctx, HashSecret("credential")); err != nil {
		t.Fatal(err)
	}
}

func TestWebhookTargets(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "agora.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	user, err := db.GetOrCreateLocalUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := db.CreateWebhookTarget(ctx, WebhookTarget{ID: "hook-1", UserID: user.UserID, Provider: "feishu", Label: "ops", URL: "https://example.test/hook", Enabled: true, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	values, err := db.ListWebhookTargets(ctx, user.UserID)
	if err != nil || len(values) != 1 || values[0].Provider != "feishu" || values[0].URL == "" {
		t.Fatalf("webhooks = %+v, %v", values, err)
	}
	if err := db.SetWebhookTargetEnabled(ctx, user.UserID, "hook-1", false); err != nil {
		t.Fatal(err)
	}
	value, err := db.GetWebhookTarget(ctx, user.UserID, "hook-1")
	if err != nil || value.Enabled {
		t.Fatalf("updated webhook = %+v, %v", value, err)
	}
	if err := db.DeleteWebhookTarget(ctx, user.UserID, "hook-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetWebhookTarget(ctx, user.UserID, "hook-1"); err == nil {
		t.Fatal("expected deleted webhook")
	}
}

func TestUpdateDeviceName(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "agora.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	user, err := db.GetOrCreateLocalUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateDevice(ctx, Device{ID: "device-1", UserID: user.UserID, CredentialHash: HashSecret("credential"), CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateDeviceName(ctx, "device-1", "home-server"); err != nil {
		t.Fatal(err)
	}
	values, err := db.ListDevices(ctx, user.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values[0].Name != "home-server" {
		t.Fatalf("listed devices = %+v, want one device named home-server", values)
	}
	// Renaming an unknown device is a no-op, not an error.
	if err := db.UpdateDeviceName(ctx, "device-nope", "ignored"); err != nil {
		t.Fatalf("rename unknown device: %v", err)
	}
}
