### 1. Authentication Setup

> **Note**: You need an `xoxp-*` User OAuth token, an `xoxb-*` Bot token, or both. If both are set, the bot token is used by default and the user token for tools that must act as you (channel management, DMs, message deletion, unreads). Browser session tokens (`xoxc`/`xoxd`) are not supported.

#### Option 1: Using `SLACK_MCP_XOXP_TOKEN` (User OAuth)

1. Go to [api.slack.com/apps](https://api.slack.com/apps) and create a new app
2. Under "OAuth & Permissions", add the following to the "User Token Scopes":
    - `channels:history` - View messages in public channels
    - `channels:read` - View basic information about public channels
    - `groups:history` - View messages in private channels
    - `groups:read` - View basic information about private channels
    - `im:history` - View messages in direct messages.
    - `im:read` - View basic information about direct messages
    - `im:write` - Start direct messages with people on a user’s behalf (new since `v1.1.18`)
    - `mpim:history` - View messages in group direct messages
    - `mpim:read` - View basic information about group direct messages
    - `mpim:write` - Start group direct messages with people on a user’s behalf (new since `v1.1.18`)
    - `users:read` - View people in a workspace.
    - `chat:write` - Send messages on a user's behalf. (new since `v1.1.18`)
    - `search:read` - Search a workspace's content. (new since `v1.1.18`)
    - `usergroups:read` - View user groups in a workspace.
    - `usergroups:write` - Create and manage user groups.
    - `channels:write` - Join and leave public channels.
    - `files:read` - View files and canvases shared in channels.
    - `files:write` - Upload and share files in conversations.

3. Install the app to your workspace
4. Copy the "User OAuth Token" (starts with `xoxp-`)

##### App manifest (preconfigured scopes)
To create the app from a manifest with permissions preconfigured, use the following code snippet:

```json
{
    "display_information": {
        "name": "Slack MCP"
    },
    "oauth_config": {
        "scopes": {
            "user": [
                "channels:history",
                "channels:read",
                "groups:history",
                "groups:read",
                "im:history",
                "im:read",
                "im:write",
                "mpim:history",
                "mpim:read",
                "mpim:write",
                "users:read",
                "chat:write",
                "search:read",
                "usergroups:read",
                "usergroups:write",
                "channels:write",
                "files:read",
                "files:write"
            ]
        }
    },
    "settings": {
        "org_deploy_enabled": false,
        "socket_mode_enabled": false,
        "token_rotation_enabled": false
    }
}
```

#### Option 2: Using `SLACK_MCP_XOXB_TOKEN` (Bot Token)

You can also use a Bot token instead of a User token:

1. Go to [api.slack.com/apps](https://api.slack.com/apps) and create a new app
2. Under "OAuth & Permissions", add Bot Token Scopes (same as User scopes above, except replace `search:read` with `search:read.public` and replace `channels:write` with `channels:join` + `channels:manage`)
3. Install the app to your workspace
4. Copy the "Bot User OAuth Token" (starts with `xoxb-`)
5. **Important**: Bot must be invited to channels for access

> **Note**: Bot tokens cannot use `search.messages` API, so `conversations_search_messages` tool will not be available.


See next: [Installation](02-installation.md)
