package core

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/bwmarrin/discordgo"
)

// Permissions is what adding peregrine to a server asks for, and the one place that says so.
// SyncApplication writes it into the app's Default Install Settings at boot, which is what
// Discord's bare install link (and peregrine.skua.lol, which redirects to it) asks a server
// for. A hand-built invite URL with a permissions number in it went stale the first time a
// feature needed a new bit; this cannot, because the deployed binary is the source.
const Permissions = discordgo.PermissionViewChannel |
	discordgo.PermissionReadMessageHistory |
	discordgo.PermissionSendMessages |
	discordgo.PermissionSendMessagesInThreads |
	discordgo.PermissionEmbedLinks | // leaderboard and wheel embeds, image previews
	discordgo.PermissionAddReactions |
	discordgo.PermissionManageMessages // tidying its own word-game posts

// installScopes: slash commands are registered globally, so the install asks for both.
var installScopes = []string{"applications.commands", "bot"}

// Application flags (Discord's application object). Only the _LIMITED intent flags are
// writable through the API; they are how an unverified app (under 100 servers) holds a
// privileged intent. The unlimited bit is what verification grants and cannot be self-set.
const (
	flagPresenceLimited       = 1 << 13
	flagGuildMembersLimited   = 1 << 15
	flagMessageContent        = 1 << 18
	flagMessageContentLimited = 1 << 19
	writableFlags             = flagPresenceLimited | flagGuildMembersLimited | flagMessageContentLimited
)

// guildInstall is integration type 0. discordgo v0.29 tags this field `integration_types`
// rather than Discord's `integration_types_config`, so its Application struct always reads
// it as empty; hence the local types.
const guildInstall = "0"

type installParams struct {
	Scopes      []string `json:"scopes"`
	Permissions string   `json:"permissions"`
}

type integrationConfig struct {
	OAuth2InstallParams *installParams `json:"oauth2_install_params,omitempty"`
}

type application struct {
	Flags                  int                          `json:"flags"`
	IntegrationTypesConfig map[string]integrationConfig `json:"integration_types_config"`
}

// applicationPatch is what SyncApplication would send to make app match this binary, or nil
// when it already does. Pure, so the decision is tested without Discord.
func applicationPatch(app application) map[string]any {
	patch := map[string]any{}
	if app.Flags&(flagMessageContent|flagMessageContentLimited) == 0 {
		// Send only the writable bits, keeping any limited intent somebody ticked by hand.
		patch["flags"] = app.Flags&writableFlags | flagMessageContentLimited
	}
	want := installParams{Scopes: installScopes, Permissions: fmt.Sprint(Permissions)}
	have, ok := app.IntegrationTypesConfig[guildInstall]
	if !ok || len(app.IntegrationTypesConfig) != 1 || have.OAuth2InstallParams == nil ||
		have.OAuth2InstallParams.Permissions != want.Permissions ||
		!slices.Equal(slices.Sorted(slices.Values(have.OAuth2InstallParams.Scopes)), installScopes) {
		// Guild install only: every command is guild-only and DMs are not learned.
		patch["integration_types_config"] = map[string]integrationConfig{guildInstall: {OAuth2InstallParams: &want}}
	}
	if len(patch) == 0 {
		return nil
	}
	return patch
}

// SyncApplication makes the Discord application match what this binary needs: the Message
// Content intent switched on, and Default Install Settings asking for Permissions. It runs
// before Open so a fresh app identifies with the intent already granted instead of failing
// with 4014. It writes only when Discord's copy differs, so a restart costs one GET, and
// returns the fields it changed.
//
// Not routed through discordguard: this is configuration of the app itself, not of any guild,
// and happens before there is a guard. A verified app cannot self-grant the intent; that PATCH
// fails, the caller logs it, and WatchReady still names the checkbox.
func SyncApplication(s *discordgo.Session) ([]string, error) {
	endpoint := discordgo.EndpointApplications + "/@me"
	body, err := s.RequestWithBucketID("GET", endpoint, nil, endpoint)
	if err != nil {
		return nil, fmt.Errorf("read application: %w", err)
	}
	var app application
	if err := json.Unmarshal(body, &app); err != nil {
		return nil, fmt.Errorf("decode application: %w", err)
	}
	patch := applicationPatch(app)
	if patch == nil {
		return nil, nil
	}
	if _, err := s.RequestWithBucketID("PATCH", endpoint, patch, endpoint); err != nil {
		return nil, fmt.Errorf("update application: %w", err)
	}
	return slices.Sorted(maps.Keys(patch)), nil
}
