#!/usr/bin/env python3
"""PoC: WB Stream VideoChannel via LiveKit (QR + Zlib)."""

import asyncio
import base64
import hashlib
import logging
import requests
import zlib
import cv2
import numpy as np
import qrcode
import requests
from livekit import rtc
from pyzbar.pyzbar import decode as qr_decode

logging.getLogger("livekit").setLevel(logging.WARNING)

API_BASE = "https://stream.wb.ru"
WS_URL = "wss://wbstream01-el.wb.ru:7880"
FPS = 10
TEST_MESSAGES = ["Hello WB Stream via Video!", "Packed JSON payload test.", "X" * 200, "Final VideoChannel test"]


def _encode(text: str) -> np.ndarray:
    payload = base64.b64encode(zlib.compress(text.encode())).decode()
    qr = qrcode.QRCode(error_correction=qrcode.constants.ERROR_CORRECT_L, box_size=8, border=3)
    qr.add_data(payload)
    qr.make(fit=True)
    arr = np.array(qr.make_image(fill_color="black", back_color="white").convert("RGBA"), dtype=np.uint8)
    h, w = arr.shape[:2]
    return arr if (h % 2 == 0 and w % 2 == 0) else cv2.resize(arr, (w + w % 2, h + h % 2))


def _decode(arr: np.ndarray) -> str | None:
    gray = cv2.cvtColor(arr, cv2.COLOR_RGBA2GRAY)
    for img in [gray, cv2.resize(gray, (gray.shape[1] * 2, gray.shape[0] * 2), interpolation=cv2.INTER_CUBIC)]:
        for obj in qr_decode(img):
            try:
                return zlib.decompress(base64.b64decode(obj.data)).decode()
            except Exception:
                pass
    return None


def _get_room_token(room_id: str, display_name: str) -> tuple[str, str]:
    headers = {"User-Agent": "Mozilla/5.0 (Linux x86_64)", "Content-Type": "application/json"}
    reg = requests.post(f"{API_BASE}/auth/api/v1/auth/user/guest-register",
                        json={"displayName": display_name, "device": {"deviceName": "Linux", "deviceType": "PARTICIPANT_DEVICE_TYPE_WEB_DESKTOP"}},
                        headers=headers)
    reg.raise_for_status()
    headers["Authorization"] = f"Bearer {reg.json()['accessToken']}"
    if not room_id:
        r = requests.post(f"{API_BASE}/api-room/api/v2/room",
                          json={"roomType": "ROOM_TYPE_ALL_ON_SCREEN", "roomPrivacy": "ROOM_PRIVACY_FREE"}, headers=headers)
        r.raise_for_status()
        room_id = r.json()["roomId"]
    requests.post(f"{API_BASE}/api-room/api/v1/room/{room_id}/join", json={}, headers=headers).raise_for_status()
    tok = requests.get(f"{API_BASE}/api-room-manager/api/v1/room/{room_id}/token",
                       params={"deviceType": "PARTICIPANT_DEVICE_TYPE_WEB_DESKTOP", "displayName": display_name}, headers=headers)
    tok.raise_for_status()
    return room_id, tok.json()["roomToken"]


async def run_poc() -> dict:
    print("\n--- WB Stream VideoChannel PoC ---")
    results = {"server_ok": False, "client_ok": False, "sent": 0, "recv": 0, "errors": []}
    recv_events: list[asyncio.Event] = [asyncio.Event() for _ in TEST_MESSAGES]
    last_hash = None

    server, client = rtc.Room(), rtc.Room()

    print("[1/3] Connecting peers...")
    try:
        shared_room_id, server_tok = _get_room_token("", "OlcRTC-Server")
        _, client_tok = _get_room_token(shared_room_id, "OlcRTC-Client")

        async def process_video_stream(stream: rtc.VideoStream):
            nonlocal last_hash
            async for event in stream:
                arr = np.frombuffer(event.frame.convert(rtc.VideoBufferType.RGBA).data, dtype=np.uint8)
                arr = arr.reshape((event.frame.height, event.frame.width, 4))
                frame_hash = hashlib.md5(arr.tobytes()).hexdigest()
                if frame_hash == last_hash:
                    continue
                last_hash = frame_hash
                msg = _decode(arr)
                if msg:
                    for i, expected in enumerate(TEST_MESSAGES):
                        if msg == expected and not recv_events[i].is_set():
                            results["recv"] += 1
                            print(f" -> Recv: {msg[:60]}")
                            recv_events[i].set()
                            break

        @server.on("track_subscribed")
        def on_track(track: rtc.Track, pub: rtc.RemoteTrackPublication, participant: rtc.RemoteParticipant):
            if track.kind == rtc.TrackKind.KIND_VIDEO:
                asyncio.ensure_future(process_video_stream(rtc.VideoStream(track)))

        await server.connect(WS_URL, server_tok)
        results["server_ok"] = True
        await client.connect(WS_URL, client_tok)
        results["client_ok"] = True
        print(f" :P Connected to room: {shared_room_id}")
    except Exception as e:
        results["errors"].append(str(e))
        return results

    print("\n[2/3] Publishing VideoChannel...")
    source = rtc.VideoSource(width=320, height=320)
    track = rtc.LocalVideoTrack.create_video_track("videochannel", source)
    opts = rtc.TrackPublishOptions()
    opts.source = rtc.TrackSource.SOURCE_CAMERA
    await client.local_participant.publish_track(track, opts)

    current_frame = [None]

    async def _push_frames():
        while True:
            if current_frame[0] is not None:
                arr = current_frame[0]
                source.capture_frame(rtc.VideoFrame(arr.shape[1], arr.shape[0], rtc.VideoBufferType.RGBA, arr.tobytes()))
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
                results["errors"].append(f"Timeout waiting for msg {idx + 1}")
        except Exception as e:
            results["errors"].append(f"Send {idx + 1} failed: {e}")

    push_task.cancel()
    await server.disconnect()
    await client.disconnect()
    return results


def print_results(res: dict):
    print("\n--- TEST RESULTS ---")
    print(f"Server: {':P' if res['server_ok'] else 'X'} / Client: {':P' if res['client_ok'] else 'X'}")
    print(f"Messages: Sent {res['sent']} / Recv {res['recv']}")
    for e in res.get("errors", []):
        print(f" Error: {e}")
    print(f"\n{':P SUCCESS' if res['sent'] and res['sent'] == res['recv'] else 'X FAILED'}\n")


if __name__ == "__main__":
    try:
        print_results(asyncio.run(run_poc()))
    except KeyboardInterrupt:
        pass
