#!/usr/bin/env python3
"""PoC: Yandex Telemost VideoChannel via aiortc (WB-style flow)."""

import asyncio
import base64
import hashlib
import json
import uuid
import zlib
from fractions import Fraction
from urllib.parse import quote

import cv2
import numpy as np
import qrcode
import requests
import websockets
from aiortc import (
    RTCConfiguration,
    RTCIceCandidate,
    RTCIceServer,
    RTCPeerConnection,
    RTCSessionDescription,
)
from aiortc.mediastreams import MediaStreamTrack
from av import VideoFrame
from pyzbar.pyzbar import decode as qr_decode

CONFERENCE_ID = "75047680642749"
CONFERENCE_URL = f"https://telemost.yandex.ru/j/{CONFERENCE_ID}"
API_BASE = "https://cloud-api.yandex.ru/telemost_front/v2/telemost"
FPS = 10
TEST_MESSAGES = [
    "Hello Telemost via Video!",
    "Packed JSON payload test.",
    "X" * 200,
    "Final VideoChannel test",
]


def _uid() -> str:
    return str(uuid.uuid4())


def _encode(text: str) -> np.ndarray:
    payload = base64.b64encode(zlib.compress(text.encode())).decode()
    qr = qrcode.QRCode(error_correction=qrcode.constants.ERROR_CORRECT_L, box_size=8, border=3)
    qr.add_data(payload)
    qr.make(fit=True)
    arr = np.array(qr.make_image(fill_color="black", back_color="white").convert("RGB"), dtype=np.uint8)
    h, w = arr.shape[:2]
    return arr if (h % 2 == 0 and w % 2 == 0) else cv2.resize(arr, (w + w % 2, h + h % 2))


def _decode(frame: VideoFrame) -> str | None:
    arr = frame.to_ndarray(format="rgb24")
    gray = cv2.cvtColor(arr, cv2.COLOR_RGB2GRAY)
    for img in [gray, cv2.resize(gray, (gray.shape[1] * 2, gray.shape[0] * 2), interpolation=cv2.INTER_CUBIC)]:
        for obj in qr_decode(img):
            try:
                return zlib.decompress(base64.b64decode(obj.data)).decode()
            except Exception:
                pass
    return None


def _get_conn_info(display_name: str) -> dict:
    url = f"{API_BASE}/conferences/{quote(CONFERENCE_URL, safe='')}/connection"
    params = {
        "next_gen_media_platform_allowed": "true",
        "display_name": display_name,
        "waiting_room_supported": "true",
    }
    headers = {
        "User-Agent": "Mozilla/5.0 (X11; Linux x86_64; rv:149.0) Gecko/20100101 Firefox/149.0",
        "Accept": "*/*",
        "content-type": "application/json",
        "Client-Instance-Id": _uid(),
        "X-Telemost-Client-Version": "187.1.0",
        "idempotency-key": _uid(),
        "Origin": "https://telemost.yandex.ru",
        "Referer": "https://telemost.yandex.ru/",
    }
    resp = requests.get(url, params=params, headers=headers)
    resp.raise_for_status()
    return resp.json()


def _make_ice_servers(raw_list: list[dict]) -> list[RTCIceServer]:
    result = []
    for server in raw_list:
        urls = server.get("urls", [])
        cred = server.get("credential", "")
        user = server.get("username", "")
        if cred:
            result.append(RTCIceServer(urls=urls, credential=cred, username=user))
        else:
            result.append(RTCIceServer(urls=urls))
    return result or [RTCIceServer(urls=["stun:stun.rtc.yandex.net:3478"])]


class _VideoChannelTrack(MediaStreamTrack):
    kind = "video"

    def __init__(self):
        super().__init__()
        self._frame = _encode("IDLE")
        self._pts = 0

    def set_frame(self, arr: np.ndarray) -> None:
        self._frame = arr

    async def recv(self) -> VideoFrame:
        await asyncio.sleep(1.0 / FPS)
        frame = VideoFrame.from_ndarray(self._frame, format="rgb24")
        frame.pts = self._pts
        frame.time_base = Fraction(1, FPS)
        self._pts += 1
        return frame


