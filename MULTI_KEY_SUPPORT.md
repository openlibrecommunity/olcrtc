# Multiple Key Support

## Overview

The server now supports multiple encryption keys for backward compatibility and key rotation scenarios. This allows clients with different keys to authenticate with the same server.

## Configuration

### Option 1: Using `crypto.keys` (preferred)

In your `olcrtc.yaml` config file, use the `keys` field with a list of hex-encoded keys:

```yaml
mode: srv
auth:
  provider: jitsi
room:
  id: my-room
crypto:
  keys:
    - "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
    - "ffeeddccbbaa99887766554433221100ffeeddccbbaa99887766554433221100"
net:
  transport: datachannel
  dns: 8.8.8.8:53
```

Keys are tried in order during decryption. The first key is used for encryption.

### Option 2: Using `crypto.keys_file`

Store keys in a separate file (one key per line):

```yaml
crypto:
  keys_file: keys.txt
```

Content of `keys.txt`:
```
00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff
ffeeddccbbaa99887766554433221100ffeeddccbbaa99887766554433221100
# comments are supported
```

Empty lines and lines starting with `#` are ignored.

### Option 3: Backward Compatibility (single key)

The old format still works:

```yaml
crypto:
  key: "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
```

Or from file:
```yaml
crypto:
  key_file: key.txt
```

## Key Rotation Example

To rotate keys:

1. Keep the old key as the first entry
2. Add the new key(s) after it
3. Clients can use any of the keys to authenticate
4. After all clients are updated, remove the old key from the list

```yaml
crypto:
  keys:
    - "old_key_hex_string"      # clients with old key still work
    - "new_key_hex_string"      # new clients use this
```

## Implementation Details

- On the server: Multiple keys are tried during decryption (first match wins)
- On the client: Uses the single configured key for encryption and decryption
- The first key in the list is used for server-to-client encryption
- Backward compatible with existing single-key configs

## Generating Keys

Generate a new 32-byte key (64 hex characters):

```bash
openssl rand -hex 32
```

## Error Handling

If no valid key is configured:
- Config loading fails with `ErrKeyRequired`

If decryption fails with all keys:
- The frame is logged and dropped (normal operation)
