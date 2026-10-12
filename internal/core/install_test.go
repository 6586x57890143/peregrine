package core

import (
	"fmt"
	"testing"
)

func TestApplicationPatch(t *testing.T) {
	synced := map[string]integrationConfig{guildInstall: {OAuth2InstallParams: &installParams{
		Scopes:      []string{"bot", "applications.commands"},
		Permissions: fmt.Sprint(Permissions),
	}}}

	if p := applicationPatch(application{Flags: flagMessageContentLimited, IntegrationTypesConfig: synced}); p != nil {
		t.Errorf("an app that already matches got a patch: %v", p)
	}
	// Verified apps hold the unlimited bit, which must not be "fixed" with a limited one.
	if p := applicationPatch(application{Flags: flagMessageContent, IntegrationTypesConfig: synced}); p != nil {
		t.Errorf("a verified app got a patch: %v", p)
	}

	// A fresh app: no intent, no install settings. Unwritable flags are not sent back.
	const unwritable = 1 << 23
	p := applicationPatch(application{Flags: unwritable | flagGuildMembersLimited})
	if got, want := p["flags"], flagGuildMembersLimited|flagMessageContentLimited; got != want {
		t.Errorf("flags = %v, want %v", got, want)
	}
	if _, ok := p["integration_types_config"]; !ok {
		t.Error("missing install settings were not written")
	}

	// A stale permissions number is rewritten; the flag is left alone.
	stale := map[string]integrationConfig{guildInstall: {OAuth2InstallParams: &installParams{
		Scopes: []string{"bot"}, Permissions: "2048",
	}}}
	p = applicationPatch(application{Flags: flagMessageContentLimited, IntegrationTypesConfig: stale})
	if _, ok := p["flags"]; ok {
		t.Error("flags rewritten when the intent was already on")
	}
	if _, ok := p["integration_types_config"]; !ok {
		t.Error("stale install settings were not rewritten")
	}
}

func TestPermissionsNumber(t *testing.T) {
	// The README's fallback invite carries this number; update both together.
	if Permissions != 274878000192 {
		t.Errorf("Permissions = %d; update README.md's invite link to match", Permissions)
	}
}