async def _connect_peer(name: str, conn: dict, is_sender: bool, on_video_message=None) -> dict:
    default_ice = [RTCIceServer(urls=["stun:stun.rtc.yandex.net:3478"])]
    track = _VideoChannelTrack() if is_sender else None

    pc_sub_ref = [RTCPeerConnection(RTCConfiguration(iceServers=default_ice))]
    pc_pub_ref = [RTCPeerConnection(RTCConfiguration(iceServers=default_ice))]
    if is_sender:
        pc_pub_ref[0].addTrack(track)

    ws = await websockets.connect(
        conn["client_configuration"]["media_server_url"],
        additional_headers={
            "Origin": "https://telemost.yandex.ru",
            "User-Agent": "Mozilla/5.0 (X11; Linux x86_64; rv:149.0) Gecko/20100101 Firefox/149.0",
        },
        ping_interval=None,
    )

    subscriber_connected = asyncio.Event()
    publisher_connected = asyncio.Event()

    async def _send(obj: dict) -> None:
        await ws.send(json.dumps(obj))

    async def _ack(uid: str) -> None:
        await _send({"uid": uid, "ack": {"status": {"code": "OK", "description": ""}}})

    def _setup(pc_sub, pc_pub) -> None:
        @pc_sub.on("track")
        def on_track(remote_track):
            if remote_track.kind != "video" or on_video_message is None:
                return

            async def _loop():
                last_hash = None
                while True:
                    try:
                        frame = await asyncio.wait_for(remote_track.recv(), timeout=30.0)
                        frame_hash = hashlib.md5(frame.to_ndarray(format="rgb24").tobytes()).hexdigest()
                        if frame_hash == last_hash:
                            continue
                        last_hash = frame_hash
                        msg = _decode(frame)
                        if msg:
                            on_video_message(msg)
                    except Exception:
                        return

            asyncio.create_task(_loop())

        @pc_sub.on("connectionstatechange")
        async def on_sub_state():
            if pc_sub.connectionState == "connected":
                subscriber_connected.set()

        @pc_pub.on("connectionstatechange")
        async def on_pub_state():
            if pc_pub.connectionState == "connected":
                publisher_connected.set()

        @pc_sub.on("icecandidate")
        async def on_sub_ice(event):
            if event.candidate:
                await _send({
                    "uid": _uid(),
                    "webrtcIceCandidate": {
                        "candidate": event.candidate.candidate,
                        "sdpMid": event.candidate.sdpMid,
                        "sdpMlineIndex": event.candidate.sdpMLineIndex,
                        "usernameFragment": "",
                        "target": "SUBSCRIBER",
                        "pcSeq": 1,
                    },
                })

        @pc_pub.on("icecandidate")
        async def on_pub_ice(event):
            if event.candidate:
                await _send({
                    "uid": _uid(),
                    "webrtcIceCandidate": {
                        "candidate": event.candidate.candidate,
                        "sdpMid": event.candidate.sdpMid,
                        "sdpMlineIndex": event.candidate.sdpMLineIndex,
                        "usernameFragment": "",
                        "target": "PUBLISHER",
                        "pcSeq": 1,
                    },
                })

    _setup(pc_sub_ref[0], pc_pub_ref[0])

    await _send({
        "uid": _uid(),
        "hello": {
            "participantMeta": {
                "name": name,
                "role": "SPEAKER",
                "description": "",
                "sendAudio": False,
                "sendVideo": is_sender,
            },
            "participantAttributes": {
                "name": name,
                "role": "SPEAKER",
                "description": "",
            },
            "sendAudio": False,
            "sendVideo": is_sender,
            "sendSharing": False,
            "participantId": conn["peer_id"],
            "roomId": conn["room_id"],
            "serviceName": "telemost",
            "credentials": conn["credentials"],
            "capabilitiesOffer": {
                "offerAnswerMode": ["SEPARATE"],
                "initialSubscriberOffer": ["ON_HELLO"],
                "slotsMode": ["FROM_CONTROLLER"],
                "simulcastMode": ["DISABLED", "STATIC"],
                "selfVadStatus": ["FROM_SERVER", "FROM_CLIENT"],
                "dataChannelSharing": ["TO_RTP"],
                "videoEncoderConfig": ["NO_CONFIG", "ONLY_INIT_CONFIG", "RUNTIME_CONFIG"],
                "dataChannelVideoCodec": ["VP8", "UNIQUE_CODEC_FROM_TRACK_DESCRIPTION"],
                "bandwidthLimitationReason": ["BANDWIDTH_REASON_DISABLED", "BANDWIDTH_REASON_ENABLED"],
                "sdkDefaultDeviceManagement": ["SDK_DEFAULT_DEVICE_MANAGEMENT_DISABLED", "SDK_DEFAULT_DEVICE_MANAGEMENT_ENABLED"],
                "joinOrderLayout": ["JOIN_ORDER_LAYOUT_DISABLED", "JOIN_ORDER_LAYOUT_ENABLED"],
                "pinLayout": ["PIN_LAYOUT_DISABLED"],
                "sendSelfViewVideoSlot": ["SEND_SELF_VIEW_VIDEO_SLOT_DISABLED", "SEND_SELF_VIEW_VIDEO_SLOT_ENABLED"],
                "serverLayoutTransition": ["SERVER_LAYOUT_TRANSITION_DISABLED"],
                "sdkPublisherOptimizeBitrate": [
                    "SDK_PUBLISHER_OPTIMIZE_BITRATE_DISABLED",
                    "SDK_PUBLISHER_OPTIMIZE_BITRATE_FULL",
                    "SDK_PUBLISHER_OPTIMIZE_BITRATE_ONLY_SELF",
                ],
                "sdkNetworkLostDetection": ["SDK_NETWORK_LOST_DETECTION_DISABLED"],
                "sdkNetworkPathMonitor": ["SDK_NETWORK_PATH_MONITOR_DISABLED"],
                "publisherVp9": ["PUBLISH_VP9_DISABLED", "PUBLISH_VP9_ENABLED"],
                "svcMode": ["SVC_MODE_DISABLED", "SVC_MODE_L3T3", "SVC_MODE_L3T3_KEY"],
                "subscriberOfferAsyncAck": ["SUBSCRIBER_OFFER_ASYNC_ACK_DISABLED", "SUBSCRIBER_OFFER_ASYNC_ACK_ENABLED"],
                "androidBluetoothRoutingFix": ["ANDROID_BLUETOOTH_ROUTING_FIX_DISABLED"],
                "fixedIceCandidatesPoolSize": ["FIXED_ICE_CANDIDATES_POOL_SIZE_DISABLED"],
                "sdkAndroidTelecomIntegration": ["SDK_ANDROID_TELECOM_INTEGRATION_DISABLED"],
                "setActiveCodecsMode": ["SET_ACTIVE_CODECS_MODE_DISABLED", "SET_ACTIVE_CODECS_MODE_VIDEO_ONLY"],
                "subscriberDtlsPassiveMode": ["SUBSCRIBER_DTLS_PASSIVE_MODE_DISABLED"],
                "publisherOpusDred": ["PUBLISHER_OPUS_DRED_DISABLED"],
                "publisherOpusLowBitrate": ["PUBLISHER_OPUS_LOW_BITRATE_DISABLED"],
                "sdkAndroidDestroySessionOnTaskRemoved": ["SDK_ANDROID_DESTROY_SESSION_ON_TASK_REMOVED_DISABLED"],
                "svcModes": ["FALSE"],
                "reportTelemetryModes": ["TRUE"],
                "keepDefaultDevicesModes": ["FALSE"],
            },
            "sdkInfo": {
                "implementation": "browser",
                "version": "5.27.0",
                "userAgent": "Mozilla/5.0 (X11; Linux x86_64; rv:149.0) Gecko/20100101 Firefox/149.0",
                "hwConcurrency": 24,
            },
            "sdkInitializationId": _uid(),
            "disablePublisher": not is_sender,
            "disableSubscriber": False,
            "disableSubscriberAudio": True,
        },
    })

    async def _ws_loop():
        pub_sdp_sent = False
        try:
            async for raw in ws:
                data = json.loads(raw)
                uid = data.get("uid", "")

                if "ack" in data:
                    continue

                if "serverHello" in data:
                    raw_ice = data["serverHello"].get("rtcConfiguration", {}).get("iceServers", [])
                    if raw_ice:
                        ice = _make_ice_servers(raw_ice)
                        old_sub = pc_sub_ref[0]
                        old_pub = pc_pub_ref[0]
                        pc_sub_ref[0] = RTCPeerConnection(RTCConfiguration(iceServers=ice))
                        pc_pub_ref[0] = RTCPeerConnection(RTCConfiguration(iceServers=ice))
                        if is_sender and track is not None:
                            pc_pub_ref[0].addTrack(track)
                        _setup(pc_sub_ref[0], pc_pub_ref[0])
                        await old_sub.close()
                        await old_pub.close()
                    await _ack(uid)
                    continue

                if "subscriberSdpOffer" in data:
                    sdp = data["subscriberSdpOffer"]
                    await pc_sub_ref[0].setRemoteDescription(
                        RTCSessionDescription(sdp=sdp["sdp"], type="offer")
                    )
                    answer = await pc_sub_ref[0].createAnswer()
                    await pc_sub_ref[0].setLocalDescription(answer)
                    await _send({
                        "uid": _uid(),
                        "subscriberSdpAnswer": {
                            "pcSeq": sdp["pcSeq"],
                            "sdp": pc_sub_ref[0].localDescription.sdp,
                        },
                    })
                    await _ack(uid)

                    if not is_sender:
                        await _send({
                            "uid": _uid(),
                            "setSlots": {
                                "slots": [{"width": 1280, "height": 720}, {"width": 640, "height": 360}],
                                "audioSlotsCount": 0,
                                "key": 1,
                                "shutdownAllVideo": None,
                                "withSelfView": False,
                                "selfViewVisibility": "ON_LOADING_THEN_SHOW",
                                "gridConfig": {},
                            },
                        })

                    if is_sender and not pub_sdp_sent:
                        await asyncio.sleep(0.3)
                        offer = await pc_pub_ref[0].createOffer()
                        await pc_pub_ref[0].setLocalDescription(offer)
                        tracks = [
                            {
                                "mid": t.mid,
                                "transceiverMid": t.mid,
                                "kind": t.sender.track.kind.upper(),
                                "priority": 0,
                                "label": "videochannel",
                                "codecs": {},
                                "groupId": 1,
                                "description": "",
                            }
                            for t in pc_pub_ref[0].getTransceivers()
                            if t.sender.track
                        ]
                        await _send({
                            "uid": _uid(),
                            "publisherSdpOffer": {
                                "pcSeq": 1,
                                "sdp": pc_pub_ref[0].localDescription.sdp,
                                "tracks": tracks,
                            },
                        })
                        pub_sdp_sent = True
                    continue

                if "publisherSdpAnswer" in data:
                    await pc_pub_ref[0].setRemoteDescription(
                        RTCSessionDescription(sdp=data["publisherSdpAnswer"]["sdp"], type="answer")
                    )
                    await _ack(uid)
                    continue

                if "webrtcIceCandidate" in data:
                    cand = data["webrtcIceCandidate"]
                    candidate_str = cand.get("candidate", "")
                    parts = candidate_str.split()
                    if len(parts) < 8:
                        continue
                    try:
                        tcp_type = parts[parts.index("tcptype") + 1] if "tcptype" in parts else None
                        ice = RTCIceCandidate(
                            component=int(parts[1]),
                            foundation=parts[0].replace("candidate:", ""),
                            ip=parts[4],
                            port=int(parts[5]),
                            priority=int(parts[3]),
                            protocol=parts[2].lower(),
                            type=parts[7],
                            tcpType=tcp_type,
                            sdpMid=cand.get("sdpMid", "0"),
                            sdpMLineIndex=cand.get("sdpMlineIndex", 0),
                        )
                        if cand.get("target") == "SUBSCRIBER":
                            await pc_sub_ref[0].addIceCandidate(ice)
                        else:
                            await pc_pub_ref[0].addIceCandidate(ice)
                    except Exception:
                        pass
                    continue

                if uid:
                    await _ack(uid)
        except Exception:
            pass

    return {
        "track": track,
        "subscriber_connected": subscriber_connected,
        "publisher_connected": publisher_connected,
        "ws_task": asyncio.create_task(_ws_loop()),
        "ws": ws,
        "pc_sub_ref": pc_sub_ref,
        "pc_pub_ref": pc_pub_ref,
    }


