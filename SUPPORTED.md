# whatsmeow → MCP tool coverage

A snapshot of which `whatsmeow.Client` (and admin) capabilities are exposed
as MCP tools today, and which are not. The "Tool" column is the registered
MCP name; "—" means no tool. Keep this file in sync when adding new tools.

Last reviewed: 2026-04-27 against `go.mau.fi/whatsmeow v0.0.0-20260421083005`.

---

## Supported

### Cache-backed read tools (`internal/mcptools`)

These read from the local SQLite cache; whatsmeow itself isn't called.

| Capability                           | Tool                          |
| ------------------------------------ | ----------------------------- |
| Browse cached chats                  | `list_chats`                  |
| Single chat lookup                   | `get_chat`                    |
| List messages in a chat              | `list_messages`               |
| Surrounding context for a message    | `get_message_context`         |
| Most recent interaction with someone | `get_last_interaction`        |
| Chats involving a contact            | `get_contact_chats`           |
| Direct chat by contact JID/number    | `get_direct_chat_by_contact`  |

### whatsmeow-backed tools (`internal/tools`)

| Capability                                   | Tool                  | whatsmeow / admin call                                        |
| -------------------------------------------- | --------------------- | ------------------------------------------------------------- |
| Health check                                 | `ping`                | (custom)                                                      |
| Cache diagnostic snapshot                    | `cache_sync_status`   | (custom; reads `Ingestor.LastEventAt` + table counts)         |
| Search cached contacts                       | `search_contacts`     | cache only                                                    |
| List all cached contacts                     | `list_all_contacts`   | cache only                                                    |
| Contact details (cache + live status / pic)  | `get_contact_details` | `UserInfo`, `IsOnWhatsApp`, `ProfilePictureURL`               |
| Authoritative group metadata                 | `get_group_info`      | `GetGroupInfo`                                                |
| Send text message                            | `send_message`        | `SendMessage` (text only)                                     |
| Start a pair flow                            | `pairing_start`       | `StartPairing`, `PairPhone`                                   |
| Poll an in-progress pair flow                | `pairing_complete`    | `PairWaitNext` / `PairLatest`                                 |

### Cache ingestion (no tool — runs from the dispatcher)

Wired in `internal/cache/handler.go::HandleEvent`; populates the cache as
events arrive.

| whatsmeow event             | Persisted as                                   |
| --------------------------- | ---------------------------------------------- |
| `*events.Message`           | message row + chat upsert + sender contact     |
| `*events.HistorySync`       | bulk message + chat backfill                   |
| `*events.Contact`           | contact upsert (full/first name)               |
| `*events.PushName`          | contact upsert (push name)                     |
| `*events.BusinessName`      | contact upsert (business name)                 |
| `*events.GroupInfo`         | chat upsert (name, ts)                         |
| `*events.JoinedGroup`       | chat upsert (`group` or `community`)           |
| `*events.NewsletterJoin`    | chat upsert (`newsletter`)                     |
| `*events.NewsletterLeave`   | chat upsert (row preserved)                    |
| `*events.MarkChatAsRead`    | unread flag                                    |
| `*events.Pin`               | pinned flag                                    |
| `*events.Archive`           | archived flag                                  |
| `*events.Star`              | chat row only (no `messages.starred` yet)      |

### Admin HTTP endpoints (`internal/admin`, not MCP)

| Endpoint                | Backed by                         |
| ----------------------- | --------------------------------- |
| `GET  /admin/health`    | `wa.Client.Status`                |
| `GET  /admin/state`     | session lifecycle SSE             |
| `POST /admin/pair/start`| `wa.Client.StartPairing`          |
| `POST /admin/pair/phone`| `wa.Client.PairPhone`             |
| `POST /admin/unpair`    | `wa.Client.Unpair` (i.e. Logout)  |

---

## Not yet supported

The list below is exhaustive over `whatsmeow.Client` exported methods that
take user-meaningful action (excluding internal protocol helpers, build-only
helpers, decrypt/encrypt, retry plumbing, network/proxy setters). ⭐ marks
the highest user-value gaps.

