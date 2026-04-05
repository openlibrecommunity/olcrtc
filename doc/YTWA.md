# Yandex Telemost WebRTC API Documentation - YTWA

## Overview

Yandex Telemost implements a Selective Forwarding Unit (SFU) architecture for WebRTC conferencing. The system supports audio, video, and SCTP DataChannel transport over WebRTC with separate publisher and subscriber peer connections.

## Architecture

### Connection Model

- SFU-based routing (not P2P)
- Separate PeerConnections for publishing and subscribing
- WebSocket signaling channel
- STUN/TURN infrastructure for NAT traversal

### Transport Capabilities

- Audio: Opus codec (48kHz, stereo, DTX, FEC)
- Video: VP8, VP9, H.264, AV1 codecs with simulcast support
- DataChannel: SCTP over DTLS (max message size: 1023MB, port: 5000)

## API Endpoints

### Base URL

```
https://cloud-api.yandex.ru/telemost_front/v2/telemost
```

### 1. Connection Initialization

**Endpoint:** `GET /conferences/{encoded_conference_url}/connection`

**Parameters:**
- `next_gen_media_platform_allowed`: boolean (string "true")
- `display_name`: string (URL-encoded participant name)
- `waiting_room_supported`: boolean (string "true")

**Headers:**
- `User-Agent`: Browser user agent string
- `Accept`: "*/*"
- `content-type`: "application/json"
- `Client-Instance-Id`: UUID v4
- `X-Telemost-Client-Version`: Version string (e.g., "187.1.0")
- `idempotency-key`: UUID v4
- `Origin`: "https://telemost.yandex.ru"
- `Referer`: "https://telemost.yandex.ru/"

**Response:**
```json
{
  "connection_type": "CONFERENCE",
  "uri": "https://telemost.yandex.ru/j/{conference_id}",
  "room_id": "uuid",
  "safe_room_id": "uuid",
  "peer_id": "uuid",
  "session_id": "uuid",
  "peer_session_id": "uuid",
  "credentials": "string",
  "expiration_time": 1775424866469,
  "conference_limit": 40,
  "media_platform": "GOLOOM",
  "client_configuration": {
    "media_server_url": "wss://goloom.strm.yandex.net/join",
    "service_name": "telemost",
    "ice_servers": [
      {
        "urls": ["stun:stun.rtc.yandex.net:3478"]
      }
    ],
    "goloom_session_open_ms": 120000,
    "wait_time_to_reconnect_ms": 3553
  }
}
```

## WebSocket Protocol

### Connection

**URL:** Obtained from `client_configuration.media_server_url`

**Protocol:** WSS (WebSocket Secure)

### Message Format

All messages are JSON objects with a `uid` field (UUID v4) and one message-specific field.

### Message Types

#### 1. Client Hello

**Direction:** Client → Server

**Structure:**
```json
{
  "uid": "uuid",
  "hello": {
    "participantMeta": {
      "name": "string",
      "role": "SPEAKER",
      "description": "string",
      "sendAudio": boolean,
      "sendVideo": boolean
    },
    "participantAttributes": {
      "name": "string",
      "role": "SPEAKER",
      "description": "string"
    },
    "sendAudio": boolean,
    "sendVideo": boolean,
    "sendSharing": boolean,
    "participantId": "uuid",
    "roomId": "uuid",
    "serviceName": "telemost",
    "credentials": "string",
    "capabilitiesOffer": {
      "offerAnswerMode": ["SEPARATE"],
      "initialSubscriberOffer": ["ON_HELLO"],
      "slotsMode": ["FROM_CONTROLLER"],
      "simulcastMode": ["DISABLED", "STATIC"],
      "selfVadStatus": ["FROM_SERVER", "FROM_CLIENT"],
      "dataChannelSharing": ["TO_RTP"]
    },
    "sdkInfo": {
      "implementation": "string",
      "version": "string",
      "userAgent": "string",
      "hwConcurrency": integer
    },
    "sdkInitializationId": "uuid",
    "disablePublisher": boolean,
    "disableSubscriber": boolean,
    "disableSubscriberAudio": boolean
  }
}
```

#### 2. Server Hello

**Direction:** Server → Client

