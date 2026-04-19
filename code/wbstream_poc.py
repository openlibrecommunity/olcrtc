#!/usr/bin/env python3
"""PoC: WB Stream DataChannel over LiveKit."""

import asyncio
import logging
import uuid
import requests

try:
    from livekit import rtc
except ImportError:
    print("[!] Error: livekit library not installed.\nRun: pip install livekit requests")
    exit(1)

logging.getLogger("livekit").setLevel(logging.WARNING)

API_BASE = "https://stream.wb.ru"
WS_URL = "wss://wbstream01-el.wb.ru:7880"
TEST_MESSAGES = ["Hello WB Stream!", "Hello world", "X" * 100, "Final test"]

def _get_room_token(room_id: str, display_name: str) -> tuple[str, str]:
    """Retrieves the room token via the guest API."""
    headers = {
        "User-Agent": "Mozilla/5.0 (Linux x86_64)",
        "Content-Type": "application/json"
    }
    
    # 1. Guest Registration
    reg_req = requests.post(
        f"{API_BASE}/auth/api/v1/auth/user/guest-register",
        json={"displayName": display_name, "device": {"deviceName": "Linux", "deviceType": "PARTICIPANT_DEVICE_TYPE_WEB_DESKTOP"}},
        headers=headers
    )
    reg_req.raise_for_status()
    headers["Authorization"] = f"Bearer {reg_req.json()['accessToken']}"
    
    # 2. Room Creation (if empty)
    if not room_id:
        room_req = requests.post(f"{API_BASE}/api-room/api/v2/room", json={"roomType": "ROOM_TYPE_ALL_ON_SCREEN", "roomPrivacy": "ROOM_PRIVACY_FREE"}, headers=headers)
        room_req.raise_for_status()
        room_id = room_req.json()["roomId"]
        
    # 3. Join & Token
    requests.post(f"{API_BASE}/api-room/api/v1/room/{room_id}/join", json={}, headers=headers).raise_for_status()
    tok_req = requests.get(f"{API_BASE}/api-room-manager/api/v1/room/{room_id}/token", params={"deviceType": "PARTICIPANT_DEVICE_TYPE_WEB_DESKTOP", "displayName": display_name}, headers=headers)
    tok_req.raise_for_status()
    return room_id, tok_req.json()["roomToken"]

async def run_poc() -> dict:
    """Runs the complete PoC flow."""
    print("\n--- WB Stream PoC ---")
    results = {"server_ok": False, "client_ok": False, "sent": 0, "recv": 0, "errors": []}
    
    server, client = rtc.Room(), rtc.Room()
    shared_room_id, _ = _get_room_token("", "OlcRTC-Server")

    print("[1/3] Connecting Server & Client...")
    try:
        shared_room_id, server_tok = _get_room_token("", "OlcRTC-Server")
        _, client_tok = _get_room_token(shared_room_id, "OlcRTC-Client")

        @server.on("data_received")
        def on_server_data(dp: rtc.DataPacket):
            if dp.topic == "olcrtc":
                asyncio.create_task(server.local_participant.publish_data(f"Echo: {dp.data.decode()}".encode(), topic="olcrtc"))

        @client.on("data_received")
        def on_client_data(dp: rtc.DataPacket):
            results["recv"] += 1

        await server.connect(WS_URL, server_tok)
        results["server_ok"] = True
        await client.connect(WS_URL, client_tok)
        results["client_ok"] = True
        print(f" :P Peers connected to room: {shared_room_id}")
    except Exception as e:
        results["errors"].append(str(e))
        return results

    print("\n[2/3] Exchanging messages...")
    await asyncio.sleep(1)
    
    for idx, msg in enumerate(TEST_MESSAGES, 1):
        try:
            await client.local_participant.publish_data(msg.encode(), topic="olcrtc")
            results["sent"] += 1
            print(f" -> Sent: {msg}")
            await asyncio.sleep(0.5)
        except Exception as e:
            results["errors"].append(f"Sending {idx} failed: {str(e)}")

    await asyncio.sleep(2)
    
    print("\n[3/3] Cleaning up...")
    await server.disconnect()
    await client.disconnect()
    
    return results

def print_results(res: dict):
    print("\n--- TEST RESULTS ---")
    print(f"Server: {':P' if res['server_ok'] else 'X'} / Client: {':P' if res['client_ok'] else 'X'}")
    print(f"Messages: Sent {res['sent']} / Recv {res['recv']}")
    if res['errors']:
        for e in res['errors']: print(f" Error: {e}")
    print(f"\n{':P SUCCESS' if res['sent'] and res['sent'] == res['recv'] else 'X FAILED'}\n")

if __name__ == "__main__":
    try:
        res = asyncio.run(run_poc())
        print_results(res)
    except KeyboardInterrupt:
        pass