### Outbound messaging (beyond plain text)

| whatsmeow                                           | Notes                                                              |
| --------------------------------------------------- | ------------------------------------------------------------------ |
| ⭐ `SendMessage` for media envelopes                | image/video/audio/document/sticker. Today only text is supported. |
| ⭐ `BuildReaction` + `SendMessage`                  | react with an emoji.                                               |
| ⭐ `RevokeMessage` (`BuildRevoke` + send)           | delete-for-everyone.                                               |
| ⭐ `BuildEdit` + `SendMessage`                      | edit a previously sent message.                                    |
| `BuildPollCreation` + `BuildPollVote`               | polls.                                                             |
| `MarkRead`                                          | send read receipts.                                                |
| `SendChatPresence`                                  | typing / recording / paused indicator.                             |
| `SendPresence`                                      | global online/offline.                                             |
| `SubscribePresence`                                 | get notified when a JID comes online.                              |
| `SetDisappearingTimer`                              | per-chat ephemerals.                                               |
| `SetDefaultDisappearingTimer`                       | account-wide default.                                              |

### Newsletter / channel management

| whatsmeow                                | Notes                                                                  |
| ---------------------------------------- | ---------------------------------------------------------------------- |
| ⭐ `FollowNewsletter`                    | subscribe to a channel by JID.                                         |
| ⭐ `UnfollowNewsletter`                  | unsubscribe.                                                           |
| `GetSubscribedNewsletters`               | authoritative list (compare against the cache for reconciliation).     |
| `GetNewsletterInfo`                      | metadata for a known JID.                                              |
| `GetNewsletterInfoWithInvite`            | metadata via an invite link.                                           |
| `GetNewsletterMessages`                  | fetch messages directly from the channel feed.                         |
| `GetNewsletterMessageUpdates`            | poll for updates.                                                      |
| `NewsletterMarkViewed`                   | mark a newsletter message as viewed.                                   |
| `NewsletterSendReaction`                 | react to a newsletter message.                                         |
| `NewsletterToggleMute`                   | mute/unmute.                                                           |
| `NewsletterSubscribeLiveUpdates`         | live-mode subscription.                                                |
| `CreateNewsletter`                       | author your own channel.                                               |
| `UploadNewsletter` / `UploadNewsletterReader` | media uploads scoped to the newsletter feed.                      |

### Group administration

| whatsmeow                                              | Notes                                                  |
| ------------------------------------------------------ | ------------------------------------------------------ |
| ⭐ `LeaveGroup`                                        | leave a group.                                         |
| ⭐ `JoinGroupWithLink`                                 | accept an invite link.                                 |
| ⭐ `JoinGroupWithInvite`                               | accept an admin-generated invite.                      |
| `GetJoinedGroups`                                      | authoritative groups list (vs. cache).                 |
| `CreateGroup`                                          | create a new group.                                    |
| `UpdateGroupParticipants`                              | add / remove / promote / demote.                       |
| `SetGroupName`, `SetGroupTopic`, `SetGroupDescription` | metadata edits.                                        |
| `SetGroupAnnounce`                                     | only-admins-can-message.                               |
| `SetGroupLocked`                                       | only-admins-can-edit-info.                             |
| `SetGroupPhoto`                                        | change group avatar.                                   |
| `SetGroupMemberAddMode`                                | who can add members.                                   |
| `SetGroupJoinApprovalMode`                             | toggle join requests.                                  |
| `GetGroupInviteLink`                                   | retrieve / rotate the invite link.                     |
| `GetGroupInfoFromInvite`                               | preview a group from an invite token.                  |
| `GetGroupInfoFromLink`                                 | preview a group from a link.                           |
| `GetGroupRequestParticipants`                          | pending join requests.                                 |
| `UpdateGroupRequestParticipants`                       | approve / reject join requests.                        |
| `LinkGroup` / `UnlinkGroup`                            | community parent ⇄ subgroup wiring.                    |
| `GetSubGroups`                                         | enumerate community subgroups.                         |
| `GetLinkedGroupsParticipants`                          | community-wide member view.                            |

### Media