**Structure:**
```json
{
  "uid": "uuid",
  "serverHello": {
    "capabilitiesAnswer": {
      "offerAnswerMode": "SEPARATE",
      "initialSubscriberOffer": "ON_HELLO",
      "slotsMode": "FROM_CONTROLLER",
      "simulcastMode": "DISABLED",
      "selfVadStatus": "FROM_SERVER",
      "dataChannelSharing": "TO_RTP",
      "videoEncoderConfig": "NO_CONFIG",
      "dataChannelVideoCodec": "UNIQUE_CODEC_FROM_TRACK_DESCRIPTION",
      "bandwidthLimitationReason": "BANDWIDTH_REASON_ENABLED",
      "publisherVp9": "PUBLISH_VP9_ENABLED",
      "svcMode": "SVC_MODE_L3T3_KEY"
    },
    "servingComponents": [
      {
        "type": "BORDER|WEBRTC_SERVER|CONTROLLER",
        "host": "string",
        "version": "string"
      }
    ],
    "sessionSecret": "uuid",
    "sfuPeerInitializationId": "uuid",
    "rtcConfiguration": {
      "iceServers": [
        {
          "urls": ["stun:turn.tel.yandex.net", "stun:stun.rtc.yandex.net"],
          "credential": "",
          "username": ""
        },
        {
          "urls": ["turn:turn.tel.yandex.net:443"],
          "credential": "string",
          "username": "string"
        }
      ]
    },
    "pingPongConfiguration": {
      "pingInterval": 5000,
      "ackTimeout": 9000
    },
    "telemetryConfiguration": {
      "sendingInterval": 20000
    }
  }
}
```

#### 3. Acknowledgment

**Direction:** Client → Server

**Structure:**
```json
{
  "uid": "uuid",
  "ack": {
    "status": {
      "code": "OK",
      "description": "string"
    }
  }
}
```

#### 4. Subscriber SDP Offer

**Direction:** Server → Client

**Structure:**
```json
{
  "uid": "uuid",
  "subscriberSdpOffer": {
    "pcSeq": integer,
    "sdp": "string"
  }
}
```

**SDP Format:** Standard WebRTC SDP with bundled media streams. Includes:
- Video tracks (m=video) with VP8, VP9, H.264, AV1 codecs
- Audio tracks (m=audio) with Opus, PCMA, PCMU, G722 codecs
- Application track (m=application) for SCTP DataChannel

#### 5. Subscriber SDP Answer

**Direction:** Client → Server

**Structure:**
```json
{
  "uid": "uuid",
  "subscriberSdpAnswer": {
    "pcSeq": integer,
    "sdp": "string"
  }
}
```

#### 6. Publisher SDP Offer

**Direction:** Client → Server

**Structure:**
```json
{
  "uid": "uuid",
  "publisherSdpOffer": {
    "pcSeq": integer,
    "sdp": "string"
  }
}
```

#### 7. Publisher SDP Answer

**Direction:** Server → Client

**Structure:**
```json
{
  "uid": "uuid",
  "publisherSdpAnswer": {
    "pcSeq": integer,
    "sdp": "string"
  }
}
```

#### 8. ICE Candidate

**Direction:** Bidirectional

**Structure:**
```json
{
  "uid": "uuid",
  "webrtcIceCandidate": {
    "candidate": "string",
    "sdpMid": "string",
    "sdpMlineIndex": integer,
    "usernameFragment": "string",
    "target": "PUBLISHER|SUBSCRIBER",
    "pcSeq": integer
  }
}
```

**Candidate Format:** Standard ICE candidate string
```
candidate:{foundation} {component} {protocol} {priority} {ip} {port} typ {type} [tcptype {tcptype}]
```

#### 9. VAD Activity

**Direction:** Server → Client

**Structure:**
```json
{
  "uid": "uuid",
  "vadActivity": {
    "active": boolean
  }
}
```

#### 10. Update Description

**Direction:** Server → Client

**Structure:**
```json
{
  "uid": "uuid",
  "updateDescription": {
    "description": []
  }
}
```

## Connection Flow

### 1. Initialization Phase

1. Client requests connection info via REST API
2. Server responds with room credentials and WebSocket URL
3. Client establishes WebSocket connection

### 2. Handshake Phase

1. Client sends `hello` message with capabilities
2. Server responds with `serverHello` containing negotiated capabilities
3. Client acknowledges with `ack` message

### 3. Subscriber Setup

1. Server sends `subscriberSdpOffer` with remote media tracks
2. Client creates answer and sends `subscriberSdpAnswer`
3. Client acknowledges offer
4. ICE candidates exchanged for subscriber PeerConnection

### 4. Publisher Setup

1. Client creates offer and sends `publisherSdpOffer`
2. Server responds with `publisherSdpAnswer`
3. Client acknowledges answer
4. ICE candidates exchanged for publisher PeerConnection