async def run_poc() -> dict:
    print("\n--- Yandex Telemost VideoChannel PoC ---")
    results = {"server_ok": False, "client_ok": False, "sent": 0, "recv": 0, "errors": []}
    recv_events = [asyncio.Event() for _ in TEST_MESSAGES]
    last_message = [None]

    print("[1/3] Connecting peers...")
    try:
        sender_conn = _get_conn_info("OlcRTC-Client")
        receiver_conn = _get_conn_info("OlcRTC-Server")

        def on_msg(msg: str) -> None:
            last_message[0] = msg
            for i, expected in enumerate(TEST_MESSAGES):
                if msg == expected and not recv_events[i].is_set():
                    results["recv"] += 1
                    print(f" -> Recv: {msg[:60]}")
                    recv_events[i].set()
                    break

        server = await _connect_peer("OlcRTC-Server", receiver_conn, is_sender=False, on_video_message=on_msg)
        client = await _connect_peer("OlcRTC-Client", sender_conn, is_sender=True)
        await asyncio.wait_for(server["subscriber_connected"].wait(), timeout=20.0)
        await asyncio.wait_for(client["publisher_connected"].wait(), timeout=20.0)
        results["server_ok"] = True
        results["client_ok"] = True
        print(" :P Peers connected")
    except Exception as err:
        results["errors"].append(str(err))
        return results

    print("\n[2/3] Publishing VideoChannel...")
    current_frame = [_encode("IDLE")]
    client["track"].set_frame(current_frame[0])

    async def _push_frames():
        while True:
            client["track"].set_frame(current_frame[0])
            await asyncio.sleep(1.0 / FPS)

    push_task = asyncio.create_task(_push_frames())

    print("\n[3/3] Sending messages...")
    await asyncio.sleep(2)

    for idx, msg in enumerate(TEST_MESSAGES):
        try:
            current_frame[0] = _encode(msg)
            results["sent"] += 1
            print(f" -> Sent: {msg[:60]}")
            try:
                await asyncio.wait_for(recv_events[idx].wait(), timeout=10.0)
            except asyncio.TimeoutError:
                if last_message[0] is not None:
                    results["errors"].append(
                        f"Timeout waiting for msg {idx + 1}, last recv={last_message[0][:30]!r}"
                    )
                else:
                    results["errors"].append(f"Timeout waiting for msg {idx + 1}")
        except Exception as err:
            results["errors"].append(f"Send {idx + 1} failed: {err}")

    push_task.cancel()
    for peer in (server, client):
        peer["ws_task"].cancel()
        try:
            await peer["ws"].close()
        except Exception:
            pass
        try:
            await peer["pc_sub_ref"][0].close()
            await peer["pc_pub_ref"][0].close()
        except Exception:
            pass
    return results


def print_results(res: dict):
    print("\n--- TEST RESULTS ---")
    print(f"Server: {':P' if res['server_ok'] else 'X'} / Client: {':P' if res['client_ok'] else 'X'}")
    print(f"Messages: Sent {res['sent']} / Recv {res['recv']}")
    for err in res.get("errors", []):
        print(f" Error: {err}")
    print(f"\n{':P SUCCESS' if res['sent'] and res['sent'] == res['recv'] else 'X FAILED'}\n")


if __name__ == "__main__":
    try:
        print_results(asyncio.run(run_poc()))
    except KeyboardInterrupt:
        pass