| whatsmeow                                                                  | Notes                                                          |
| -------------------------------------------------------------------------- | -------------------------------------------------------------- |
| ⭐ `Download` / `DownloadAny` / `DownloadToFile` / `DownloadThumbnail`     | fetch the bytes for cached image/video/audio/document rows.    |
| `DownloadHistorySync`                                                      | large blob retrieval.                                          |
| `DownloadMediaWithPath` / `DownloadMediaWithPathToFile`                    | typed CDN-backed download.                                     |
| `DownloadFB` / `DownloadFBToFile`                                          | Facebook CDN variant.                                          |
| `Upload` / `UploadReader`                                                  | required by any media-send tool.                               |
| `DeleteMedia`                                                              | server-side delete.                                            |

### Identity & contacts (partial today)

| whatsmeow                            | Notes                                                       |
| ------------------------------------ | ----------------------------------------------------------- |
| `GetBusinessProfile`                 | richer business metadata than `get_contact_details`.        |
| `GetUserDevices` / `GetUserDevicesContext` | list paired devices for a JID.                        |
| `GetProfilePictureInfo`              | full picture metadata (we only expose the URL today).       |
| `ResolveBusinessMessageLink`         | resolve a `wa.me/message/...` link.                         |
| `ResolveContactQRLink`               | resolve a contact QR / link.                                |
| `GetContactQRLink`                   | your own contact-share link.                                |

### Privacy / safety

| whatsmeow                                  | Notes                                              |
| ------------------------------------------ | -------------------------------------------------- |
| `GetPrivacySettings` / `SetPrivacySetting` | profile / last-seen / read-receipts visibility.    |
| `GetBlocklist` / `UpdateBlocklist`         | block / unblock.                                   |
| `GetStatusPrivacy`                         | who can see your status.                           |
| `TryFetchPrivacySettings`                  | (variant; usually paired with the getter).         |

### Profile / status

| whatsmeow            | Notes                |
| -------------------- | -------------------- |
| `SetStatusMessage`   | your "About" string. |

### Sync / history (orchestration)

| whatsmeow                                          | Notes                                                                  |
| -------------------------------------------------- | ---------------------------------------------------------------------- |
| `BuildHistorySyncRequest` + `SendPeerMessage`      | peer-driven backfill from your phone — useful for extending known-chat history backward. Whatsmeow can't bootstrap an empty cache via this path. |
| `FetchAppState`                                    | full re-sync of app state (chats list, contacts, settings, mute, archive, pin). Could power a "reconcile cache" tool.   |
| `SendAppState`                                     | advanced; mirror local state to the WhatsApp server (e.g. mark a chat read across devices). |

### Lifecycle / connection (admin-side)

| whatsmeow                                       | Notes                                                                          |
| ----------------------------------------------- | ------------------------------------------------------------------------------ |
| `Connect` / `ConnectContext` / `Disconnect`     | manual reconnect control. Indirectly driven by pairing today.                  |
| `IsConnected` / `IsLoggedIn`                    | low-level state probes (we expose `Status` instead, which is richer).          |
| `Logout`                                        | exposed via `POST /admin/unpair`, no MCP tool.                                 |
| `ResetConnection`                               | force-reset the websocket.                                                     |
| `WaitForConnection`                             | block until connected.                                                         |

### Calls

| whatsmeow                                                 | Notes                       |
| --------------------------------------------------------- | --------------------------- |
| `RejectCall`                                              | reject incoming WA call.    |

### Bots / system / acks

| whatsmeow                                                 | Notes                                       |
| --------------------------------------------------------- | ------------------------------------------- |
| `GetBotListV2`, `GetBotProfiles`                          | discover bots.                              |
| `AcceptTOSNotice`                                         | rare; surfaces during ToS rollouts.         |
| `MarkNotDirty`                                            | clear the "dirty" flag for app-state types. |
| `RegisterForPushNotifications`, `GetServerPushNotificationConfig`, `SetPassive` | push-notification config; not relevant to a server-side companion. |
| `SendMediaRetryReceipt`                                   | media decryption retry handshake.           |