### 5. Media Exchange

1. DataChannel opens on publisher PeerConnection
2. Audio/video tracks activated based on configuration
3. Bidirectional media flow through SFU

## DataChannel Specifications

### Configuration

- **Protocol:** SCTP over DTLS
- **Port:** 5000
- **Max Message Size:** 1,073,741,823 bytes (1023 MB)
- **Ordered:** Configurable (recommended: true)
- **Label:** Custom (e.g., "olcrtc")

### SDP Attributes

```
m=application 9 UDP/DTLS/SCTP webrtc-datachannel
a=sctp-port:5000
a=max-message-size:1073741823
```

### Usage

DataChannel is created on the publisher PeerConnection and becomes available on the subscriber PeerConnection through the `ondatachannel` event.

## Audio Codec Configuration

### Opus

- **Sample Rate:** 48000 Hz
- **Channels:** 2 (stereo)
- **Parameters:**
  - `minptime=10`
  - `useinbandfec=1` (Forward Error Correction)
  - `usedtx=1` (Discontinuous Transmission)

### RED (Redundant Audio Data)

- **Payload Type:** 101
- **Format:** `111/111` (Opus redundancy)

## Video Codec Configuration

### Supported Codecs

1. **VP8**
   - Payload type: 96
   - RTX support: 97
   - NACK, PLI, REMB feedback

2. **VP9**
   - Payload type: 98
   - Profile: 0
   - RTX support: 99
   - NACK, PLI, REMB feedback

3. **H.264**
   - Multiple profiles supported:
     - Baseline (42001f, 42e01f)
     - Main (4d001f)
     - High (64001f)
   - Packetization modes: 0, 1
   - Level asymmetry allowed

4. **AV1**
   - Payload type: 45
   - RTX support: 46

### RTP Extensions

- `http://www.webrtc.org/experiments/rtp-hdrext/abs-send-time`

## ICE Configuration

### STUN Servers

- `stun:stun.rtc.yandex.net:3478`
- `stun:turn.tel.yandex.net`

### TURN Servers

- `turn:turn.tel.yandex.net:443`
- `turn:turn.tel.yandex.net:443?transport=tcp`

Credentials are time-limited and provided in `serverHello.rtcConfiguration.iceServers`.

## Error Handling

### Connection Errors

- Retry with exponential backoff
- Maximum wait time: `wait_time_to_reconnect_ms` (typically 3553ms)

### Session Timeout

- Session open timeout: `goloom_session_open_ms` (typically 120000ms)
- Expiration time provided in connection response

### Ping/Pong

- Ping interval: 5000ms
- ACK timeout: 9000ms
- Connection considered dead if no ACK received

## Security Considerations

### Authentication

- Credentials obtained from REST API
- Time-limited session tokens
- Peer ID and room ID validation

### Transport Security

- WSS (WebSocket Secure) for signaling
- DTLS for media transport
- SRTP for audio/video encryption

## Implementation Notes

### Guest Access

- No authentication required for participants
- Only conference initiator needs account
- Display name can be arbitrary

### Peer Limit

- Default conference limit: 40 participants
- Configurable per conference

### User Agent Spoofing

- Server may validate `User-Agent` and `sdkInfo`
- Recommended to use realistic browser signatures

### DataChannel Availability

- DataChannel support is not guaranteed
- Check SDP for `m=application` line before assuming availability
- Server may disable DataChannel without notice

## Rate Limits

Not explicitly documented. Recommended approach:
- Limit connection attempts per IP
- Implement exponential backoff on failures
- Respect session expiration times

## Bandwidth Configuration

### Video Layers

- **L1:** 1000 kbps (single layer)
- **L2:** 120 kbps (low), 360 kbps (med)
- **L3:** 120 kbps (low), 360 kbps (med), 800 kbps (hi)
- **L4:** 120 kbps (low), 360 kbps (med), 800 kbps (hi), 1000 kbps (ultra)

### Screen Sharing (4K)

- **Codec:** VP8
- **Min Bitrate:** 300 kbps
- **Max Bitrate:** 2000 kbps
- **Min Framerate:** 8 fps
- **Max Framerate:** 30 fps
- **Content Hint:** detail

## Telemetry

- Sending interval: 20000ms
- Endpoint: `logEndpoint` (if provided in serverHello)
- Format: Not documented

## Version Information

- **API Version:** v2
- **Client Version:** 187.1.0 (as of documentation date)
- **Media Platform:** GOLOOM
- **SDK Version:** Configurable in `sdkInfo`
