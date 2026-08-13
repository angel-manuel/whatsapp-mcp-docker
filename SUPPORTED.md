# whatsmeow → MCP tool coverage

A snapshot of which `whatsmeow.Client` (and admin) capabilities are exposed
as MCP tools today, and which are not. The "Tool" column is the registered
MCP name; "—" means no tool. Keep this file in sync when adding new tools.

Last reviewed: 2026-08-13 against `go.mau.fi/whatsmeow v0.0.0-20260421083005-5b8886176ff7`
(the revision pinned in `go.mod`).

---

## Supported

### Cache-backed read tools (`internal/mcptools`)

These read from the local SQLite cache; whatsmeow itself isn't called.

| Capability                           | Tool                          |
| ------------------------------------ | ----------------------------- |
| Browse cached chats                  | `list_chats`                  |
| Browse conversations (JIDs merged)   | `list_conversations`          |
| Single chat lookup                   | `get_chat`                    |
| List messages in a chat              | `list_messages`               |
| Surrounding context for a message    | `get_message_context`         |
| Most recent interaction with someone | `get_last_interaction`        |
| Chats involving a contact            | `get_contact_chats`           |
| Direct chat by contact JID/number    | `get_direct_chat_by_contact`  |
| Full conversation merged across a contact's phone JID + LID | `get_conversation` |

### whatsmeow-backed tools (`internal/tools`)

| Capability                                   | Tool                  | whatsmeow / admin call                                        |
| -------------------------------------------- | --------------------- | ------------------------------------------------------------- |
| Health check                                 | `ping`                | (custom)                                                      |
| Cache diagnostic snapshot                    | `cache_sync_status`   | (custom; reads `Ingestor.LastEventAt` + table counts)         |
| Search cached contacts                       | `search_contacts`     | cache only                                                    |
| List all cached contacts                     | `list_all_contacts`   | cache only                                                    |
| Contact details (cache + live status / pic)  | `get_contact_details` | `GetUserInfo`, `IsOnWhatsApp`, `GetProfilePictureInfo` (URL field only) |
| Any recipient → readable identity            | `resolve_jid`         | cache only (`GetGroupInfo` only for an unnamed group)         |
| Authoritative group metadata                 | `get_group_info`      | `GetGroupInfo`                                                |
| Send text message                            | `send_message`        | `SendMessage` (text only)                                     |
| Send an image / video / audio / document / sticker | `send_file`     | `UploadReader` (`Upload` when audio is transcoded) + `SendMessage` (`ImageMessage`, `VideoMessage`, `AudioMessage`, `DocumentMessage`, `StickerMessage`) |
| Send a voice note (PTT)                      | `send_audio_message`  | `UploadReader` / `Upload` + `SendMessage` (`AudioMessage` with `PTT`); ffmpeg transcode to Opus when `FFMPEG_PATH` resolves |
| React to a message with an emoji             | `send_reaction`       | `BuildReaction` + `SendMessage`                               |
| Create a poll                                | `send_poll`           | `BuildPollCreation` + `SendMessage` (+ `PutMessageSecret`, so votes on our own poll stay readable) |
| Vote on / withdraw from a poll               | `vote_poll`           | `BuildPollVote` + `SendMessage`                               |
| Read a poll's tally                          | `get_poll_results`    | cache only (votes accumulated from `DecryptPollVote`)         |
| Download a message attachment                | `download_media`      | `DownloadMediaWithPath`, `Download` (URL fallback)            |
| Set the account's "About" text               | `set_status_message`  | `SetStatusMessage`                                            |
| Publish global online / offline presence     | `send_presence`       | `SendPresence`                                                |
| Per-chat typing / recording indicator        | `send_chat_presence`  | `SendChatPresence`                                            |
| Subscribe to a user's presence               | `subscribe_presence`  | `SubscribePresence` (the resulting `*events.Presence` are ingested onto the contact row and read back via `get_contact_details`) |
| Per-chat disappearing-message timer          | `set_disappearing_timer` | `SetDisappearingTimer` (`off` / `24h` / `7d` / `90d` only) |
| Account-wide default disappearing timer      | `set_default_disappearing_timer` | `SetDefaultDisappearingTimer` (`off` / `24h` / `7d` / `90d` only) |
| Send read receipts                           | `mark_read`           | `MarkRead` (also clears the chat's cached unread flag)        |
| Start a pair flow                            | `pairing_start`       | `StartPairing`, `PairPhone`                                   |
| Poll an in-progress pair flow                | `pairing_complete`    | `PairWaitNext` / `PairLatest`                                 |
| Reconcile the cache against the server       | `cache_sync`          | `GetJoinedGroups`, `GetSubscribedNewsletters`, `FetchAppState` |

`set_status_message`, `send_presence`, `send_chat_presence`,
`subscribe_presence`, `set_disappearing_timer`,
`set_default_disappearing_timer` and `mark_read` mutate state other WhatsApp
users can see (or, for `subscribe_presence`, register server-side activity
under this account). Each says so in its tool description and validates
strictly before calling whatsmeow: the presence and chat-state enums, the
139-character `About` limit, and the four disappearing-message timers
WhatsApp actually honours (`off`, `24h`, `7d`, `90d` — whatsmeow itself will
forward any duration, which official clients then ignore and the server
rejects in groups).

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
| `*events.Presence`          | contact presence columns (migration `007`)     |
| `*events.Message` (poll creation) | `poll` message row + `polls` / `poll_options` ballot (migration `006`) |
| `*events.Message` (poll update)   | decrypted via `DecryptPollVote` into `poll_votes` (migration `006`); no message row |

Poll tallies are accumulated here and nowhere else. Neither whatsmeow nor the
WhatsApp protocol offers a way to ask the server for a poll's current
standings: votes exist only as the `PollUpdateMessage` events above, and they
are never replayed. `get_poll_results` therefore reports what this device
observed — votes cast before pairing, or while the container was down, are
gone. A vote whose poll secret whatsmeow never held cannot be decrypted at
all; it is logged and dropped, since nothing about it is recoverable later.

A `*events.Message` carrying a `ReactionMessage` is persisted to the `reactions`
table (migration `005`) rather than as a message row, keyed by
`(chat_jid, target_id, sender_jid)` — WhatsApp allows one reaction per person
per message, a new emoji replaces it, and an empty emoji removes it. Reactions
deliberately do not bump `chats.last_message_ts`. `*events.HistorySync` also
backfills the reactions attached to each synced message. Our own reactions are
stored under a canonical empty sender so the live, history-sync, and
`send_reaction` paths cannot produce duplicate rows.

Media envelopes additionally persist `media_direct_path` (migration `004`),
which is what `download_media` needs to re-request the CDN object after the
pre-signed `media_url` expires. It is captured at ingest and cannot be
backfilled — rows older than that migration must be re-ingested via
`cache_sync` before their media can be downloaded.

`*events.Presence` only arrives for JIDs `subscribe_presence` asked for, and
only while this device is itself marked available, so the presence columns
added by migration `007` stay at their defaults for every other contact.
`get_contact_details` reports that as `presence_observed: false` rather than
as a contact who is offline.

### HTTP routes (not MCP)

Mounted on the same listener and behind the same bearer auth as `/mcp`. This
is not a general-purpose API: the only non-MCP routes are the one thing MCP
structurally cannot do, which is transfer bytes — out and in. The former `internal/admin`
package and its `:8082` surface were removed in `99b0ce7`; pairing and health
are MCP tools (`pairing_start`, `pairing_complete`, `ping`).

| Endpoint               | Backed by                                        |
| ---------------------- | ------------------------------------------------ |
| `GET /media/{sha256}`  | `media.Store` — serves blobs stored by `download_media`; `Range`, `ETag`, `Content-Disposition` |
| `POST /media` (also `PUT`) | `media.Store` — stores the request body content-addressed and answers `201` with the same descriptor shape; the `media_path` it returns is what `send_file` / `send_audio_message` take. `Content-Type` sets the mimetype (sniffed from the bytes when absent *or* `application/octet-stream`), `?filename=` names the file. `400` for an empty body, `413` over `MEDIA_MAX_UPLOAD_BYTES` (default 100 MiB) |

---

## Not yet supported

The list below is exhaustive over `whatsmeow.Client` exported methods that
take user-meaningful action (excluding internal protocol helpers, build-only
helpers, decrypt/encrypt, retry plumbing, network/proxy setters). ⭐ marks
the highest user-value gaps. A ✅ row is a method the code **already calls** —
it stays listed here only because the capability is partially exposed (no
dedicated tool, or only part of the response surfaced). Everything unmarked
is genuinely uncalled.

### Outbound messaging (beyond plain text)

| whatsmeow                                           | Notes                                                              |
| --------------------------------------------------- | ------------------------------------------------------------------ |
| ⭐ `RevokeMessage` (`BuildRevoke` + send)           | delete-for-everyone.                                               |
| ⭐ `BuildEdit` + `SendMessage`                      | edit a previously sent message.                                    |

### Newsletter / channel management

| whatsmeow                                | Notes                                                                  |
| ---------------------------------------- | ---------------------------------------------------------------------- |
| ⭐ `FollowNewsletter`                    | subscribe to a channel by JID.                                         |
| ⭐ `UnfollowNewsletter`                  | unsubscribe.                                                           |
| `GetSubscribedNewsletters`               | ✅ already called by `cache_sync` (`internal/cache/sync.go`) to reconcile cached newsletter chats. No tool returns the raw list. |
| `GetNewsletterInfo`                      | metadata for a known JID.                                              |
| `GetNewsletterInfoWithInvite`            | metadata via an invite link.                                           |
| `GetNewsletterMessages`                  | fetch messages directly from the channel feed.                         |
| `GetNewsletterMessageUpdates`            | poll for updates.                                                      |
| `NewsletterMarkViewed`                   | mark a newsletter message as viewed.                                   |
| `NewsletterSendReaction`                 | react to a newsletter message. `send_reaction` rejects `@newsletter` targets: this needs a `MessageServerID` the cache does not capture. |
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
| `GetJoinedGroups`                                      | ✅ already called by `cache_sync` (`internal/cache/sync.go`) to reconcile cached group / community chats. No tool returns the raw list. |
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
| `DownloadThumbnail`                                                        | link-preview thumbnails; `JPEGThumbnail` is not captured at ingest today, and outbound envelopes from `send_file` do not carry one either (recipients render a placeholder until the bytes arrive). |
| `DownloadHistorySync`                                                      | large blob retrieval.                                          |
| `DownloadToFile` / `DownloadMediaWithPathToFile`                           | streaming-to-disk variants. `download_media` buffers in memory and writes content-addressed. |
| `DownloadFB` / `DownloadFBToFile`                                          | Facebook CDN variant.                                          |
| `DeleteMedia`                                                              | server-side delete.                                            |

### Identity & contacts (partial today)

| whatsmeow                            | Notes                                                       |
| ------------------------------------ | ----------------------------------------------------------- |
| `GetBusinessProfile`                 | richer business metadata than `get_contact_details`.        |
| `GetUserDevices` / `GetUserDevicesContext` | list paired devices for a JID.                        |
| `GetProfilePictureInfo` — full metadata | ✅ the call itself already backs `get_contact_details` (`internal/wa/lookups.go`), but only the `URL` field is surfaced; ID, type, and direct path are dropped. |
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

### Sync / history (orchestration)

| whatsmeow                                          | Notes                                                                  |
| -------------------------------------------------- | ---------------------------------------------------------------------- |
| `BuildHistorySyncRequest` + `SendPeerMessage`      | peer-driven backfill from your phone — useful for extending known-chat history backward. Whatsmeow can't bootstrap an empty cache via this path. |
| `FetchAppState`                                    | ✅ already called by `cache_sync`'s `app_state` stage (`internal/cache/sync.go`) for the regular / regular-low / regular-high / critical-unblock-low patches. The resulting events are ingested; the `CriticalBlock` patch is skipped (blocklist is not cached). |
| `SendAppState`                                     | advanced; mirror local state to the WhatsApp server (e.g. mark a chat read across devices). |

### Lifecycle / connection

| whatsmeow                                       | Notes                                                                          |
| ----------------------------------------------- | ------------------------------------------------------------------------------ |
| `Connect` / `ConnectContext` / `Disconnect`     | manual reconnect control. Indirectly driven by pairing today.                  |
| `IsConnected` / `IsLoggedIn`                    | low-level state probes (we expose `Status` instead, which is richer).          |
| `Logout`                                        | no tool. The `POST /admin/unpair` endpoint that used to expose it was removed in `99b0ce7`. Not dead code: it is still called internally as the best-effort server-side logout when re-pairing tears down an old session (`internal/wa/admin_ops.go`). |
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
